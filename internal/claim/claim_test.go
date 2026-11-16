// SPDX-License-Identifier: Apache-2.0

package claim

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/state"
)

func TestSecretIsMadeOnceAndKept(t *testing.T) {
	t.Parallel()
	dir := state.Dir(t.TempDir())
	a, err := Secret(dir)
	if err != nil || len(a) != v1.ClaimSecretBytes {
		t.Fatalf("secret: %v %v", a, err)
	}
	b, err := Secret(dir)
	if err != nil || string(a) != string(b) {
		t.Fatal("the secret must survive restarts, or the label's code stops working")
	}
	code, err := Code(dir)
	if err != nil || code != v1.ClaimCode(a) {
		t.Fatalf("code: %q %v", code, err)
	}

	// A reset (deleting the file) means a new secret and a new code.
	if err := os.Remove(dir.Path(SecretFile)); err != nil {
		t.Fatal(err)
	}
	if c, _ := Secret(dir); string(c) == string(a) {
		t.Fatal("reset kept the old secret")
	}

	if err := os.WriteFile(dir.Path(SecretFile), []byte(`{"secret":"bad"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Secret(dir); err == nil {
		t.Fatal("a damaged secret file should be reported, not silently replaced")
	}
}

type fakeAPI struct {
	answers []func() (v1.EnrollResponse, *v1.ClaimPending, error)
	got     []v1.ClaimRequest
}

func (f *fakeAPI) Claim(_ context.Context, req v1.ClaimRequest) (v1.EnrollResponse, *v1.ClaimPending, error) {
	f.got = append(f.got, req)
	next := f.answers[0]
	if len(f.answers) > 1 {
		f.answers = f.answers[1:]
	}
	return next()
}

func TestWaitPollsUntilClaimed(t *testing.T) {
	t.Parallel()
	pending := func() (v1.EnrollResponse, *v1.ClaimPending, error) {
		return v1.EnrollResponse{}, &v1.ClaimPending{Status: "waiting", RetryAfterSeconds: 30}, nil
	}
	api := &fakeAPI{answers: []func() (v1.EnrollResponse, *v1.ClaimPending, error){
		pending,
		func() (v1.EnrollResponse, *v1.ClaimPending, error) {
			return v1.EnrollResponse{}, nil, errors.New("connection refused")
		},
		pending,
		func() (v1.EnrollResponse, *v1.ClaimPending, error) {
			return v1.EnrollResponse{ControllerID: 9, AgentToken: "hsa_x", PropertyID: 3}, nil, nil
		},
	}}
	var waits []time.Duration
	secret := make([]byte, v1.ClaimSecretBytes)
	res, err := Wait(context.Background(), api, secret, v1.ClaimRequest{Hostname: "hub"}, slog.New(slog.DiscardHandler),
		func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil })
	if err != nil || res.AgentToken != "hsa_x" || res.ControllerID != 9 {
		t.Fatalf("result: %+v %v", res, err)
	}
	if want := []time.Duration{30 * time.Second, 5 * time.Second, 30 * time.Second}; len(waits) != 3 || waits[0] != want[0] || waits[1] != want[1] || waits[2] != want[2] {
		t.Fatalf("waits: %v, want %v", waits, want)
	}
	if got := api.got[0]; got.ClaimSecret != base64.RawURLEncoding.EncodeToString(secret) || got.Hostname != "hub" {
		t.Fatalf("request: %+v", got)
	}
}

func TestWaitStopsWithItsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	api := &fakeAPI{answers: []func() (v1.EnrollResponse, *v1.ClaimPending, error){
		func() (v1.EnrollResponse, *v1.ClaimPending, error) {
			return v1.EnrollResponse{}, &v1.ClaimPending{RetryAfterSeconds: 3600}, nil
		},
	}}
	_, err := Wait(ctx, api, make([]byte, v1.ClaimSecretBytes), v1.ClaimRequest{}, slog.New(slog.DiscardHandler),
		func(ctx context.Context, d time.Duration) error {
			if d != maxWait {
				t.Errorf("an hour's retry-after should be capped at %v, got %v", maxWait, d)
			}
			cancel()
			return ctx.Err()
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
