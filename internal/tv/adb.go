// SPDX-License-Identifier: Apache-2.0

package tv

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ADB drives a TV through the platform `adb` binary (Debian package `adb`).
// The ADB key lives in $HOME/.android/adbkey of the agent's service user; it
// must persist across agent restarts or every TV will prompt again.
type ADB struct {
	Bin  string // path to adb, default "adb"
	Addr string // host:port
	// Timeout bounds every individual adb invocation.
	Timeout time.Duration
}

func NewADBDialer(bin string) Dialer {
	if bin == "" {
		bin = "adb"
	}
	return func(addr string) Device { return &ADB{Bin: bin, Addr: addr, Timeout: 45 * time.Second} }
}

func (a *ADB) Address() string { return a.Addr }

func (a *ADB) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	if timeout == 0 {
		timeout = a.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// a.Bin is operator configuration; args are built by this package from
	// validated values, and shell arguments are quoted (shellQuote).
	cmd := exec.CommandContext(ctx, a.Bin, args...) //nolint:gosec // see above
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	s := strings.TrimSpace(strings.ReplaceAll(out.String(), "\r\n", "\n"))
	if err != nil {
		if ctx.Err() != nil {
			return s, fmt.Errorf("adb %s: %w (%w)", args[0], ErrUnreachable, ctx.Err())
		}
		return s, classify(s, fmt.Errorf("adb %s: %w: %s", strings.Join(args, " "), err, s))
	}
	return s, nil
}

func classify(out string, err error) error {
	switch {
	case strings.Contains(out, "unauthorized"), strings.Contains(out, "failed to authenticate"):
		return fmt.Errorf("%w: %w", ErrUnauthorized, err)
	case strings.Contains(out, "offline"), strings.Contains(out, "not found"),
		strings.Contains(out, "cannot connect"), strings.Contains(out, "unable to connect"),
		strings.Contains(out, "Connection refused"), strings.Contains(out, "No route"):
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	return err
}

// shell runs a fixed command on the TV. Every argument is single-quoted so the
// remote /system/bin/sh treats it literally.
func (a *ADB) shell(ctx context.Context, args ...string) (string, error) {
	q := make([]string, len(args))
	for i, s := range args {
		q[i] = shellQuote(s)
	}
	return a.run(ctx, 0, "-s", a.Addr, "shell", strings.Join(q, " "))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (a *ADB) Connect(ctx context.Context) error {
	out, err := a.run(ctx, 15*time.Second, "connect", a.Addr)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "connected to") {
		return classify(out, fmt.Errorf("adb connect %s: %s", a.Addr, out))
	}
	state, err := a.run(ctx, 10*time.Second, "-s", a.Addr, "get-state")
	if err != nil {
		return err
	}
	switch strings.TrimSpace(state) {
	case "device":
		return nil
	case "unauthorized":
		return ErrUnauthorized
	default:
		return fmt.Errorf("%w: state %q", ErrUnreachable, state)
	}
}

func (a *ADB) Info(ctx context.Context) (Info, error) {
	var info Info
	props := map[string]*string{
		"ro.product.model":         &info.Model,
		"ro.product.manufacturer":  &info.Manufacturer,
		"ro.build.version.release": &info.OSVersion,
		"ro.build.fingerprint":     &info.Fingerprint,
	}
	for k, dst := range props {
		v, err := a.shell(ctx, "getprop", k)
		if err != nil {
			return info, err
		}
		*dst = v
	}
	sdk, err := a.shell(ctx, "getprop", "ro.build.version.sdk")
	if err != nil {
		return info, err
	}
	info.SDKLevel, _ = strconv.Atoi(sdk)
	return info, nil
}

var pkgLine = regexp.MustCompile(`^package:(\S+) versionCode:(\d+)`)

func (a *ADB) PackageVersions(ctx context.Context, pkgs []string) (map[string]int64, error) {
	out, err := a.shell(ctx, "pm", "list", "packages", "--show-versioncode")
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, p := range pkgs {
		want[p] = true
	}
	res := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		m := pkgLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil || !want[m[1]] {
			continue
		}
		v, _ := strconv.ParseInt(m[2], 10, 64)
		res[m[1]] = v
	}
	return res, nil
}

