// SPDX-License-Identifier: Apache-2.0

// Package check runs the hardware test protocol against one TV and reports
// what it observed. It is meant for a person on site, before a TV goes into
// service: it reboots the TV, changes HOME and clears app data, so it must
// never run while a guest is staying.
//
// It automates what the agent can see for itself. What only a person can
// judge (does HOME escape the launcher from an app, is a streaming app back
// at its sign-in screen) is listed in the report as manual checks.
package check

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/tv"
)

// Options says what to test. Everything that changes the TV is opt-in.
type Options struct {
	// LauncherAPK, when set, is installed and activated. LauncherPackage and
	// LauncherVersion name it (the version is only used by the simulator).
	LauncherAPK     string
	LauncherPackage string
	LauncherVersion int64
	HomeActivity    string // default <package>/.MainActivity
	// Strategy is v1.ActivationHomeActivity, v1.ActivationDisableStock, or
	// "auto" (try home_activity, then disable_stock).
	Strategy       string
	StockLaunchers []string
	// Reboots is how many times to reboot and check that ADB comes back
	// without a prompt and, with a launcher, that HOME stays on it.
	Reboots int
	// Clear lists app packages whose data is cleared and verified.
	Clear []string
	// BootTimeout bounds the wait for the TV to come back after a reboot.
	BootTimeout time.Duration
	// Poll is the interval between reconnect attempts while it boots.
	Poll time.Duration
}

