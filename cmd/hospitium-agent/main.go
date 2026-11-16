// SPDX-License-Identifier: Apache-2.0

// Command hospitium-agent keeps Google TVs at their desired state.
//
//	hospitium-agent enroll --server https://hospitium.example --token <enrollment-token>
//	hospitium-agent run
//	hospitium-agent run --local /etc/hospitium-agent/desired.json   (standalone, no control plane)
//	hospitium-agent check --tv 10.0.50.21:5555 [--launcher-apk ... --reboots 5 --clear com.netflix.ninja --yes]
//	hospitium-agent claim-code   (prints the code for the hub's label)
//	hospitium-agent version
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/agent"
	"github.com/spectrum-labs-tech/hospitium-agent/internal/claim"
	"github.com/spectrum-labs-tech/hospitium-agent/internal/client"
	"github.com/spectrum-labs-tech/hospitium-agent/internal/discover"
	"github.com/spectrum-labs-tech/hospitium-agent/internal/local"
	"github.com/spectrum-labs-tech/hospitium-agent/internal/state"
	"github.com/spectrum-labs-tech/hospitium-agent/internal/tv"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	// One root context: SIGINT/SIGTERM cancels it, and it flows into every
	// HTTP request, adb invocation and download.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "enroll":
		err = enroll(ctx, os.Args[2:])
	case "run":
		err = run(ctx, os.Args[2:])
	case "check":
		err = checkTV(ctx, os.Args[2:], os.Stdout)
	case "claim-code":
		err = claimCode(os.Args[2:], os.Stdout)
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		stop()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: hospitium-agent enroll|run|check|claim-code|version [flags]")
	os.Exit(2)
}

func defaultStateDir() string {
	if v := os.Getenv("HOSPITIUM_AGENT_STATE_DIR"); v != "" {
		return v
	}
	return "/var/lib/hospitium-agent"
}

