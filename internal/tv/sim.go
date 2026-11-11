// SPDX-License-Identifier: Apache-2.0

package tv

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// SimTV is an in-memory Google TV used by tests and `--simulate` mode.
// Behavior is modeled on documented Android behavior, not verified hardware
// quirks; anything observed only in simulation must be labeled as such.
type SimTV struct {
	mu sync.Mutex
	SimState
	// Fault injection (not persisted).
	FailInstalls     int  // fail the next N installs
	OfflineConnects  int  // report unreachable for the next N connects (e.g. reboot)
	RejectDowngrade  bool // behave like a non-debuggable app on a user build
	IgnoreHomeSetter bool // OEM that silently ignores set-home-activity
	Calls            []string
}

// SimState is the persisted part of a simulated TV.
type SimState struct {
	Addr         string            `json:"addr"`
	Reachable    bool              `json:"reachable"`
	Authorized   bool              `json:"authorized"`
	Model        string            `json:"model"`
	Packages     map[string]int64  `json:"packages"`
	Disabled     map[string]bool   `json:"disabled"`
	Settings     map[string]string `json:"settings"`
	HomeOverride string            `json:"home_override"`
	AppData      map[string]bool   `json:"app_data"` // package -> has signed-in data
	Boots        int               `json:"boots"`
}

// Stock Google TV launcher (Chromecast with Google TV / Google TV Streamer).
const SimStockLauncher = "com.google.android.apps.tv.launcherx"

func NewSimTV(addr string) *SimTV {
	return &SimTV{SimState: SimState{
		Addr: addr, Reachable: true, Authorized: true, Model: "Simulated Google TV",
		Packages: map[string]int64{
			SimStockLauncher:                    1,
			"com.netflix.ninja":                 1,
			"com.google.android.youtube.tv":     1,
			"com.disney.disneyplus":             1,
			"com.amazon.amazonvideo.livingroom": 1,
			"com.hulu.livingroomplus":           1,
		},
		Disabled: map[string]bool{},
		Settings: map[string]string{},
		AppData:  map[string]bool{"com.netflix.ninja": true, "com.google.android.youtube.tv": true},
	}}
}

func (s *SimTV) call(name string) error {
	s.Calls = append(s.Calls, name)
	if !s.Reachable {
		return ErrUnreachable
	}
	if !s.Authorized {
		return ErrUnauthorized
	}
	return nil
}

func (s *SimTV) Address() string { return s.Addr }

func (s *SimTV) Connect(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.OfflineConnects > 0 {
		s.OfflineConnects--
		s.Calls = append(s.Calls, "connect")
		return ErrUnreachable
	}
	return s.call("connect")
}

func (s *SimTV) Info(ctx context.Context) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("info"); err != nil {
		return Info{}, err
	}
	return Info{Model: s.Model, Manufacturer: "Simulator", OSVersion: "12", SDKLevel: 31, Fingerprint: "sim/sim/sim:12"}, nil
}

func (s *SimTV) PackageVersions(ctx context.Context, pkgs []string) (map[string]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("versions"); err != nil {
		return nil, err
	}
	res := map[string]int64{}
	for _, p := range pkgs {
		if v, ok := s.Packages[p]; ok {
			res[p] = v
		}
	}
	return res, nil
}

func (s *SimTV) DisabledPackages(ctx context.Context) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("disabled"); err != nil {
		return nil, err
	}
	res := map[string]bool{}
	for k, v := range s.Disabled {
		if v {
			res[k] = true
		}
	}
	return res, nil
}

func (s *SimTV) Install(ctx context.Context, apk APK, allowDowngrade bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("install " + apk.PackageName); err != nil {
		return err
	}
	if s.FailInstalls > 0 {
		s.FailInstalls--
		return fmt.Errorf("install %s: Failure [INSTALL_FAILED_INTERNAL_ERROR] (simulated)", apk.PackageName)
	}
	if cur, ok := s.Packages[apk.PackageName]; ok && cur > apk.VersionCode && (!allowDowngrade || s.RejectDowngrade) {
		return ErrDowngrade
	}
	s.Packages[apk.PackageName] = apk.VersionCode
	return nil
}

func (s *SimTV) Uninstall(ctx context.Context, pkg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("uninstall " + pkg); err != nil {
		return err
	}
	delete(s.Packages, pkg)
	if s.HomeOverride != "" && pkgOf(s.HomeOverride) == pkg {
		s.HomeOverride = ""
	}
	return nil
}

func (s *SimTV) GetSetting(ctx context.Context, ns, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("settings get"); err != nil {
		return "", err
	}
	v, ok := s.Settings[ns+"/"+key]
	if !ok {
		return "null", nil
	}
	return v, nil
}

