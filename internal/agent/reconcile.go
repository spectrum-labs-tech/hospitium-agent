// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/tv"
)

// Artifacts fetches and caches verified APKs.
type Artifacts interface {
	// Fetch returns a local path to an APK whose SHA-256 matches.
	Fetch(ctx context.Context, url, sha256 string) (string, error)
}

// reconcileDevice drives one TV toward spec. It observes first and only acts
// on differences, so a compliant TV sees read-only commands only.
func reconcileDevice(ctx context.Context, dev tv.Device, a v1.DeviceAssignment, art Artifacts, now time.Time) v1.DeviceStatus {
	st := v1.DeviceStatus{DeviceID: a.DeviceID, ReconciledAt: now}
	spec := a.Spec

	if err := dev.Connect(ctx); err != nil {
		st.Reachable = !errors.Is(err, tv.ErrUnreachable)
		st.Authorized = false
		st.LastError = err.Error()
		reason := "Unreachable"
		if errors.Is(err, tv.ErrUnauthorized) {
			reason = "ADBUnauthorized"
			err = fmt.Errorf("%w: accept the 'Allow debugging?' prompt on the TV with the remote", err)
			st.LastError = err.Error()
		}
		st.Conditions = append(st.Conditions, cond("Reachable", st.Reachable, reason, err.Error()))
		return st
	}
	st.Reachable, st.Authorized = true, true
	st.Conditions = append(st.Conditions, cond("Reachable", true, "Connected", ""))

	if info, err := dev.Info(ctx); err == nil {
		st.Model, st.Manufacturer, st.OSVersion, st.SDKLevel = info.Model, info.Manufacturer, info.OSVersion, info.SDKLevel
	}

	managed := managedPackages(spec)
	versions, err := dev.PackageVersions(ctx, managed)
	if err != nil {
		return fail(st, err)
	}

	var errs []string
	compliant := true

	// 1. Launcher package at the exact desired version.
	if l := spec.Launcher; l != nil {
		ok, err := ensurePackage(ctx, dev, art, l.PackageName, l.VersionCode, l.APKURL, l.SHA256, true, versions)
		if err != nil {
			errs = append(errs, err.Error())
		}
		st.Conditions = append(st.Conditions, cond("LauncherInstalled", ok, "", errString(err)))
		compliant = compliant && ok

		// 2. Launcher activation (only once installed).
		if ok {
			ok, err := ensureActivation(ctx, dev, l)
			if err != nil {
				errs = append(errs, err.Error())
			}
			st.Conditions = append(st.Conditions, cond("LauncherActive", ok, l.Activation, errString(err)))
			compliant = compliant && ok
		}
	}

	// 3. Required apps: present at >= min version; sideload only when an
	// artifact is provided (store apps cannot be sideloaded).
	missing := []string{}
	for _, app := range spec.RequiredApps {
		ok, err := ensurePackage(ctx, dev, art, app.PackageName, app.MinVersionCode, app.APKURL, app.SHA256, false, versions)
		if err != nil {
			errs = append(errs, err.Error())
		}
		if !ok {
			missing = append(missing, app.PackageName)
		}
	}
	if len(spec.RequiredApps) > 0 {
		st.Conditions = append(st.Conditions, cond("AppsPresent", len(missing) == 0, "", strings.Join(missing, ",")))
		compliant = compliant && len(missing) == 0
	}

	// 4. Settings.
	if len(spec.Settings) > 0 {
		ok := true
		for _, s := range spec.Settings {
			cur, err := dev.GetSetting(ctx, s.Namespace, s.Key)
			if err != nil {
				errs = append(errs, err.Error())
				ok = false
				continue
			}
			if cur == s.Value {
				continue
			}
			if err := dev.PutSetting(ctx, s.Namespace, s.Key, s.Value); err != nil {
				errs = append(errs, err.Error())
				ok = false
				continue
			}
			if got, _ := dev.GetSetting(ctx, s.Namespace, s.Key); got != s.Value {
				errs = append(errs, fmt.Sprintf("setting %s/%s did not stick (got %q)", s.Namespace, s.Key, got))
				ok = false
			}
		}
		st.Conditions = append(st.Conditions, cond("SettingsApplied", ok, "", ""))
		compliant = compliant && ok
	}

	if final, err := dev.PackageVersions(ctx, managed); err == nil {
		st.Packages = final
	}
	st.Compliant = compliant
	if len(errs) > 0 {
		st.LastError = strings.Join(errs, "; ")
	} else {
		// A clean pass means this generation was fully processed, even if a
		// store app is missing (that needs a human, not a retry).
		st.ObservedGeneration = a.Generation
	}
	return st
}

