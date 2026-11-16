// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/check"
)

func TestCheckRefusesChangesWithoutYes(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--tv", "10.0.50.21:5555", "--simulate", "--reboots", "1"},
		{"--tv", "10.0.50.21:5555", "--simulate", "--clear", "com.netflix.ninja"},
		{"--tv", "10.0.50.21:5555", "--simulate", "--launcher-apk", "x.apk"},
	} {
		var out bytes.Buffer
		err := checkTV(context.Background(), args, &out)
		if err == nil || !strings.Contains(err.Error(), "--yes") {
			t.Errorf("%v: want a refusal naming --yes, got %v", args, err)
		}
		if out.Len() != 0 {
			t.Errorf("%v: nothing should run before confirmation", args)
		}
	}
}

func TestCheckReadOnlyNeedsNoConfirmation(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := checkTV(context.Background(), []string{"--tv", "10.0.50.21:5555", "--simulate"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "SIMULATED TV") || !strings.Contains(out.String(), "PASS  connect") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestCheckWritesReport(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "report.json")
	var out bytes.Buffer
	err := checkTV(context.Background(), []string{"--tv", "10.0.50.21:5555", "--simulate", "--yes",
		"--launcher-apk", "launcher.apk", "--reboots", "1", "--clear", "com.netflix.ninja", "--out", path, "--poll", "1ms"}, &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rep check.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Passed() || len(rep.Observations) < 5 || rep.Facts == nil {
		t.Fatalf("report: %+v", rep)
	}
	if !strings.Contains(out.String(), "Still to check by hand") {
		t.Fatalf("the manual checklist should be printed:\n%s", out.String())
	}
}

func TestCheckRejectsTooManyReboots(t *testing.T) {
	t.Parallel()
	err := checkTV(context.Background(), []string{"--tv", "x:5555", "--simulate", "--reboots", "50", "--yes"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "between 0 and 10") {
		t.Fatalf("got %v", err)
	}
}

func TestClaimCodePrintsAStableCode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var a, b bytes.Buffer
	if err := claimCode([]string{"--state-dir", dir}, &a); err != nil {
		t.Fatal(err)
	}
	if err := claimCode([]string{"--state-dir", dir}, &b); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.String(), "Claim code: ") || a.String() != b.String() {
		t.Fatalf("output:\n%s\n%s", a.String(), b.String())
	}
}
