// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"testing"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/tv"
)

// wakeHarness: the harness's TV wants the guestbook on wake, and its launcher is installed.
func wakeHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	s := spec(1)
	s.Launcher.Activation = v1.ActivationNone
	s.Launcher.WakeToGuestbook = true
	h.setSpec(1, s)
	h.agent.WakeSettle = -1
	h.agent.Tick(context.Background()) // installs the launcher, learns the targets
	return h
}

func TestWakeBringsUpTheGuestbookOncePerWake(t *testing.T) {
	t.Parallel()
	h := wakeHarness(t)
	ctx := context.Background()

	h.agent.checkWake(ctx) // first look: a baseline, never a wake
	if s := h.tv.Snapshot(); s.OnScreen != "" {
		t.Fatalf("nothing should start on the first look: %+v", s.OnScreen)
	}

	h.tv.SetAwake(false)
	h.agent.checkWake(ctx)
	h.tv.SetAwake(true) // back on the stock home screen
	h.agent.checkWake(ctx)
	s := h.tv.Snapshot()
	if s.OnScreen != launcherPkg || len(s.LastExtras) != 1 || s.LastExtras[0] != v1.LauncherExtraWake {
		t.Fatalf("the launcher should be up with the wake extra: %q %v", s.OnScreen, s.LastExtras)
	}

	// Still awake: no second start, even if the guest went back to the stock home screen.
	h.tv.Open(tv.SimStockLauncher)
	h.agent.checkWake(ctx)
	if s := h.tv.Snapshot(); s.OnScreen != tv.SimStockLauncher {
		t.Fatalf("started again without a new wake: %q", s.OnScreen)
	}
}

func TestWakeLeavesAnAppAlone(t *testing.T) {
	t.Parallel()
	h := wakeHarness(t)
	ctx := context.Background()
	h.agent.checkWake(ctx)
	h.tv.SetAwake(false)
	h.agent.checkWake(ctx)
	h.tv.SetAwake(true)
	h.tv.Open("com.netflix.ninja") // the TV woke straight into Netflix
	h.agent.checkWake(ctx)
	if s := h.tv.Snapshot(); s.OnScreen != "com.netflix.ninja" {
		t.Fatalf("a guest's app was interrupted: %q", s.OnScreen)
	}
}

func TestWakeOnlyWhereAsked(t *testing.T) {
	t.Parallel()
	h := newHarness(t) // spec without wake_to_guestbook
	h.agent.WakeSettle = -1
	ctx := context.Background()
	h.agent.Tick(ctx)
	h.agent.checkWake(ctx)
	h.tv.SetAwake(false)
	h.agent.checkWake(ctx)
	h.tv.SetAwake(true)
	h.agent.checkWake(ctx)
	if s := h.tv.Snapshot(); s.OnScreen != "" || s.LastExtras != nil {
		t.Fatalf("a TV that didn't ask was touched: %q", s.OnScreen)
	}
}
