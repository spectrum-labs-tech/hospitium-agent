// SPDX-License-Identifier: Apache-2.0

// Package tv abstracts the operations the agent performs on a managed TV.
// The production implementation drives the `adb` binary; the simulator is
// used for tests and for demos without hardware.
package tv

import (
	"context"
	"errors"
)

var (
	// ErrUnreachable: the TV did not answer on its ADB address (off, rebooting,
	// network down, or wireless debugging disabled).
	ErrUnreachable = errors.New("tv unreachable")
	// ErrUnauthorized: the TV answered but has not accepted this agent's ADB
	// key. Someone must accept the prompt on screen with the remote.
	ErrUnauthorized = errors.New("adb key not authorized on tv")
	// ErrDowngrade: install refused because the installed version is newer.
	ErrDowngrade = errors.New("install refused: version downgrade")
)

type Info struct {
	Model        string
	Manufacturer string
	OSVersion    string
	SDKLevel     int
	Fingerprint  string
}

// APK describes an artifact to install. PackageName/VersionCode are hints
// used by the simulator and for logging; real devices read them from the APK.
type APK struct {
	Path        string
	PackageName string
	VersionCode int64
}

// Device is the narrow, closed set of actions the agent may take on a TV.
// There is intentionally no generic Shell method on this interface.
type Device interface {
	Address() string
	Connect(ctx context.Context) error
	Info(ctx context.Context) (Info, error)
	// PackageVersions returns versionCode for each installed package in pkgs.
	// Missing packages are absent from the map.
	PackageVersions(ctx context.Context, pkgs []string) (map[string]int64, error)
	DisabledPackages(ctx context.Context) (map[string]bool, error)
	Install(ctx context.Context, apk APK, allowDowngrade bool) error
	Uninstall(ctx context.Context, pkg string) error
	GetSetting(ctx context.Context, namespace, key string) (string, error)
	PutSetting(ctx context.Context, namespace, key, value string) error
	// HomeActivity returns the component that currently resolves for HOME.
	HomeActivity(ctx context.Context) (string, error)
	SetHomeActivity(ctx context.Context, component string) error
	SetPackageEnabled(ctx context.Context, pkg string, enabled bool) error
	ClearAppData(ctx context.Context, pkg string) error
	// VerifyCleared gathers evidence that pkg's local data was cleared.
	VerifyCleared(ctx context.Context, pkg string) (map[string]string, error)
	Reboot(ctx context.Context) error
	Diagnostics(ctx context.Context) (map[string]string, error)
}

// Dialer returns a Device handle for an ADB address.
type Dialer func(address string) Device
