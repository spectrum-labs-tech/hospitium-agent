// SPDX-License-Identifier: Apache-2.0

// Package claim lets a hub join its owner's account without anyone pasting a token on it. The hub
// generates a secret once; its claim code, printed on the label, is a one-way hash of that secret
// (protocol v0.4.0). Until an owner claims the code, the hub waits; then it receives its agent
// credential.
package claim

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/state"
)

// SecretFile is where the claim secret is kept, in the state directory. Deleting it (a reset)
// gives the hub a new secret and so a new code.
const SecretFile = "claim-secret.json"

type secretFile struct {
	Secret string `json:"secret"` // base64url, no padding
}

// Secret returns the hub's claim secret, creating it on first use.
func Secret(dir state.Dir) ([]byte, error) {
	var f secretFile
	ok, err := dir.Load(SecretFile, &f)
	if err != nil {
		return nil, err
	}
	if ok {
		b, err := base64.RawURLEncoding.DecodeString(f.Secret)
		if err != nil || len(b) != v1.ClaimSecretBytes {
			return nil, fmt.Errorf("%s is damaged; delete it to make a new secret (the hub then has a new claim code)", dir.Path(SecretFile))
		}
		return b, nil
	}
	b := make([]byte, v1.ClaimSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := dir.Save(SecretFile, secretFile{Secret: base64.RawURLEncoding.EncodeToString(b)}); err != nil {
		return nil, err
	}
	return b, nil
}

// Code is the hub's claim code, for its label.
func Code(dir state.Dir) (string, error) {
	s, err := Secret(dir)
	if err != nil {
		return "", err
	}
	return v1.ClaimCode(s), nil
}

// API is the control plane's claim endpoint.
type API interface {
	Claim(ctx context.Context, req v1.ClaimRequest) (v1.EnrollResponse, *v1.ClaimPending, error)
}

const (
	minWait = 5 * time.Second
	maxWait = 5 * time.Minute
)

// Wait polls until an owner claims the hub, then returns its credential. It waits as long as the
// control plane says between polls (bounded), backs off on errors, and stops when ctx ends.
func Wait(ctx context.Context, api API, secret []byte, info v1.ClaimRequest, log *slog.Logger,
	sleep func(context.Context, time.Duration) error,
) (v1.EnrollResponse, error) {
	info.ClaimSecret = base64.RawURLEncoding.EncodeToString(secret)
	code := v1.ClaimCode(secret)
	backoff := minWait
	logged := false
	for {
		res, pending, err := api.Claim(ctx, info)
		var wait time.Duration
		switch {
		case err == nil && pending == nil:
			return res, nil
		case err == nil:
			if !logged {
				log.InfoContext(ctx, "waiting to be claimed: enter this hub's code in Hospitium", "claim_code", code)
				logged = true
			}
			wait = time.Duration(pending.RetryAfterSeconds) * time.Second
			backoff = minWait
		default:
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			log.WarnContext(ctx, "claim check failed; retrying", "err", err, "retry_in", backoff)
			wait = backoff
			backoff = min(backoff*2, maxWait)
		}
		wait = max(min(wait, maxWait), minWait)
		if err := sleep(ctx, wait); err != nil {
			return res, err
		}
	}
}

// Sleep waits for d or until ctx ends.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ErrNoServer: an unenrolled hub was started without the control plane's address.
var ErrNoServer = errors.New("not enrolled and no control plane address: set --server or HOSPITIUM_SERVER " +
	"(the hub then waits to be claimed by its code), or run `hospitium-agent enroll`, or use --local")
