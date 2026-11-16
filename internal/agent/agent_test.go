// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/state"
	"github.com/spectrum-labs-tech/hospitium-agent/internal/tv"
)

const launcherPkg = "tech.spectrumlabs.hospitium.launcher"

type fakeAPI struct {
	mu        sync.Mutex
	desired   v1.DesiredStateResponse
	pollErr   error
	reportErr error
	statuses  []v1.StatusReport
	opResults map[string][]v1.OperationResult
}

func (f *fakeAPI) DesiredState(ctx context.Context) (v1.DesiredStateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pollErr != nil {
		return v1.DesiredStateResponse{}, f.pollErr
	}
	b, _ := json.Marshal(f.desired)
	var c v1.DesiredStateResponse
	_ = json.Unmarshal(b, &c)
	return c, nil
}

func (f *fakeAPI) ReportStatus(ctx context.Context, r v1.StatusReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, r)
	return nil
}

func (f *fakeAPI) ReportOperation(ctx context.Context, id string, r v1.OperationResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reportErr != nil {
		return f.reportErr
	}
	if f.opResults == nil {
		f.opResults = map[string][]v1.OperationResult{}
	}
	f.opResults[id] = append(f.opResults[id], r)
	return nil
}

func (f *fakeAPI) lastStatus(t *testing.T) v1.DeviceStatus {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.statuses) == 0 || len(f.statuses[len(f.statuses)-1].Devices) == 0 {
		t.Fatal("no status reported")
	}
	return f.statuses[len(f.statuses)-1].Devices[0]
}

type fakeArtifacts struct{ fetched []string }

func (f *fakeArtifacts) Fetch(ctx context.Context, url, sum string) (string, error) {
	f.fetched = append(f.fetched, url)
	return "/artifacts/" + sum + ".apk", nil
}

type harness struct {
	t     *testing.T
	api   *fakeAPI
	fleet *tv.SimFleet
	tv    *tv.SimTV
	dir   state.Dir
	now   time.Time
	agent *Agent
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, api: &fakeAPI{}, fleet: tv.NewSimFleet(""), dir: state.Dir(t.TempDir()),
		now: time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)}
	h.tv = h.fleet.Get("10.0.50.21:5555")
	h.api.desired = v1.DesiredStateResponse{
		ControllerID: 1, PollIntervalSeconds: 30,
		Devices: []v1.DeviceAssignment{{DeviceID: 7, Name: "Cabin TV", ADBAddress: "10.0.50.21:5555", Generation: 1, Spec: spec(1)}},
	}
	h.agent = h.newAgent()
	return h
}

func (h *harness) newAgent() *Agent {
	return &Agent{API: h.api, Dial: h.fleet.Dialer(), Artifacts: &fakeArtifacts{}, State: h.dir,
		Version: "test", Now: func() time.Time { return h.now },
		Log: slog.New(slog.DiscardHandler)}
}

func spec(version int64) v1.DeviceSpec {
	return v1.DeviceSpec{
		Launcher: &v1.LauncherSpec{PackageName: launcherPkg, VersionCode: version,
			APKURL: "https://hospitium.test/apk", SHA256: strings.Repeat("a", 63) + string(rune('0'+version)),
			Activation: v1.ActivationDisableStock, StockLaunchers: []string{tv.SimStockLauncher}},
		RequiredApps:    []v1.AppSpec{{PackageName: "com.netflix.ninja"}},
		Settings:        []v1.Setting{{Namespace: "secure", Key: "screensaver_enabled", Value: "0"}},
		CleanupPackages: []string{"com.netflix.ninja", "com.google.android.youtube.tv"},
	}
}

func (h *harness) setSpec(gen int64, s v1.DeviceSpec) {
	h.api.mu.Lock()
	h.api.desired.Devices[0].Generation, h.api.desired.Devices[0].Spec = gen, s
	h.api.mu.Unlock()
}

func (h *harness) mutatingCalls() []string {
	var out []string
	for _, c := range h.tv.Calls {
		for _, p := range []string{"install", "uninstall", "enable", "settings put", "set-home", "clear", "reboot"} {
			if strings.HasPrefix(c, p) {
				out = append(out, c)
			}
		}
	}
	return out
}

func TestReconcileInstallsActivatesAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.agent.Tick(context.Background())

	st := h.api.lastStatus(t)
	if !st.Compliant || st.ObservedGeneration != 1 || st.LastError != "" {
		t.Fatalf("not reconciled: %+v", st)
	}
	snap := h.tv.Snapshot()
	if snap.Packages[launcherPkg] != 1 || !snap.Disabled[tv.SimStockLauncher] || snap.Settings["secure/screensaver_enabled"] != "0" {
		t.Fatalf("tv state wrong: %+v", snap)
	}
	if home, _ := h.tv.HomeActivity(context.Background()); !strings.HasPrefix(home, launcherPkg+"/") {
		t.Fatalf("home = %s", home)
	}

	// Second pass on a compliant TV must not change anything.
	h.tv.Calls = nil
	h.now = h.now.Add(time.Minute)
	h.agent.Tick(context.Background())
	if m := h.mutatingCalls(); len(m) != 0 {
		t.Fatalf("compliant device was modified: %v", m)
	}
	// Ordinary reconciliation must never clear streaming data.
	if !h.tv.Snapshot().AppData["com.netflix.ninja"] {
		t.Fatal("reconcile cleared app data")
	}
}

func TestLauncherUpgradeKeepsData(t *testing.T) {
	h := newHarness(t)
	h.agent.Tick(context.Background())
	h.tv.SignIn(launcherPkg) // the launcher paired: its data holds the device token

	h.setSpec(2, spec(2))
	h.now = h.now.Add(time.Minute)
	h.agent.Tick(context.Background())
	if st := h.api.lastStatus(t); st.ObservedGeneration != 2 || st.Packages[launcherPkg] != 2 || !st.Compliant {
		t.Fatalf("upgrade not applied: %+v", st)
	}
	if !h.tv.Snapshot().AppData[launcherPkg] {
		t.Fatal("upgrade wiped the launcher's data")
	}
}

// Rolling back would mean uninstalling (Android refuses `install -r` to a lower
// versionCode on user builds), which deletes the launcher's pairing. The agent
// refuses, touches nothing, and says how to roll forward instead.
func TestLauncherNeverDowngrades(t *testing.T) {
	h := newHarness(t)
	h.setSpec(1, spec(2))
	h.agent.Tick(context.Background())
	h.tv.SignIn(launcherPkg)

	h.tv.Calls = nil
	h.setSpec(2, spec(1))
	h.now = h.now.Add(time.Minute)
	h.agent.Tick(context.Background())

	st := h.api.lastStatus(t)
	if st.Compliant || st.Packages[launcherPkg] != 2 || st.ObservedGeneration == 2 {
		t.Fatalf("expected a refused downgrade: %+v", st)
	}
	if !strings.Contains(st.LastError, "never downgrades") || !strings.Contains(st.LastError, "versionCode above 2") {
		t.Fatalf("error doesn't explain rolling forward: %q", st.LastError)
	}
	for _, c := range h.tv.Calls {
		if strings.HasPrefix(c, "install") || strings.HasPrefix(c, "uninstall") {
			t.Fatalf("downgrade attempted: %v", h.tv.Calls)
		}
	}
	if !h.tv.Snapshot().AppData[launcherPkg] {
		t.Fatal("launcher data lost")
	}

	// Rolling forward: the old build republished with a higher versionCode.
	h.setSpec(3, spec(3))
	h.now = h.now.Add(time.Hour) // past any backoff
	h.agent.Tick(context.Background())
	if st := h.api.lastStatus(t); !st.Compliant || st.Packages[launcherPkg] != 3 || !h.tv.Snapshot().AppData[launcherPkg] {
		t.Fatalf("roll forward failed: %+v", st)
	}
}

func TestReinstallLauncherKeepsDataAndNeverUninstalls(t *testing.T) {
	h := newHarness(t)
	h.agent.Tick(context.Background())
	h.tv.SignIn(launcherPkg)

	h.tv.Calls = nil
	h.api.desired.Operations = []v1.Operation{{ID: "op-ri", DeviceID: 7, Type: v1.OpReinstallLauncher, Attempt: 1, ExpiresAt: h.now.Add(time.Hour)}}
	h.now = h.now.Add(time.Minute)
	h.agent.Tick(context.Background())

	res := h.api.opResults["op-ri"]
	if len(res) == 0 || res[0].Status != v1.OpStatusSucceeded {
		t.Fatalf("reinstall failed: %+v", res)
	}
	if countPrefix(h.tv.Calls, "install "+launcherPkg) != 1 || countPrefix(h.tv.Calls, "uninstall") != 0 {
		t.Fatalf("expected one in-place install and no uninstall, calls=%v", h.tv.Calls)
	}
	if !h.tv.Snapshot().AppData[launcherPkg] {
		t.Fatal("reinstall wiped the launcher's pairing")
	}
}

