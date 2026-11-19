// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"strings"
	"sync"
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"
)

// Wake to the guestbook (protocol v0.7.0). On TVs that keep their own home screen (Fire OS), the
// agent brings the launcher to the front when the TV turns on and sits idle on that home screen,
// so guests still see the welcome and the message of the day. It acts once per wake, and never
// when the TV woke into an app.

const (
	defaultWakePoll   = 3 * time.Second
	defaultWakeSettle = 2 * time.Second
)

type wakeTarget struct {
	addr      string
	launcher  string // package
	component string
}

type wakeWatch struct {
	mu      sync.Mutex
	targets map[string]wakeTarget // by ADB address
	awake   map[string]bool       // what each TV was last seen as
}

// setWakeTargets records which TVs want the guestbook on wake, from the desired state.
func (a *Agent) setWakeTargets(devices []v1.DeviceAssignment) {
	targets := map[string]wakeTarget{}
	for _, d := range devices {
		l := d.Spec.Launcher
		if l == nil || !l.WakeToGuestbook || d.ADBAddress == "" || l.PackageName == "" {
			continue
		}
		comp := l.HomeActivity
		if comp == "" {
			comp = l.PackageName + "/.MainActivity"
		}
		targets[d.ADBAddress] = wakeTarget{addr: d.ADBAddress, launcher: l.PackageName, component: comp}
	}
	a.wake.mu.Lock()
	defer a.wake.mu.Unlock()
	a.wake.targets = targets
	for addr := range a.wake.awake {
		if _, ok := targets[addr]; !ok {
			delete(a.wake.awake, addr)
		}
	}
}

// watchWake checks the TVs that want it every WakePoll until ctx ends.
func (a *Agent) watchWake(ctx context.Context) {
	every := a.WakePoll
	if every == 0 {
		every = defaultWakePoll
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.checkWake(ctx)
		}
	}
}

// checkWake looks at each TV once: only whether it is awake, which is cheap. A TV first seen awake
// sets a baseline; only a change from asleep to awake counts as a wake.
func (a *Agent) checkWake(ctx context.Context) {
	a.wake.mu.Lock()
	targets := make([]wakeTarget, 0, len(a.wake.targets))
	for _, t := range a.wake.targets {
		targets = append(targets, t)
	}
	a.wake.mu.Unlock()

	for _, t := range targets {
		dev := a.Dial(t.addr)
		awake, err := dev.Awake(ctx)
		a.wake.mu.Lock()
		if a.wake.awake == nil {
			a.wake.awake = map[string]bool{}
		}
		prev, known := a.wake.awake[t.addr]
		if err != nil {
			delete(a.wake.awake, t.addr) // unreachable says nothing; start over when it's back
		} else {
			a.wake.awake[t.addr] = awake
		}
		a.wake.mu.Unlock()
		if err == nil && known && !prev && awake {
			a.onWake(ctx, t)
		}
	}
}

// onWake brings the launcher up if the TV is idle on its own home screen.
func (a *Agent) onWake(ctx context.Context, t wakeTarget) {
	settle := a.WakeSettle
	if settle == 0 {
		settle = defaultWakeSettle
	}
	if settle > 0 {
		timer := time.NewTimer(settle)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	dev := a.Dial(t.addr)
	fg, err := dev.Foreground(ctx)
	if err != nil {
		a.Log.WarnContext(ctx, "wake: foreground", "tv", t.addr, "err", err)
		return
	}
	home, err := dev.HomeActivity(ctx)
	if err != nil {
		a.Log.WarnContext(ctx, "wake: home", "tv", t.addr, "err", err)
		return
	}
	homePkg, _, _ := strings.Cut(home, "/")
	switch {
	case fg == t.launcher:
		return // already up (or it is the home screen here)
	case fg == "" || fg != homePkg:
		return // woke into an app, or nothing to judge by: leave the guest alone
	}
	if err := dev.StartActivity(ctx, t.component, v1.LauncherExtraWake); err != nil {
		a.Log.WarnContext(ctx, "wake: start launcher", "tv", t.addr, "err", err)
		return
	}
	a.Log.InfoContext(ctx, "brought the guestbook up on wake", "tv", t.addr)
}