func enroll(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", "", "control plane base URL (https://...)")
	token := fs.String("token", "", "single-use enrollment token from Hospitium")
	dir := fs.String("state-dir", defaultStateDir(), "state directory")
	_ = fs.Parse(args) // ExitOnError
	if *server == "" || *token == "" {
		return fmt.Errorf("--server and --token are required")
	}
	if !strings.HasPrefix(*server, "https://") && !isLocal(*server) {
		return fmt.Errorf("refusing non-HTTPS control plane %q", *server)
	}
	host, _ := os.Hostname()
	c := client.New(*server, "", version)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := c.Enroll(ctx, v1.EnrollRequest{EnrollmentToken: *token, Hostname: host, AgentVersion: version, Hardware: hardwareModel()})
	if err != nil {
		return err
	}
	creds := state.Credentials{ServerURL: *server, ControllerID: res.ControllerID, PropertyID: res.PropertyID, AgentToken: res.AgentToken}
	if err := state.Dir(*dir).Save(state.CredentialsFile, creds); err != nil {
		return err
	}
	fmt.Printf("enrolled as controller %d for property %d\n", res.ControllerID, res.PropertyID)
	return nil
}

// isLocal permits plain HTTP only for loopback development.
func isLocal(u string) bool {
	return strings.HasPrefix(u, "http://localhost") || strings.HasPrefix(u, "http://127.0.0.1")
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dir := fs.String("state-dir", defaultStateDir(), "state directory")
	adb := fs.String("adb", "adb", "path to the adb binary")
	interval := fs.Duration("interval", 45*time.Second, "base poll interval until the server suggests one")
	simulate := fs.Bool("simulate", false, "manage simulated TVs instead of real ones (no hardware; for demos/tests)")
	once := fs.Bool("once", false, "run a single reconcile pass and exit")
	debug := fs.Bool("debug", false, "debug logging")
	localFile := fs.String("local", "", "standalone mode: read desired state from this JSON file instead of a control plane")
	server := fs.String("server", os.Getenv("HOSPITIUM_SERVER"), "control plane URL; an unenrolled hub waits there to be claimed by its code")
	_ = fs.Parse(args) // ExitOnError

	lvl := slog.LevelInfo
	if *debug {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	sd := state.Dir(*dir)
	var api agent.API
	var c *client.Client
	target := ""
	if *localFile != "" {
		// Standalone: no enrollment, no control plane.
		c = client.New("", "", version)
		api = &local.API{Path: *localFile, StatusDir: sd}
		target = "local:" + *localFile
	} else {
		var creds state.Credentials
		ok, err := sd.Load(state.CredentialsFile, &creds)
		if err != nil {
			return err
		}
		if !ok {
			// Not enrolled: wait for the owner to claim this hub by its code (ADR 0017 in the control plane).
			if creds, err = claimHub(ctx, sd, *server, log); err != nil {
				return err
			}
		}
		c = client.New(creds.ServerURL, creds.AgentToken, version)
		api = c
		target = creds.ServerURL
	}
	host, _ := os.Hostname()

	a := &agent.Agent{
		API:       api,
		Artifacts: &agent.FileArtifacts{Dir: filepath.Join(*dir, "artifacts"), Download: c.Download},
		State:     sd,
		Version:   version,
		Hostname:  host,
		Interval:  *interval,
		Log:       log,
	}
	if *simulate {
		fleet := tv.NewSimFleet(sd.Path("simulated-tvs.json"))
		a.Dial = fleet.Dialer()
		a.AfterTick = func() {
			if err := fleet.Save(); err != nil {
				log.ErrorContext(ctx, "save simulated tvs", "err", err)
			}
		}
		log.WarnContext(ctx, "SIMULATION MODE: no real TVs are being managed")
	} else {
		a.Dial = tv.NewADBDialer(*adb)
		if *localFile == "" {
			// Propose the TVs on this network to the owner (managed mode only).
			a.Discover = func(ctx context.Context) ([]v1.DiscoveredTV, error) { return discover.Browse(ctx, 2*time.Second) }
		}
	}

	log.InfoContext(ctx, "hospitium-agent starting", "version", version, "desired_state", target)
	if *once {
		a.Tick(ctx)
		return nil
	}
	return a.Run(ctx)
}

func hardwareModel() string {
	b, err := os.ReadFile("/proc/device-tree/model")
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\x00\n")
}

// claimHub waits until an owner claims this hub by its code, then saves its credential.
func claimHub(ctx context.Context, sd state.Dir, server string, log *slog.Logger) (state.Credentials, error) {
	if server == "" {
		return state.Credentials{}, claim.ErrNoServer
	}
	if !strings.HasPrefix(server, "https://") && !isLocal(server) {
		return state.Credentials{}, fmt.Errorf("refusing non-HTTPS control plane %q", server)
	}
	secret, err := claim.Secret(sd)
	if err != nil {
		return state.Credentials{}, err
	}
	host, _ := os.Hostname()
	res, err := claim.Wait(ctx, client.New(server, "", version), secret,
		v1.ClaimRequest{Hostname: host, AgentVersion: version, Hardware: hardwareModel()}, log, claim.Sleep)
	if err != nil {
		return state.Credentials{}, err
	}
	creds := state.Credentials{ServerURL: server, ControllerID: res.ControllerID, PropertyID: res.PropertyID, AgentToken: res.AgentToken}
	if err := sd.Save(state.CredentialsFile, creds); err != nil {
		return state.Credentials{}, err
	}
	log.InfoContext(ctx, "claimed", "controller", res.ControllerID, "property", res.PropertyID)
	return creds, nil
}

// claimCode prints the hub's claim code (creating its secret on first use), for its label.
func claimCode(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("claim-code", flag.ContinueOnError)
	dir := fs.String("state-dir", defaultStateDir(), "state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	code, err := claim.Code(state.Dir(*dir))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Claim code: %s\n", code)
	fmt.Fprintln(out, "Put it on the hub's label, inside the case or on the packaging: anyone who sees it could claim the hub first.")
	fmt.Fprintln(out, "The QR code on the label holds the same text.")
	return nil
}
