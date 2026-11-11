// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/state"
)

func TestExampleConfigLoadsAndStatusIsWritten(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	api := &API{Path: filepath.Join("..", "..", "examples", "desired.json"), StatusDir: state.Dir(dir)}
	ds, err := api.DesiredState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Devices) != 1 || ds.Devices[0].Spec.Launcher == nil || len(ds.Operations) != 0 {
		t.Fatalf("unexpected desired state: %+v", ds)
	}
	if err := api.ReportStatus(context.Background(), v1.StatusReport{AgentVersion: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "status.json")); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledContextStopsWork(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	api := &API{Path: "unused", StatusDir: state.Dir(t.TempDir())}
	if _, err := api.DesiredState(ctx); err == nil {
		t.Fatal("expected context error")
	}
}
