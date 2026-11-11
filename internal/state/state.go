// SPDX-License-Identifier: Apache-2.0

// Package state holds the agent's minimal durable state as small JSON files
// written atomically (write temp, fsync, rename). No database is needed.
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Dir is the agent state directory, typically /var/lib/hospitium-agent.
type Dir string

func (d Dir) Path(name string) string { return filepath.Join(string(d), name) }

// Load reads name into v. It returns (false, nil) if the file does not exist.
func (d Dir) Load(name string, v any) (bool, error) {
	b, err := os.ReadFile(d.Path(name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

// Save atomically replaces name with the JSON encoding of v.
func (d Dir) Save(name string, v any) error {
	if err := os.MkdirAll(string(d), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(string(d), "."+name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil && !isWindows() {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), d.Path(name))
}

func isWindows() bool { return os.PathSeparator == '\\' }

// Credentials are written by `hospitium-agent enroll`.
type Credentials struct {
	ServerURL    string `json:"server_url"`
	ControllerID int64  `json:"controller_id"`
	PropertyID   int64  `json:"property_id"`
	AgentToken   string `json:"agent_token"`
}

const CredentialsFile = "credentials.json"