func managedPackages(spec v1.DeviceSpec) []string {
	set := map[string]bool{}
	if spec.Launcher != nil {
		set[spec.Launcher.PackageName] = true
		for _, p := range spec.Launcher.StockLaunchers {
			set[p] = true
		}
	}
	for _, a := range spec.RequiredApps {
		set[a.PackageName] = true
	}
	for _, p := range spec.CleanupPackages {
		set[p] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ErrNewerInstalled: the TV has a newer version than desired. The agent never
// downgrades: Android only allows it by uninstalling (or `install -d` on
// debuggable builds), and uninstalling deletes the app's data, including the
// launcher's one-time device token. Roll forward instead: publish the old
// build again with a higher versionCode.
var ErrNewerInstalled = errors.New("installed version is newer than desired; the agent never downgrades")

func newerInstalled(pkg string, cur, want int64) error {
	return fmt.Errorf("%s: v%d installed, v%d desired: %w (publish the wanted build with a versionCode above %d)", pkg, cur, want, ErrNewerInstalled, cur)
}

// ensurePackage returns ok=true when pkg is installed at the wanted version
// (exact when exact, otherwise >= want).
func ensurePackage(ctx context.Context, dev tv.Device, art Artifacts, pkg string, want int64, url, sum string, exact bool, versions map[string]int64) (bool, error) {
	cur, installed := versions[pkg]
	if installed && (cur == want || (!exact && cur >= want)) {
		return true, nil
	}
	if installed && cur > want {
		return false, newerInstalled(pkg, cur, want)
	}
	if url == "" {
		if !installed {
			return false, nil
		}
		return false, fmt.Errorf("%s version %d below required %d and no artifact provided", pkg, cur, want)
	}
	path, err := art.Fetch(ctx, url, sum)
	if err != nil {
		return false, fmt.Errorf("fetch %s: %w", pkg, err)
	}
	apk := tv.APK{Path: path, PackageName: pkg, VersionCode: want}
	err = dev.Install(ctx, apk)
	if errors.Is(err, tv.ErrDowngrade) {
		// The versionCode on the TV is newer than the artifact's, though the
		// desired state said otherwise (a mislabeled artifact).
		return false, newerInstalled(pkg, cur, want)
	}
	if err != nil {
		return false, fmt.Errorf("install %s v%d: %w", pkg, want, err)
	}
	// Verify.
	after, err := dev.PackageVersions(ctx, []string{pkg})
	if err != nil {
		return false, err
	}
	if got, ok := after[pkg]; !ok || (exact && got != want) || (!exact && got < want) {
		return false, fmt.Errorf("install %s: verification failed, device reports version %d, want %d", pkg, got, want)
	}
	versions[pkg] = after[pkg]
	return true, nil
}

func ensureActivation(ctx context.Context, dev tv.Device, l *v1.LauncherSpec) (bool, error) {
	home, err := dev.HomeActivity(ctx)
	if err != nil {
		return false, err
	}
	ours := strings.HasPrefix(home, l.PackageName+"/")
	switch l.Activation {
	case v1.ActivationNone, "":
		// Restore stock behavior: re-enable any stock launcher we disabled.
		disabled, err := dev.DisabledPackages(ctx)
		if err != nil {
			return false, err
		}
		for _, p := range l.StockLaunchers {
			if disabled[p] {
				if err := dev.SetPackageEnabled(ctx, p, true); err != nil {
					return false, err
				}
			}
		}
		return true, nil
	case v1.ActivationHomeActivity:
		if ours {
			return true, nil
		}
		comp := l.HomeActivity
		if comp == "" {
			comp = l.PackageName + "/.MainActivity"
		}
		if err := dev.SetHomeActivity(ctx, comp); err != nil {
			return false, fmt.Errorf("set-home-activity: %w", err)
		}
	case v1.ActivationDisableStock:
		if !ours {
			disabled, err := dev.DisabledPackages(ctx)
			if err != nil {
				return false, err
			}
			for _, p := range l.StockLaunchers {
				if !disabled[p] {
					if err := dev.SetPackageEnabled(ctx, p, false); err != nil {
						return false, err
					}
				}
			}
		} else {
			return true, nil
		}
	default:
		return false, fmt.Errorf("unknown activation strategy %q", l.Activation)
	}
	home, err = dev.HomeActivity(ctx)
	if err != nil {
		return false, err
	}
	if !strings.HasPrefix(home, l.PackageName+"/") {
		return false, fmt.Errorf("activation %q did not take effect: HOME resolves to %s", l.Activation, home)
	}
	return true, nil
}

func cond(t string, ok bool, reason, msg string) v1.Condition {
	s := "False"
	if ok {
		s = "True"
	}
	return v1.Condition{Type: t, Status: s, Reason: reason, Message: msg}
}

func fail(st v1.DeviceStatus, err error) v1.DeviceStatus {
	st.LastError = err.Error()
	if errors.Is(err, tv.ErrUnreachable) {
		st.Reachable = false
	}
	return st
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// FileArtifacts caches verified APKs under dir, keyed by digest.
type FileArtifacts struct {
	Dir      string
	Download func(ctx context.Context, url, sha256, dst string) error
}

func (f *FileArtifacts) Fetch(ctx context.Context, url, sum string) (string, error) {
	if sum == "" {
		return "", errors.New("refusing artifact without sha256")
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(f.Dir, strings.ToLower(sum)+".apk")
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := f.Download(ctx, url, sum, dst); err != nil {
		return "", err
	}
	return dst, nil
}