// Step is one test with its outcome.
type Step struct {
	Name     string        `json:"name"`
	Passed   bool          `json:"passed"`
	Detail   string        `json:"detail,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

// Report is everything one run observed. Observations use the protocol
// types, so a control plane can record them as they are.
type Report struct {
	Address      string                     `json:"address"`
	AgentVersion string                     `json:"agent_version"`
	StartedAt    time.Time                  `json:"started_at"`
	FinishedAt   time.Time                  `json:"finished_at"`
	Facts        *v1.DeviceFacts            `json:"facts,omitempty"`
	Steps        []Step                     `json:"steps"`
	Observations []v1.CapabilityObservation `json:"observations"`
	Manual       []string                   `json:"manual_checks"`
}

// Passed reports whether every step passed.
func (r Report) Passed() bool {
	for _, s := range r.Steps {
		if !s.Passed {
			return false
		}
	}
	return len(r.Steps) > 0
}

// Runner holds the clock and sleep, so tests run without waiting.
type Runner struct {
	Now     func() time.Time
	Sleep   func(ctx context.Context, d time.Duration) error
	Version string
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type run struct {
	*Runner
	ctx context.Context
	dev tv.Device
	rep *Report
}

func (r *run) step(name string, fn func() (bool, string)) bool {
	start := r.Now()
	ok, detail := fn()
	r.rep.Steps = append(r.rep.Steps, Step{Name: name, Passed: ok, Detail: detail, Duration: r.Now().Sub(start)})
	return ok
}

func (r *run) observe(capability, subject, state, detail string) {
	ev := v1.EvidenceVerified
	if r.dev.Driver() == v1.DriverSimulator {
		ev = v1.EvidenceSimulated
	}
	r.rep.Observations = append(r.rep.Observations, v1.CapabilityObservation{
		Capability: capability, Subject: subject, State: state, Evidence: ev, Detail: detail, ObservedAt: r.Now(),
	})
}

// Run tests dev and returns the report. It stops early only when the TV
// can't be reached at all; every other failure is recorded and the
// remaining steps still run.
func (rn *Runner) Run(ctx context.Context, dev tv.Device, o Options) Report {
	if rn.Now == nil {
		rn.Now = time.Now
	}
	if rn.Sleep == nil {
		rn.Sleep = sleep
	}
	if o.BootTimeout == 0 {
		o.BootTimeout = 3 * time.Minute
	}
	if o.Poll == 0 {
		o.Poll = 5 * time.Second
	}
	if o.Strategy == "" {
		o.Strategy = "auto"
	}
	rep := &Report{Address: dev.Address(), AgentVersion: rn.Version, StartedAt: rn.Now(), Observations: []v1.CapabilityObservation{}}
	r := &run{Runner: rn, ctx: ctx, dev: dev, rep: rep}
	defer func() { rep.FinishedAt = rn.Now() }()

	if !r.step("connect", r.connect) {
		rep.Manual = manualChecks(o, false)
		rep.FinishedAt = rn.Now()
		return *rep
	}
	launcher := o.LauncherAPK != "" && o.LauncherPackage != ""
	active := false
	if launcher {
		if r.step("install launcher", func() (bool, string) { return r.install(o) }) {
			active = r.step("activate launcher", func() (bool, string) { return r.activate(o) })
		}
	}
	if o.Reboots > 0 {
		r.step(fmt.Sprintf("reboot %d×", o.Reboots), func() (bool, string) { return r.reboots(o, active) })
	}
	for _, pkg := range o.Clear {
		r.step("clear "+pkg, func() (bool, string) { return r.clear(pkg) })
	}
	r.facts()
	rep.Manual = manualChecks(o, active)
	return *rep
}

func (r *run) connect() (bool, string) {
	if err := r.dev.Connect(r.ctx); err != nil {
		if errors.Is(err, tv.ErrUnauthorized) {
			return false, "accept the 'Allow debugging?' prompt on the TV with the remote (tick 'Always allow'), then run again"
		}
		return false, err.Error()
	}
	info, err := r.dev.Info(r.ctx)
	if err != nil {
		return false, "connected, but reading the TV's properties failed: " + err.Error()
	}
	r.observe(v1.CapHealthMonitoring, "", v1.CapSupported, "")
	return true, fmt.Sprintf("%s %s, Android %s (SDK %d)", info.Manufacturer, info.Model, info.OSVersion, info.SDKLevel)
}

func (r *run) facts() {
	info, err := r.dev.Info(r.ctx)
	if err != nil {
		return
	}
	home, _ := r.dev.HomeActivity(r.ctx)
	r.rep.Facts = &v1.DeviceFacts{
		Driver: r.dev.Driver(), Manufacturer: info.Manufacturer, Model: info.Model, OSVersion: info.OSVersion,
		SDKLevel: info.SDKLevel, BuildFingerprint: info.Fingerprint, HomeActivity: home,
	}
}

func (r *run) install(o Options) (bool, string) {
	start := r.Now()
	if err := r.dev.Install(r.ctx, tv.APK{Path: o.LauncherAPK, PackageName: o.LauncherPackage, VersionCode: o.LauncherVersion}); err != nil {
		if errors.Is(err, tv.ErrDowngrade) {
			return false, "a newer launcher is already installed; install a build with a higher versionCode"
		}
		return false, err.Error()
	}
	v, err := r.dev.PackageVersions(r.ctx, []string{o.LauncherPackage})
	if err != nil {
		return false, "installed, but listing packages failed: " + err.Error()
	}
	if _, ok := v[o.LauncherPackage]; !ok {
		r.observe(v1.CapRemoteAppInstall, "", v1.CapUnsupported, "install reported success but the package is missing")
		return false, "install reported success but the package is missing"
	}
	took := r.Now().Sub(start).Round(time.Second)
	r.observe(v1.CapRemoteAppInstall, "", v1.CapSupported, fmt.Sprintf("installed in %s", took))
	return true, fmt.Sprintf("v%d in %s", v[o.LauncherPackage], took)
}

func (r *run) ours(home string, o Options) bool {
	return strings.HasPrefix(home, o.LauncherPackage+"/")
}

func (r *run) activate(o Options) (bool, string) {
	strategies := []string{o.Strategy}
	if o.Strategy == "auto" {
		strategies = []string{v1.ActivationHomeActivity, v1.ActivationDisableStock}
	}
	var tried []string
	for _, s := range strategies {
		home, err := r.apply(s, o)
		if err != nil {
			tried = append(tried, s+": "+err.Error())
			continue
		}
		if r.ours(home, o) {
			r.observe(v1.CapCustomLauncher, "", v1.CapSupported, s)
			return true, s + " works"
		}
		tried = append(tried, s+": HOME resolves to "+home)
	}
	detail := strings.Join(tried, "; ")
	r.observe(v1.CapCustomLauncher, "", v1.CapUnsupported, detail)
	return false, detail
}

// apply runs one activation strategy and returns where HOME resolves after.
func (r *run) apply(strategy string, o Options) (string, error) {
	switch strategy {
	case v1.ActivationHomeActivity:
		comp := o.HomeActivity
		if comp == "" {
			comp = o.LauncherPackage + "/.MainActivity"
		}
		if err := r.dev.SetHomeActivity(r.ctx, comp); err != nil {
			return "", err
		}
	case v1.ActivationDisableStock:
		if len(o.StockLaunchers) == 0 {
			return "", errors.New("no stock launcher packages given (--stock-launchers)")
		}
		for _, p := range o.StockLaunchers {
			if err := r.dev.SetPackageEnabled(r.ctx, p, false); err != nil {
				return "", err
			}
		}
	default:
		return "", fmt.Errorf("unknown strategy %q", strategy)
	}
	return r.dev.HomeActivity(r.ctx)
}

// reboots reboots the TV o.Reboots times. Each time it waits for ADB to come
// back, and, with an active launcher, checks HOME before anything re-applies
// activation.
func (r *run) reboots(o Options, launcherActive bool) (bool, string) {
	kept, back := 0, 0
	var notes []string
	for i := 1; i <= o.Reboots; i++ {
		if err := r.dev.Reboot(r.ctx); err != nil {
			r.observe(v1.CapRemoteReboot, "", v1.CapUnsupported, err.Error())
			return false, fmt.Sprintf("reboot %d: %v", i, err)
		}
		took, err := r.waitForBoot(o)
		if err != nil {
			notes = append(notes, fmt.Sprintf("reboot %d: %v", i, err))
			break
		}
		back++
		if launcherActive {
			home, err := r.dev.HomeActivity(r.ctx)
			switch {
			case err != nil:
				notes = append(notes, fmt.Sprintf("reboot %d: reading HOME failed: %v", i, err))
			case r.ours(home, o):
				kept++
			default:
				notes = append(notes, fmt.Sprintf("reboot %d: HOME resolved to %s", i, home))
			}
		}
		notes = append(notes, fmt.Sprintf("reboot %d back in %s", i, took.Round(time.Second)))
	}
	if back > 0 {
		r.observe(v1.CapRemoteReboot, "", v1.CapSupported, fmt.Sprintf("%d/%d reboots came back without a prompt", back, o.Reboots))
	}
	ok := back == o.Reboots
	if launcherActive {
		switch {
		case kept == o.Reboots:
			r.observe(v1.CapLauncherPersistsReboot, "", v1.CapSupported, fmt.Sprintf("HOME kept the launcher across %d reboots", kept))
		case back == o.Reboots:
			r.observe(v1.CapLauncherPersistsReboot, "", v1.CapUnsupported, fmt.Sprintf("HOME kept the launcher on %d of %d reboots", kept, o.Reboots))
		}
		ok = ok && kept == o.Reboots
	}
	return ok, strings.Join(notes, "; ")
}

func (r *run) waitForBoot(o Options) (time.Duration, error) {
	start := r.Now()
	for {
		err := r.dev.Connect(r.ctx)
		if err == nil {
			return r.Now().Sub(start), nil
		}
		if errors.Is(err, tv.ErrUnauthorized) {
			return 0, errors.New("the TV asked to authorize debugging again after the reboot; the hub could not manage it unattended")
		}
		if r.Now().Sub(start) >= o.BootTimeout {
			return 0, fmt.Errorf("not back after %s (is network debugging still on?)", o.BootTimeout)
		}
		if err := r.Sleep(r.ctx, o.Poll); err != nil {
			return 0, err
		}
	}
}

func (r *run) clear(pkg string) (bool, string) {
	v, err := r.dev.PackageVersions(r.ctx, []string{pkg})
	if err != nil {
		return false, err.Error()
	}
	if _, ok := v[pkg]; !ok {
		return false, "not installed"
	}
	if err := r.dev.ClearAppData(r.ctx, pkg); err != nil {
		if errors.Is(err, tv.ErrRefused) {
			r.observe(v1.CapAppDataCleanup, pkg, v1.CapUnsupported, err.Error())
		}
		return false, err.Error()
	}
	ev, err := r.dev.VerifyCleared(r.ctx, pkg)
	if err != nil {
		return false, "cleared, but verification failed: " + err.Error()
	}
	r.observe(v1.CapAppDataCleanup, pkg, v1.CapSupported, "cleared and verified")
	parts := make([]string, 0, len(ev))
	for k, val := range ev {
		parts = append(parts, k+"="+val)
	}
	return true, strings.Join(parts, " ")
}

// manualChecks is the part of the test protocol only a person can judge.
func manualChecks(o Options, launcherActive bool) []string {
	m := []string{
		"Power-cycle the TV at the wall twice; run this check again with no flags and confirm it connects without a prompt.",
	}
	if launcherActive {
		m = append(m,
			"Press HOME from Netflix, YouTube, Settings and an HDMI input; each should return to the Hospitium launcher.",
			"Long-press HOME, press the voice button and the input switcher; note anything that escapes the launcher.",
			"After a wall power-cycle, confirm the launcher comes up as the home screen.",
		)
		if o.Strategy != v1.ActivationHomeActivity {
			m = append(m, "If disable_stock was used: check Assistant, ambient mode, the app store and Settings still work.")
		}
	}
	for _, p := range o.Clear {
		m = append(m, fmt.Sprintf("Open %s: it should show its sign-in screen, and the test account's device list should no longer show this TV.", p))
	}
	return m
}