func TestReinstallLauncherRefusesWhenNewerIsInstalled(t *testing.T) {
	h := newHarness(t)
	h.setSpec(1, spec(2))
	h.agent.Tick(context.Background())

	h.setSpec(2, spec(1))
	h.tv.Calls = nil
	h.api.desired.Operations = []v1.Operation{{ID: "op-ri", DeviceID: 7, Type: v1.OpReinstallLauncher, Attempt: 1, ExpiresAt: h.now.Add(time.Hour)}}
	h.now = h.now.Add(time.Minute)
	h.agent.Tick(context.Background())

	res := h.api.opResults["op-ri"]
	if len(res) == 0 || res[0].Status != v1.OpStatusFailed || res[0].Retryable {
		t.Fatalf("expected a non-retryable failure: %+v", res)
	}
	if countPrefix(h.tv.Calls, "install") != 0 {
		t.Fatalf("installed anyway: %v", h.tv.Calls)
	}
}

func TestFailedInstallBacksOffThenRecovers(t *testing.T) {
	h := newHarness(t)
	h.tv.FailInstalls = 1
	h.agent.Tick(context.Background())
	st := h.api.lastStatus(t)
	if st.Compliant || st.LastError == "" || st.NextAttemptAt == nil || st.ObservedGeneration != 0 {
		t.Fatalf("expected failure with backoff: %+v", st)
	}

	// Within the backoff window the device is not touched.
	h.tv.Calls = nil
	h.now = h.now.Add(5 * time.Second)
	h.agent.Tick(context.Background())
	if len(h.tv.Calls) != 0 {
		t.Fatalf("device touched during backoff: %v", h.tv.Calls)
	}

	h.now = h.now.Add(time.Minute)
	h.agent.Tick(context.Background())
	if st := h.api.lastStatus(t); !st.Compliant || st.ObservedGeneration != 1 {
		t.Fatalf("did not recover: %+v", st)
	}
}

func TestTVRebootAndADBAuthorization(t *testing.T) {
	h := newHarness(t)
	h.tv.Authorized = false
	h.agent.Tick(context.Background())
	st := h.api.lastStatus(t)
	if st.Authorized || !strings.Contains(st.LastError, "Allow debugging") {
		t.Fatalf("expected unauthorized guidance: %+v", st)
	}

	h.tv.Authorized = true
	h.tv.OfflineConnects = 1 // rebooting
	h.now = h.now.Add(time.Hour)
	h.agent.Tick(context.Background())
	if st := h.api.lastStatus(t); st.Reachable {
		t.Fatalf("expected unreachable during reboot: %+v", st)
	}
	h.now = h.now.Add(time.Hour)
	h.agent.Tick(context.Background())
	if st := h.api.lastStatus(t); !st.Compliant {
		t.Fatalf("did not recover after reboot: %+v", st)
	}
}

func clearOp(id string, attempt int, expires time.Time) v1.Operation {
	p, _ := json.Marshal(v1.ClearAppDataParams{Packages: []string{"com.netflix.ninja", "com.google.android.youtube.tv", "com.not.installed"}})
	return v1.Operation{ID: id, DeviceID: 7, Type: v1.OpClearAppData, Params: p, Attempt: attempt, ExpiresAt: expires}
}

func TestCleanupOperationVerifiedAndDeduplicatedAcrossRestart(t *testing.T) {
	h := newHarness(t)
	h.api.desired.Operations = []v1.Operation{clearOp("op-1", 1, h.now.Add(time.Hour))}
	h.api.reportErr = errors.New("network down") // result report is lost
	h.agent.Tick(context.Background())

	snap := h.tv.Snapshot()
	if snap.AppData["com.netflix.ninja"] || snap.AppData["com.google.android.youtube.tv"] {
		t.Fatal("app data not cleared")
	}
	clears := countPrefix(h.tv.Calls, "clear ")

	// Agent restarts (new process, same state dir). Server redelivers op-1.
	h.tv.SignIn("com.netflix.ninja") // next guest signs in
	h.api.reportErr = nil
	h.agent = h.newAgent()
	h.now = h.now.Add(time.Minute)
	h.agent.Tick(context.Background())

	if got := countPrefix(h.tv.Calls, "clear "); got != clears {
		t.Fatalf("operation re-executed after restart (%d -> %d clears)", clears, got)
	}
	if !h.tv.Snapshot().AppData["com.netflix.ninja"] {
		t.Fatal("redelivered cleanup wiped the next guest's session")
	}
	res := h.api.opResults["op-1"]
	if len(res) == 0 || res[0].Status != v1.OpStatusSucceeded || res[0].Verification["com.not.installed"] != "not_installed" {
		t.Fatalf("result not reported correctly: %+v", res)
	}
}

func TestExpiredOperationIsRejectedNotExecuted(t *testing.T) {
	h := newHarness(t)
	h.api.desired.Operations = []v1.Operation{clearOp("op-old", 1, h.now.Add(-time.Minute))}
	h.agent.Tick(context.Background())
	if countPrefix(h.tv.Calls, "clear ") != 0 {
		t.Fatal("expired op executed")
	}
	if r := h.api.opResults["op-old"]; len(r) != 1 || r[0].Status != v1.OpStatusRejected || r[0].Retryable {
		t.Fatalf("expected non-retryable rejection: %+v", r)
	}
}

