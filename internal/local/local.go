// SPDX-License-Identifier: Apache-2.0

// Package local runs the agent without a control plane: desired state comes
// from a JSON file the operator edits, and observed state is written next to
// it. This is the self-hosted mode: everything the agent can do to a TV works
// with no account and no network beyond the TVs themselves.
package local

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/state"
)

// Config is the standalone desired-state file: the same device assignments a
// control plane would send, without operations.
//
//	{
//	  "devices": [
//	    {"device_id": 1, "name": "Living room", "adb_address": "10.0.50.21:5555", "generation": 1,
//	     "spec": {"launcher": {...}, "settings": [...]}}
//	  ]
//	}
//
// Bump "generation" when you change a spec so status.json shows when the TV
// has caught up.
type Config struct {
	PollIntervalSeconds int                   `json:"poll_interval_seconds,omitempty"`
	Devices             []v1.DeviceAssignment `json:"devices"`
}

// API implements agent.API over local files.
type API struct {
	// Path is the desired-state file.
	Path string
	// StatusDir receives status.json and operations.log.
	StatusDir state.Dir
}

func (l *API) DesiredState(ctx context.Context) (v1.DesiredStateResponse, error) {
	if err := ctx.Err(); err != nil {
		return v1.DesiredStateResponse{}, err
	}
	b, err := os.ReadFile(l.Path)
	if err != nil {
		return v1.DesiredStateResponse{}, fmt.Errorf("read desired state: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return v1.DesiredStateResponse{}, fmt.Errorf("parse %s: %w", filepath.Base(l.Path), err)
	}
	return v1.DesiredStateResponse{
		PollIntervalSeconds: cfg.PollIntervalSeconds,
		Devices:             cfg.Devices,
		Operations:          nil, // one-time operations need a control plane
		ServerTime:          time.Now(),
	}, nil
}

func (l *API) ReportStatus(ctx context.Context, r v1.StatusReport) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return l.StatusDir.Save("status.json", r)
}

func (l *API) ReportOperation(ctx context.Context, id string, r v1.OperationResult) error {
	return ctx.Err() // never called: standalone mode delivers no operations
}
