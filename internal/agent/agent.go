// SPDX-License-Identifier: Apache-2.0

// Package agent implements the Hospitium management agent: a pull-based
// reconciliation loop that keeps assigned Google TVs at their desired state
// and executes one-time operations at least once, deduplicated by ID.
package agent

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"runtime"
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/state"
	"github.com/spectrum-labs-tech/hospitium-agent/internal/tv"
)

// API is the subset of the control-plane client the loop needs.
type API interface {
	DesiredState(ctx context.Context) (v1.DesiredStateResponse, error)
	ReportStatus(ctx context.Context, r v1.StatusReport) error
	ReportOperation(ctx context.Context, id string, r v1.OperationResult) error
}

type Agent struct {
	API       API
	Dial      tv.Dialer
	Artifacts Artifacts
	State     state.Dir
	Version   string
	Hostname  string
	// Interval is used until the server suggests one. 30-60s is typical.
	Interval time.Duration
	Now      func() time.Time
	Log      *slog.Logger
	// AfterTick is called after each tick (used by --simulate to persist TVs).
	AfterTick func()

	ops          opLog
	devs         map[int64]*deviceState
	pollFailures int
	started      time.Time
}

type deviceState struct {
	failures int
	next     time.Time
	last     v1.DeviceStatus
}

const desiredCacheFile = "desired-state.json"

func (a *Agent) init() error {
	if a.Now == nil {
		a.Now = time.Now
	}
	if a.Log == nil {
		a.Log = slog.Default()
	}
	if a.Interval == 0 {
		a.Interval = 45 * time.Second
	}
	if a.devs == nil {
		a.devs = map[int64]*deviceState{}
	}
	if a.started.IsZero() {
		a.started = a.Now()
	}
	if a.ops == nil {
		a.ops = opLog{}
		if _, err := a.State.Load(opLogFile, &a.ops); err != nil {
			return err
		}
	}
	return nil
}

