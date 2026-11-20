// SPDX-License-Identifier: Apache-2.0

package check

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/tv"
)

const launcher = "tv.hospitium.launcher"

// runner returns a Runner whose sleeps advance a fake clock instantly.
func runner() *Runner {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	return &Runner{
		Version: "test",
		Now:     func() time.Time { return now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			now = now.Add(d)
			return nil
		},
	}
}

func opts() Options {
	return Options{
		LauncherAPK: "/tmp/launcher.apk", LauncherPackage: launcher, LauncherVersion: 1,
		StockLaunchers: []string{tv.SimStockLauncher}, Reboots: 2, Clear: []string{"com.netflix.ninja"},
	}
}

func observed(r Report) map[string]v1.CapabilityObservation {
	m := map[string]v1.CapabilityObservation{}
	for _, o := range r.Observations {
		m[o.Capability+"/"+o.Subject] = o
	}
	return m
}

func TestFullCheckOnACooperativeTV(t *testing.T) {
	t.Parallel()
	sim := tv.NewSimTV("10.0.50.21:5555")
	rep := runner().Run(context.Background(), sim, opts())

	if !rep.Passed() {
		t.Fatalf("every step should pass: %+v", rep.Steps)
	}
	got := observed(rep)
	for _, k := range []string{
		v1.CapHealthMonitoring + "/", v1.CapRemoteAppInstall + "/", v1.CapCustomLauncher + "/", v1.CapRemoteReboot + "/",
		v1.CapLauncherPersistsReboot + "/", v1.CapAppDataCleanup + "/com.netflix.ninja",
	} {
		o, ok := got[k]
		if !ok || o.State != v1.CapSupported {
			t.Fatalf("%s should be supported: %+v", k, rep.Observations)
		}
		if o.Evidence != v1.EvidenceSimulated {
			t.Fatalf("simulator results must be labeled simulated: %+v", o)
		}
	}
	if got[v1.CapCustomLauncher+"/"].Detail != v1.ActivationHomeActivity {
		t.Fatalf("auto should succeed with home_activity first: %+v", got[v1.CapCustomLauncher+"/"])
	}
	// Two reboots on the fake clock take time; the report must say how long, not a zero end time.
	if took := rep.FinishedAt.Sub(rep.StartedAt); took <= 0 {
		t.Fatalf("FinishedAt %v is not after StartedAt %v", rep.FinishedAt, rep.StartedAt)
	}
	if sim.Snapshot().Boots != 2 {
		t.Fatalf("expected 2 reboots, got %d", sim.Snapshot().Boots)
	}
	if rep.Facts == nil || rep.Facts.BuildFingerprint == "" || !strings.HasPrefix(rep.Facts.HomeActivity, launcher+"/") {
		t.Fatalf("facts should identify the build and show the launcher as HOME: %+v", rep.Facts)
	}
	if len(rep.Manual) < 4 {
		t.Fatalf("a person still has to check the HOME button and sign-in screens: %v", rep.Manual)
	}
}

func TestAutoFallsBackToDisableStock(t *testing.T) {
	t.Parallel()
	sim := tv.NewSimTV("10.0.50.21:5555")
	sim.IgnoreHomeSetter = true
	o := opts()
	o.Reboots, o.Clear = 0, nil
	rep := runner().Run(context.Background(), sim, o)
	if c := observed(rep)[v1.CapCustomLauncher+"/"]; c.State != v1.CapSupported || c.Detail != v1.ActivationDisableStock {
		t.Fatalf("an OEM ignoring set-home-activity should fall back to disable_stock: %+v", c)
	}
}

func TestLauncherLostOnBootIsUnsupported(t *testing.T) {
	t.Parallel()
	sim := tv.NewSimTV("10.0.50.21:5555")
	sim.ResetHomeOnBoot = true
	o := opts()
	o.Clear = nil
	rep := runner().Run(context.Background(), sim, o)
	if rep.Passed() {
		t.Fatal("the reboot step should fail when HOME reverts")
	}
	if c := observed(rep)[v1.CapLauncherPersistsReboot+"/"]; c.State != v1.CapUnsupported || !strings.Contains(c.Detail, "0 of 2") {
		t.Fatalf("persistence: %+v", c)
	}
	if c := observed(rep)[v1.CapRemoteReboot+"/"]; c.State != v1.CapSupported {
		t.Fatalf("the reboots themselves worked: %+v", c)
	}
}

func TestUnauthorizedTVStopsWithGuidance(t *testing.T) {
	t.Parallel()
	sim := tv.NewSimTV("10.0.50.21:5555")
	sim.Authorized = false
	rep := runner().Run(context.Background(), sim, opts())
	if len(rep.Steps) != 1 || rep.Steps[0].Passed || !strings.Contains(rep.Steps[0].Detail, "Allow debugging") {
		t.Fatalf("should stop at connect with guidance: %+v", rep.Steps)
	}
	if len(rep.Observations) != 0 {
		t.Fatalf("an unreachable TV is not evidence: %+v", rep.Observations)
	}
	if sim.Snapshot().Boots != 0 || len(sim.Snapshot().Packages) == 0 {
		t.Fatal("nothing should have been changed")
	}
}

func TestTVThatNeverComesBack(t *testing.T) {
	t.Parallel()
	sim := tv.NewSimTV("10.0.50.21:5555")
	o := opts()
	o.LauncherAPK, o.Clear, o.Reboots, o.BootTimeout = "", nil, 1, time.Minute
	rep := runner().Run(context.Background(), &stuckAfterReboot{SimTV: sim}, o)
	last := rep.Steps[len(rep.Steps)-1]
	if last.Passed || !strings.Contains(last.Detail, "not back after 1m0s") {
		t.Fatalf("should time out: %+v", last)
	}
	if _, ok := observed(rep)[v1.CapRemoteReboot+"/"]; ok {
		t.Fatal("a reboot that never came back proves nothing about remote reboot")
	}
}

// stuckAfterReboot is a TV that never answers again once rebooted.
type stuckAfterReboot struct {
	*tv.SimTV
	rebooted bool
}

func (s *stuckAfterReboot) Reboot(ctx context.Context) error {
	s.rebooted = true
	return s.SimTV.Reboot(ctx)
}

func (s *stuckAfterReboot) Connect(ctx context.Context) error {
	if s.rebooted {
		return tv.ErrUnreachable
	}
	return s.SimTV.Connect(ctx)
}

func TestRefusedClearIsUnsupported(t *testing.T) {
	t.Parallel()
	sim := tv.NewSimTV("10.0.50.21:5555")
	sim.RefuseClear = map[string]bool{"com.google.android.youtube.tv": true}
	o := Options{Clear: []string{"com.google.android.youtube.tv", "com.not.installed"}}
	rep := runner().Run(context.Background(), sim, o)
	got := observed(rep)
	if c := got[v1.CapAppDataCleanup+"/com.google.android.youtube.tv"]; c.State != v1.CapUnsupported {
		t.Fatalf("refused clear: %+v", c)
	}
	if _, ok := got[v1.CapAppDataCleanup+"/com.not.installed"]; ok {
		t.Fatal("a missing app proves nothing")
	}
	if rep.Passed() {
		t.Fatal("failed clears should fail the run")
	}
}
