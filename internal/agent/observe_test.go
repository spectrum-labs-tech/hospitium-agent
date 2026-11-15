// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"testing"
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"
)

// caps indexes observations by capability and subject.
func caps(obs []v1.CapabilityObservation) map[string]v1.CapabilityObservation {
	m := map[string]v1.CapabilityObservation{}
	for _, o := range obs {
		m[o.Capability+"/"+o.Subject] = o
	}
	return m
}

func TestReconcileReportsFactsAndObservations(t *testing.T) {
	h := newHarness(t)
	h.agent.Tick(context.Background())
	st := h.api.lastStatus(t)

	if f := st.Facts; f == nil || f.Driver != v1.DriverSimulator || f.BuildFingerprint == "" || f.HomeActivity != launcherPkg+"/.MainActivity" {
		t.Fatalf("facts should identify the TV and HOME after activation: %+v", st.Facts)
	}
	got := caps(st.Capabilities)
	for _, c := range []string{v1.CapHealthMonitoring, v1.CapCustomLauncher, v1.CapRemoteAppInstall} {
		o, ok := got[c+"/"]
		if !ok || o.State != v1.CapSupported {
			t.Fatalf("%s should be observed supported: %+v", c, st.Capabilities)
		}
		if o.Evidence != v1.EvidenceSimulated {
			t.Fatalf("a simulated TV must never report hardware evidence: %+v", o)
		}
	}

	// A compliant pass installs nothing, so it says nothing about installs.
	h.now = h.now.Add(time.Minute)
	h.agent.Tick(context.Background())
	if _, ok := caps(h.api.lastStatus(t).Capabilities)[v1.CapRemoteAppInstall+"/"]; ok {
		t.Fatal("remote_app_install reported without an install")
	}
}

func TestIgnoredActivationIsUnsupported(t *testing.T) {
	h := newHarness(t)
	h.tv.IgnoreHomeSetter = true
	s := spec(1)
	s.Launcher.Activation = v1.ActivationHomeActivity
	h.setSpec(1, s)
	h.agent.Tick(context.Background())
	o, ok := caps(h.api.lastStatus(t).Capabilities)[v1.CapCustomLauncher+"/"]
	if !ok || o.State != v1.CapUnsupported || o.Detail == "" {
		t.Fatalf("an OEM that ignores set-home-activity should be observed unsupported: %+v", o)
	}
}

func TestCleanupReportsEachPackage(t *testing.T) {
	h := newHarness(t)
	h.tv.RefuseClear = map[string]bool{"com.google.android.youtube.tv": true}
	h.api.desired.Operations = []v1.Operation{clearOp("op-c", 1, h.now.Add(time.Hour))}
	h.agent.Tick(context.Background())

	res := h.api.opResults["op-c"]
	if len(res) != 1 {
		t.Fatalf("one result expected: %+v", res)
	}
	got := caps(res[0].Capabilities)
	if o := got[v1.CapAppDataCleanup+"/com.netflix.ninja"]; o.State != v1.CapSupported || o.Evidence != v1.EvidenceSimulated {
		t.Fatalf("netflix should be cleared and verified: %+v", res[0].Capabilities)
	}
	if o := got[v1.CapAppDataCleanup+"/com.google.android.youtube.tv"]; o.State != v1.CapUnsupported {
		t.Fatalf("a refused clear should be unsupported: %+v", res[0].Capabilities)
	}
	if _, ok := got[v1.CapAppDataCleanup+"/com.not.installed"]; ok {
		t.Fatal("a package that is not installed proves nothing either way")
	}
}

func TestCleanupUnreachableReportsNothing(t *testing.T) {
	h := newHarness(t)
	h.tv.Reachable = false
	h.api.desired.Operations = []v1.Operation{clearOp("op-u", 1, h.now.Add(time.Hour))}
	h.agent.Tick(context.Background())
	if r := h.api.opResults["op-u"]; len(r) != 1 || len(r[0].Capabilities) != 0 {
		t.Fatalf("an unreachable TV is not evidence: %+v", r)
	}
}

func TestLauncherPersistenceObservedAfterReboot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset bool
		want  string
	}{
		{"activation survives", false, v1.CapSupported},
		{"OEM restores stock launcher", true, v1.CapUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.tv.ResetHomeOnBoot = tc.reset
			h.agent.Tick(context.Background()) // install and activate

			h.api.desired.Operations = []v1.Operation{{ID: "op-r", DeviceID: 7, Type: v1.OpReboot, Attempt: 1, ExpiresAt: h.now.Add(time.Hour)}}
			h.now = h.now.Add(time.Minute)
			h.agent.Tick(context.Background())
			if o := caps(h.api.opResults["op-r"][0].Capabilities)[v1.CapRemoteReboot+"/"]; o.State != v1.CapSupported {
				t.Fatalf("reboot should be observed: %+v", h.api.opResults["op-r"])
			}

			// The TV is offline while it boots, then comes back. Reconcile
			// backs off after the offline pass, so step past the backoff.
			h.api.desired.Operations = nil
			var o v1.CapabilityObservation
			for i := 0; i < 4 && o.Capability == ""; i++ {
				h.now = h.now.Add(10 * time.Minute)
				h.agent.Tick(context.Background())
				o = caps(h.api.lastStatus(t).Capabilities)[v1.CapLauncherPersistsReboot+"/"]
			}
			if o.State != tc.want {
				t.Fatalf("launcher_persists_reboot = %q, want %q", o.State, tc.want)
			}
			if st := h.api.lastStatus(t); !st.Compliant {
				t.Fatalf("the agent should re-activate after observing: %+v", st)
			}

			h.now = h.now.Add(10 * time.Minute)
			h.agent.Tick(context.Background())
			if _, ok := caps(h.api.lastStatus(t).Capabilities)[v1.CapLauncherPersistsReboot+"/"]; ok {
				t.Fatal("persistence is observed once per reboot, not every pass")
			}
		})
	}
}