// Run loops until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.init(); err != nil {
		return err
	}
	for {
		wait := a.Tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// Tick performs one poll/reconcile/report cycle and returns how long to wait
// before the next one.
func (a *Agent) Tick(ctx context.Context) time.Duration {
	if err := a.init(); err != nil {
		a.Log.ErrorContext(ctx, "load state", "err", err)
		return a.Interval
	}
	defer func() {
		if a.AfterTick != nil {
			a.AfterTick()
		}
	}()

	desired, err := a.API.DesiredState(ctx)
	online := err == nil
	if err != nil {
		a.pollFailures++
		a.Log.WarnContext(ctx, "poll failed; reconciling from cached desired state", "err", err, "failures", a.pollFailures)
		if ok, lerr := a.State.Load(desiredCacheFile, &desired); lerr != nil || !ok {
			return backoff(a.pollFailures)
		}
		desired.Operations = nil // never act on stale one-time operations
	} else {
		a.pollFailures = 0
		if err := a.State.Save(desiredCacheFile, desired); err != nil {
			a.Log.ErrorContext(ctx, "cache desired state", "err", err)
		}
	}

	byID := map[int64]v1.DeviceAssignment{}
	for _, d := range desired.Devices {
		byID[d.DeviceID] = d
	}

	skip := map[int64]bool{}
	if online {
		skip = a.processOperations(ctx, desired.Operations, byID)
	}

	statuses := make([]v1.DeviceStatus, 0, len(desired.Devices))
	now := a.Now()
	for _, d := range desired.Devices {
		ds := a.devs[d.DeviceID]
		if ds == nil {
			ds = &deviceState{}
			a.devs[d.DeviceID] = ds
		}
		if skip[d.DeviceID] || now.Before(ds.next) {
			st := ds.last
			st.DeviceID = d.DeviceID
			if !ds.next.IsZero() && now.Before(ds.next) {
				n := ds.next
				st.NextAttemptAt = &n
			}
			statuses = append(statuses, st)
			continue
		}
		st := reconcileDevice(ctx, a.Dial(d.ADBAddress), d, a.Artifacts, now)
		if st.LastError != "" || !st.Reachable {
			ds.failures++
			ds.next = now.Add(backoff(ds.failures))
			n := ds.next
			st.NextAttemptAt = &n
			a.Log.WarnContext(ctx, "reconcile failed", "device", d.DeviceID, "err", st.LastError, "retry_in", ds.next.Sub(now).Round(time.Second))
		} else {
			if ds.failures > 0 || ds.last.ObservedGeneration != st.ObservedGeneration {
				a.Log.InfoContext(ctx, "device reconciled", "device", d.DeviceID, "generation", st.ObservedGeneration, "compliant", st.Compliant)
			}
			ds.failures, ds.next = 0, time.Time{}
		}
		ds.last = st
		statuses = append(statuses, st)
	}

	if online {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		rep := v1.StatusReport{
			AgentVersion: a.Version, Hostname: a.Hostname,
			UptimeSeconds: int64(a.Now().Sub(a.started).Seconds()),
			MemAllocBytes: ms.Alloc, Devices: statuses,
		}
		if err := a.API.ReportStatus(ctx, rep); err != nil {
			a.Log.WarnContext(ctx, "report status", "err", err)
		}
	}

	if !online {
		return backoff(a.pollFailures)
	}
	interval := a.Interval
	if desired.PollIntervalSeconds > 0 {
		interval = time.Duration(desired.PollIntervalSeconds) * time.Second
	}
	return jitter(interval)
}

// processOperations executes new operations and (re-)reports results. It
// returns devices that should skip reconciliation this tick (e.g. rebooting).
func (a *Agent) processOperations(ctx context.Context, ops []v1.Operation, devices map[int64]v1.DeviceAssignment) map[int64]bool {
	skip := map[int64]bool{}
	for _, op := range ops {
		rec := a.ops[op.ID]
		if rec != nil && rec.State == "done" && rec.Result != nil && rec.Attempt >= op.Attempt {
			// Already executed this attempt; the server did not get (or
			// lost) our report. A higher attempt number means the server
			// wants a retry after a reported failure.
			a.Log.InfoContext(ctx, "operation already executed; re-reporting", "op", op.ID, "type", op.Type)
		} else {
			if rec != nil && rec.State == "running" {
				a.Log.WarnContext(ctx, "operation was interrupted; re-executing", "op", op.ID, "type", op.Type)
			}
			rec = &opRecord{ID: op.ID, DeviceID: op.DeviceID, Type: op.Type, Attempt: op.Attempt, State: "running", UpdatedAt: a.Now()}
			a.ops[op.ID] = rec
			a.saveOps(ctx)

			var dev tv.Device
			assign, assigned := devices[op.DeviceID]
			if assigned {
				dev = a.Dial(assign.ADBAddress)
			}
			var res v1.OperationResult
			if op.Type == v1.OpReinstallLauncher && dev != nil {
				res = v1.OperationResult{Attempt: op.Attempt, StartedAt: a.Now()}
				if !op.ExpiresAt.IsZero() && a.Now().After(op.ExpiresAt) {
					res.Status, res.Message = v1.OpStatusRejected, "operation expired before execution"
				} else if err := dev.Connect(ctx); err != nil {
					res.Status, res.Message, res.Retryable = v1.OpStatusFailed, err.Error(), true
				} else if err := reinstallLauncher(ctx, dev, assign.Spec, a.Artifacts); err != nil {
					// A newer launcher on the TV won't change by retrying.
					res.Status, res.Message, res.Retryable = v1.OpStatusFailed, err.Error(), !errors.Is(err, ErrNewerInstalled)
				} else {
					res.Status, res.Message = v1.OpStatusSucceeded, "launcher reinstalled"
				}
				res.FinishedAt = a.Now()
			} else {
				res = executeOperation(ctx, dev, op, a.Now)
			}
			if op.Type == v1.OpReboot && res.Status == v1.OpStatusSucceeded {
				skip[op.DeviceID] = true
			}
			a.Log.InfoContext(ctx, "operation executed", "op", op.ID, "type", op.Type, "device", op.DeviceID, "status", res.Status, "msg", res.Message)
			rec.State, rec.Result, rec.UpdatedAt = "done", &res, a.Now()
			a.saveOps(ctx)
		}
		a.report(ctx, rec)
	}
	// Retry reports that previously failed to send.
	for _, rec := range a.ops {
		if rec.State == "done" && !rec.Reported {
			a.report(ctx, rec)
		}
	}
	a.ops.prune(a.Now())
	a.saveOps(ctx)
	return skip
}

func (a *Agent) report(ctx context.Context, rec *opRecord) {
	if err := a.API.ReportOperation(ctx, rec.ID, *rec.Result); err != nil {
		a.Log.WarnContext(ctx, "report operation result", "op", rec.ID, "err", err)
		rec.Reported = false
		return
	}
	rec.Reported = true
}

func (a *Agent) saveOps(ctx context.Context) {
	if err := a.State.Save(opLogFile, a.ops); err != nil {
		a.Log.ErrorContext(ctx, "persist operation log", "err", err)
	}
}

// backoff: 15s, 30s, 60s ... capped at 10 minutes, with ±20% jitter.
func backoff(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := 15 * time.Second * time.Duration(math.Pow(2, float64(min(failures-1, 10))))
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return jitter(d)
}

func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // jitter, not security
}