func (s *SimTV) PutSetting(ctx context.Context, ns, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("settings put " + key); err != nil {
		return err
	}
	s.Settings[ns+"/"+key] = value
	return nil
}

func pkgOf(component string) string {
	for i, c := range component {
		if c == '/' {
			return component[:i]
		}
	}
	return component
}

func (s *SimTV) HomeActivity(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("home"); err != nil {
		return "", err
	}
	if s.HomeOverride != "" {
		if _, ok := s.Packages[pkgOf(s.HomeOverride)]; ok && !s.Disabled[pkgOf(s.HomeOverride)] {
			return s.HomeOverride, nil
		}
	}
	if _, ok := s.Packages[SimStockLauncher]; ok && !s.Disabled[SimStockLauncher] {
		return SimStockLauncher + "/.MainActivity", nil
	}
	// Stock launcher disabled: the only other HOME handler wins.
	for p := range s.Packages {
		if p != SimStockLauncher && !s.Disabled[p] && isLauncherPkg(p) {
			return p + "/.MainActivity", nil
		}
	}
	return "android/com.android.internal.app.ResolverActivity", nil
}

// isLauncherPkg: in the simulator, any package with "launcher" in its name
// declares a HOME intent filter.
func isLauncherPkg(p string) bool {
	for i := 0; i+8 <= len(p); i++ {
		if p[i:i+8] == "launcher" {
			return true
		}
	}
	return false
}

func (s *SimTV) SetHomeActivity(ctx context.Context, component string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("set-home " + component); err != nil {
		return err
	}
	if !s.IgnoreHomeSetter {
		s.HomeOverride = component
	}
	return nil
}

func (s *SimTV) SetPackageEnabled(ctx context.Context, pkg string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call(fmt.Sprintf("enable %s=%v", pkg, enabled)); err != nil {
		return err
	}
	s.Disabled[pkg] = !enabled
	return nil
}

func (s *SimTV) ClearAppData(ctx context.Context, pkg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("clear " + pkg); err != nil {
		return err
	}
	if _, ok := s.Packages[pkg]; !ok {
		return fmt.Errorf("pm clear %s: Failed (package not installed)", pkg)
	}
	delete(s.AppData, pkg)
	return nil
}

func (s *SimTV) VerifyCleared(ctx context.Context, pkg string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("verify " + pkg); err != nil {
		return nil, err
	}
	ev := map[string]string{"method": "simulated", "simulated": "true", "app_data_present": fmt.Sprint(s.AppData[pkg])}
	if s.AppData[pkg] {
		return ev, fmt.Errorf("%s data still present", pkg)
	}
	return ev, nil
}

func (s *SimTV) Reboot(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("reboot"); err != nil {
		return err
	}
	s.Boots++
	s.OfflineConnects = 1
	return nil
}

func (s *SimTV) Diagnostics(ctx context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.call("diagnostics"); err != nil {
		return nil, err
	}
	return map[string]string{"simulated": "true", "boots": fmt.Sprint(s.Boots)}, nil
}

// SignIn marks a streaming app as holding a guest session (test helper).
func (s *SimTV) SignIn(pkg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AppData[pkg] = true
}

func (s *SimTV) Snapshot() SimState {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.SimState)
	var c SimState
	_ = json.Unmarshal(b, &c)
	return c
}

// SimFleet hands out simulated TVs by address and optionally persists their
// state to a JSON file so `--simulate` survives agent restarts.
type SimFleet struct {
	mu   sync.Mutex
	TVs  map[string]*SimTV
	path string
}

func NewSimFleet(path string) *SimFleet {
	f := &SimFleet{TVs: map[string]*SimTV{}, path: path}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil { //nolint:gosec // path is the agent state dir
			var states map[string]SimState
			if json.Unmarshal(b, &states) == nil {
				for k, st := range states {
					f.TVs[k] = &SimTV{SimState: st}
				}
			}
		}
	}
	return f
}

func (f *SimFleet) Dialer() Dialer {
	return func(addr string) Device { return f.Get(addr) }
}

func (f *SimFleet) Get(addr string) *SimTV {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.TVs[addr]
	if !ok {
		t = NewSimTV(addr)
		f.TVs[addr] = t
	}
	return t
}

// Save persists all simulated TVs.
func (f *SimFleet) Save() error {
	if f.path == "" {
		return nil
	}
	f.mu.Lock()
	states := map[string]SimState{}
	for k, t := range f.TVs {
		states[k] = t.Snapshot()
	}
	f.mu.Unlock()
	b, err := json.MarshalIndent(states, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(f.path, b, 0o600)
}
