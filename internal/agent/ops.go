// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/tv"
)

// opRecord is the durable execution record for one operation ID. It is
// written before execution starts and again when it finishes, so a crash
// mid-operation is detectable and a finished operation is never re-executed
// just because the result report was lost.
type opRecord struct {
	ID        string              `json:"id"`
	DeviceID  int64               `json:"device_id"`
	Type      string              `json:"type"`
	Attempt   int                 `json:"attempt"`
	State     string              `json:"state"` // running | done
	Result    *v1.OperationResult `json:"result,omitempty"`
	Reported  bool                `json:"reported"`
	UpdatedAt time.Time           `json:"updated_at"`
}

type opLog map[string]*opRecord

const opLogFile = "operations.json"

// opRetention bounds how long finished records are kept for deduplication.
// It must exceed the server's maximum operation lifetime.
const opRetention = 14 * 24 * time.Hour

func (l opLog) prune(now time.Time) {
	for id, r := range l {
		if r.State == "done" && r.Reported && now.Sub(r.UpdatedAt) > opRetention {
			delete(l, id)
		}
	}
}

// executeOperation runs a single operation against dev. It never panics on
// bad input; malformed or unknown operations are rejected (not retried).
func executeOperation(ctx context.Context, dev tv.Device, op v1.Operation, now func() time.Time) v1.OperationResult {
	res := v1.OperationResult{Attempt: op.Attempt, StartedAt: now()}
	finish := func(status, msg string, retryable bool) v1.OperationResult {
		res.Status, res.Message, res.Retryable, res.FinishedAt = status, msg, retryable, now()
		return res
	}
	if !op.ExpiresAt.IsZero() && now().After(op.ExpiresAt) {
		return finish(v1.OpStatusRejected, "operation expired before execution", false)
	}
	if dev == nil {
		return finish(v1.OpStatusRejected, fmt.Sprintf("device %d is not assigned to this agent", op.DeviceID), false)
	}
	if err := dev.Connect(ctx); err != nil {
		return finish(v1.OpStatusFailed, "connect: "+err.Error(), true)
	}

	switch op.Type {
	case v1.OpClearAppData:
		var p v1.ClearAppDataParams
		if err := json.Unmarshal(op.Params, &p); err != nil || len(p.Packages) == 0 {
			return finish(v1.OpStatusRejected, "invalid clear_app_data params", false)
		}
		installed, err := dev.PackageVersions(ctx, p.Packages)
		if err != nil {
			return finish(v1.OpStatusFailed, err.Error(), true)
		}
		res.Verification = map[string]string{}
		var failed []string
		for _, pkg := range p.Packages {
			if _, ok := installed[pkg]; !ok {
				res.Verification[pkg] = "not_installed"
				continue
			}
			if err := dev.ClearAppData(ctx, pkg); err != nil {
				res.Verification[pkg] = "clear_failed: " + err.Error()
				failed = append(failed, pkg)
				continue
			}
			ev, err := dev.VerifyCleared(ctx, pkg)
			res.Verification[pkg] = flatten(ev)
			if err != nil {
				res.Verification[pkg] += "; verify_failed: " + err.Error()
				failed = append(failed, pkg)
			} else {
				res.Verification[pkg] = "cleared; " + res.Verification[pkg]
			}
		}
		if len(failed) > 0 {
			return finish(v1.OpStatusFailed, "not verified: "+strings.Join(failed, ","), true)
		}
		return finish(v1.OpStatusSucceeded, fmt.Sprintf("cleared %d package(s)", len(p.Packages)), false)

	case v1.OpReboot:
		if err := dev.Reboot(ctx); err != nil {
			return finish(v1.OpStatusFailed, err.Error(), true)
		}
		return finish(v1.OpStatusSucceeded, "reboot issued", false)

	case v1.OpDiagnostics:
		d, err := dev.Diagnostics(ctx)
		if err != nil {
			return finish(v1.OpStatusFailed, err.Error(), true)
		}
		res.Verification = d
		return finish(v1.OpStatusSucceeded, "diagnostics collected", false)

	default:
		return finish(v1.OpStatusRejected, "unsupported operation type "+op.Type, false)
	}
}

// reinstallLauncher is handled by the agent loop since it needs the spec. It
// re-installs the desired build IN PLACE (`install -r`), which repairs a
// damaged install while keeping the launcher's data, pairing included. It
// never uninstalls, and refuses if the TV has a newer version (roll forward).
func reinstallLauncher(ctx context.Context, dev tv.Device, spec v1.DeviceSpec, art Artifacts) error {
	if spec.Launcher == nil {
		return errors.New("no launcher in desired state")
	}
	l := spec.Launcher
	versions, err := dev.PackageVersions(ctx, []string{l.PackageName})
	if err != nil {
		return err
	}
	if cur, ok := versions[l.PackageName]; ok && cur > l.VersionCode {
		return newerInstalled(l.PackageName, cur, l.VersionCode)
	}
	// An empty version map makes ensurePackage install even when the version matches.
	_, err = ensurePackage(ctx, dev, art, l.PackageName, l.VersionCode, l.APKURL, l.SHA256, true, map[string]int64{})
	return err
}

func flatten(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}
