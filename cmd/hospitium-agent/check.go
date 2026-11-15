// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/check"
	"github.com/spectrum-labs-tech/hospitium-agent/internal/tv"
)

// checkTV runs the hardware test protocol against one TV and prints what it
// observed. Anything that changes the TV needs --yes.
func checkTV(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	addr := fs.String("tv", "", "the TV's ADB address (host:port)")
	adb := fs.String("adb", "adb", "path to the adb binary")
	apk := fs.String("launcher-apk", "", "install and activate this launcher APK")
	pkg := fs.String("launcher-package", "tech.spectrumlabs.hospitium.launcher", "the launcher's package name")
	home := fs.String("home-activity", "", "the launcher's HOME component (default <package>/.MainActivity)")
	strategy := fs.String("strategy", "auto", "activation: auto, home_activity or disable_stock")
	stock := fs.String("stock-launchers", "com.google.android.apps.tv.launcherx", "comma-separated stock launcher packages (for disable_stock)")
	reboots := fs.Int("reboots", 0, "reboot this many times, checking ADB and HOME each time")
	clear := fs.String("clear", "", "comma-separated app packages to clear and verify (signs out their accounts)")
	report := fs.String("out", "", "also write the full report as JSON to this file")
	yes := fs.Bool("yes", false, "allow changes to the TV (install, activation, reboots, clearing app data)")
	simulate := fs.Bool("simulate", false, "check a simulated TV (no hardware)")
	poll := fs.Duration("poll", 5*time.Second, "how often to try reconnecting while the TV reboots")
	bootTimeout := fs.Duration("boot-timeout", 3*time.Minute, "how long to wait for the TV to come back after a reboot")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *addr == "" {
		return errors.New("--tv is required")
	}
	o := check.Options{
		LauncherPackage: *pkg, LauncherVersion: 1, HomeActivity: *home, Strategy: *strategy,
		StockLaunchers: splitList(*stock), Reboots: *reboots, Clear: splitList(*clear),
		Poll: *poll, BootTimeout: *bootTimeout,
	}
	if *apk != "" {
		o.LauncherAPK = *apk
	}
	if *reboots < 0 || *reboots > 10 {
		return errors.New("--reboots must be between 0 and 10")
	}
	if changes := plannedChanges(o); len(changes) > 0 && !*yes {
		return fmt.Errorf("this check would change the TV (%s). Never run it while a guest is staying. Add --yes to go ahead",
			strings.Join(changes, ", "))
	}

	var dev tv.Device
	if *simulate {
		dev = tv.NewSimTV(*addr)
	} else {
		dev = tv.NewADBDialer(*adb)(*addr)
	}
	rep := (&check.Runner{Version: version}).Run(ctx, dev, o)
	printReport(out, rep, *simulate)
	if *report != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*report, append(b, '\n'), 0o600); err != nil {
			return err
		}
		fmt.Fprintf(out, "\nFull report: %s\n", *report)
	}
	if !rep.Passed() {
		return errors.New("some checks failed (see above)")
	}
	return nil
}

func plannedChanges(o check.Options) []string {
	var c []string
	if o.LauncherAPK != "" {
		c = append(c, "installs and activates the launcher")
	}
	if o.Reboots > 0 {
		c = append(c, fmt.Sprintf("reboots %d×", o.Reboots))
	}
	if len(o.Clear) > 0 {
		c = append(c, "clears app data for "+strings.Join(o.Clear, ", "))
	}
	return c
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func printReport(w io.Writer, r check.Report, simulated bool) {
	if simulated {
		fmt.Fprintln(w, "SIMULATED TV: these results are not evidence about any real hardware.")
	}
	if r.Facts != nil {
		fmt.Fprintf(w, "%s %s, Android %s, build %s\n", r.Facts.Manufacturer, r.Facts.Model, r.Facts.OSVersion, r.Facts.BuildFingerprint)
	}
	fmt.Fprintln(w, "\nChecks:")
	for _, s := range r.Steps {
		mark := "PASS"
		if !s.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "  %s  %-28s %s\n", mark, s.Name, s.Detail)
	}
	fmt.Fprintln(w, "\nCapabilities observed:")
	for _, o := range r.Observations {
		name := o.Capability
		if o.Subject != "" {
			name += " " + o.Subject
		}
		fmt.Fprintf(w, "  %-11s %-44s %s (%s)\n", o.State, name, o.Detail, o.Evidence)
	}
	if len(r.Manual) > 0 {
		fmt.Fprintln(w, "\nStill to check by hand:")
		for _, m := range r.Manual {
			fmt.Fprintf(w, "  [ ] %s\n", m)
		}
	}
	fmt.Fprintf(w, "\nTook %s.\n", r.FinishedAt.Sub(r.StartedAt).Round(time.Second))
}