func TestFailedOperationRetriedOnHigherAttempt(t *testing.T) {
	h := newHarness(t)
	h.tv.Reachable = false
	h.api.desired.Operations = []v1.Operation{clearOp("op-2", 1, h.now.Add(time.Hour))}
	h.agent.Tick(context.Background())
	if r := h.api.opResults["op-2"]; len(r) != 1 || r[0].Status != v1.OpStatusFailed || !r[0].Retryable {
		t.Fatalf("expected retryable failure: %+v", r)
	}

	h.tv.Reachable = true
	h.api.desired.Operations = []v1.Operation{clearOp("op-2", 2, h.now.Add(time.Hour))}
	h.now = h.now.Add(time.Minute)
	h.agent.Tick(context.Background())
	if r := h.api.opResults["op-2"]; len(r) != 2 || r[1].Status != v1.OpStatusSucceeded || r[1].Attempt != 2 {
		t.Fatalf("retry not executed: %+v", r)
	}
}

func TestOfflineReconcilesFromCacheWithoutOperations(t *testing.T) {
	h := newHarness(t)
	h.agent.Tick(context.Background())

	// Control plane unreachable; someone re-enables the stock launcher.
	h.api.pollErr = errors.New("dial tcp: no route to host")
	h.api.desired.Operations = []v1.Operation{clearOp("op-3", 1, h.now.Add(time.Hour))}
	_ = h.tv.SetPackageEnabled(context.Background(), tv.SimStockLauncher, true)

	h.agent = h.newAgent() // also covers restart while offline
	h.now = h.now.Add(time.Minute)
	wait := h.agent.Tick(context.Background())
	if !h.tv.Snapshot().Disabled[tv.SimStockLauncher] {
		t.Fatal("did not reconcile from cached desired state")
	}
	if countPrefix(h.tv.Calls, "clear ") != 0 {
		t.Fatal("executed an operation while offline")
	}
	if wait < 10*time.Second {
		t.Fatalf("offline poll should back off, got %v", wait)
	}
}

func TestRebootOperationSkipsReconcileThatTick(t *testing.T) {
	h := newHarness(t)
	h.api.desired.Operations = []v1.Operation{{ID: "op-r", DeviceID: 7, Type: v1.OpReboot, Attempt: 1, ExpiresAt: h.now.Add(time.Hour)}}
	h.agent.Tick(context.Background())
	if h.tv.Snapshot().Boots != 1 {
		t.Fatal("not rebooted")
	}
	if contains(h.tv.Calls, "install "+launcherPkg) {
		t.Fatal("reconciled a rebooting device")
	}
}

func TestUnknownOperationRejected(t *testing.T) {
	h := newHarness(t)
	h.api.desired.Operations = []v1.Operation{{ID: "op-x", DeviceID: 7, Type: "shell", Params: json.RawMessage(`{"cmd":"rm -rf /"}`), Attempt: 1}}
	h.agent.Tick(context.Background())
	if r := h.api.opResults["op-x"]; len(r) != 1 || r[0].Status != v1.OpStatusRejected {
		t.Fatalf("unknown op not rejected: %+v", r)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func countPrefix(s []string, p string) int {
	n := 0
	for _, x := range s {
		if strings.HasPrefix(x, p) {
			n++
		}
	}
	return n
}

func TestReportsDiscoveredTVs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	calls := 0
	seen := []v1.DiscoveredTV{{Key: "cast:1", Name: "Living Room TV", Address: "10.0.50.30", Services: []string{v1.ServiceCast}}}
	h.agent.Discover = func(context.Context) ([]v1.DiscoveredTV, error) {
		calls++
		if calls == 3 {
			return nil, errors.New("network is down")
		}
		return seen, nil
	}
	h.agent.DiscoverEvery = 5 * time.Minute
	ctx := context.Background()

	h.agent.Tick(ctx)
	h.now = h.now.Add(time.Minute)
	h.agent.Tick(ctx)
	if calls != 1 {
		t.Fatalf("browsed %d times within the interval", calls)
	}
	h.now = h.now.Add(5 * time.Minute)
	h.agent.Tick(ctx) // browses again
	h.now = h.now.Add(5 * time.Minute)
	h.agent.Tick(ctx) // browse fails: keep the last result
	if calls != 3 {
		t.Fatalf("browsed %d times", calls)
	}
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	for i, st := range h.api.statuses {
		if len(st.Discovered) != 1 || st.Discovered[0].Key != "cast:1" {
			t.Fatalf("report %d discovered: %+v", i, st.Discovered)
		}
	}
}