func (a *ADB) DisabledPackages(ctx context.Context) (map[string]bool, error) {
	out, err := a.shell(ctx, "pm", "list", "packages", "-d")
	if err != nil {
		return nil, err
	}
	res := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "package:"); ok {
			res[p] = true
		}
	}
	return res, nil
}

func (a *ADB) Install(ctx context.Context, apk APK) error {
	out, err := a.run(ctx, 5*time.Minute, "-s", a.Addr, "install", "-r", apk.Path)
	if strings.Contains(out, "INSTALL_FAILED_VERSION_DOWNGRADE") {
		return ErrDowngrade
	}
	if err != nil {
		return err
	}
	if !strings.Contains(out, "Success") {
		return fmt.Errorf("install %s: %s", apk.PackageName, out)
	}
	return nil
}

func (a *ADB) GetSetting(ctx context.Context, ns, key string) (string, error) {
	return a.shell(ctx, "settings", "get", ns, key)
}

func (a *ADB) PutSetting(ctx context.Context, ns, key, value string) error {
	_, err := a.shell(ctx, "settings", "put", ns, key, value)
	return err
}

func (a *ADB) HomeActivity(ctx context.Context) (string, error) {
	out, err := a.shell(ctx, "cmd", "package", "resolve-activity", "--brief",
		"-a", "android.intent.action.MAIN", "-c", "android.intent.category.HOME")
	if err != nil {
		return "", err
	}
	lines := strings.Split(out, "\n")
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

func (a *ADB) SetHomeActivity(ctx context.Context, component string) error {
	out, err := a.shell(ctx, "cmd", "package", "set-home-activity", component)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "Success") {
		return fmt.Errorf("set-home-activity: %s", out)
	}
	return nil
}

func (a *ADB) SetPackageEnabled(ctx context.Context, pkg string, enabled bool) error {
	var out string
	var err error
	if enabled {
		out, err = a.shell(ctx, "pm", "enable", pkg)
	} else {
		out, err = a.shell(ctx, "pm", "disable-user", "--user", "0", pkg)
	}
	if err != nil {
		return err
	}
	if !strings.Contains(out, "new state") {
		return fmt.Errorf("pm enable/disable %s: %s", pkg, out)
	}
	return nil
}

func (a *ADB) ClearAppData(ctx context.Context, pkg string) error {
	out, err := a.shell(ctx, "pm", "clear", pkg)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "Success") {
		return fmt.Errorf("pm clear %s: %s", pkg, out)
	}
	return nil
}

// VerifyCleared collects what can be observed without root: the clear
// command's success (checked by the caller), that the app process is not
// running, and that its external-storage directory is empty. It cannot prove
// that server-side tokens were revoked; see docs/streaming-cleanup.md.
func (a *ADB) VerifyCleared(ctx context.Context, pkg string) (map[string]string, error) {
	ev := map[string]string{"method": "pm-clear+process+external-storage"}
	pid, _ := a.shell(ctx, "pidof", pkg)
	ev["process_running"] = strconv.FormatBool(strings.TrimSpace(pid) != "")
	ls, _ := a.shell(ctx, "ls", "/sdcard/Android/data/"+pkg)
	ev["external_data_empty"] = strconv.FormatBool(strings.TrimSpace(ls) == "" || strings.Contains(ls, "No such file"))
	if ev["process_running"] == "true" {
		return ev, fmt.Errorf("%s still running after clear", pkg)
	}
	return ev, nil
}

func (a *ADB) Reboot(ctx context.Context) error {
	_, err := a.run(ctx, 30*time.Second, "-s", a.Addr, "reboot")
	return err
}

// Diagnostics runs a fixed, read-only command set.
func (a *ADB) Diagnostics(ctx context.Context) (map[string]string, error) {
	cmds := map[string][]string{
		"uptime":      {"uptime"},
		"fingerprint": {"getprop", "ro.build.fingerprint"},
		"disk_data":   {"df", "-h", "/data"},
		"home":        {"cmd", "package", "resolve-activity", "--brief", "-a", "android.intent.action.MAIN", "-c", "android.intent.category.HOME"},
	}
	res := map[string]string{}
	for k, c := range cmds {
		out, err := a.shell(ctx, c...)
		if err != nil {
			res[k] = "error: " + err.Error()
			continue
		}
		res[k] = out
	}
	return res, nil
}
