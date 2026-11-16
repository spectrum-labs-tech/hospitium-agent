// SPDX-License-Identifier: Apache-2.0

// Package client is the agent's HTTPS client for the Hospitium control plane.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"
)

type Client struct {
	BaseURL   string
	Token     string
	UserAgent string
	HTTP      *http.Client
}

func New(baseURL, token, version string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Token:     token,
		UserAgent: "hospitium-agent/" + version,
		HTTP:      &http.Client{Timeout: 30 * time.Second},
	}
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("http %d: %s", e.Status, e.Body) }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Enroll(ctx context.Context, req v1.EnrollRequest) (v1.EnrollResponse, error) {
	var res v1.EnrollResponse
	err := c.do(ctx, http.MethodPost, v1.PathAgentEnroll, req, &res)
	return res, err
}

func (c *Client) DesiredState(ctx context.Context) (v1.DesiredStateResponse, error) {
	var res v1.DesiredStateResponse
	err := c.do(ctx, http.MethodGet, v1.PathAgentDesiredState, nil, &res)
	return res, err
}

func (c *Client) ReportStatus(ctx context.Context, r v1.StatusReport) error {
	return c.do(ctx, http.MethodPost, v1.PathAgentStatus, r, nil)
}

func (c *Client) ReportOperation(ctx context.Context, id string, r v1.OperationResult) error {
	return c.do(ctx, http.MethodPost, strings.Replace(v1.PathAgentOperationResult, "{id}", url.PathEscape(id), 1), r, nil)
}

// Download fetches an artifact to dst and verifies its SHA-256. The agent
// token is only sent when the artifact is hosted by the control plane itself.
func (c *Client) Download(ctx context.Context, rawURL, wantSHA256, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	if strings.HasPrefix(rawURL, c.BaseURL+"/") && c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := *c.HTTP
	hc.Timeout = 10 * time.Minute
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &HTTPError{Status: resp.StatusCode, Body: "download " + rawURL}
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp) //nolint:gosec // dst is chosen by the agent (artifact cache), not the server
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantSHA256) {
		_ = os.Remove(tmp)
		return fmt.Errorf("artifact integrity check failed: sha256 %s, want %s", got, wantSHA256)
	}
	return os.Rename(tmp, dst)
}

// Claim asks whether an owner has claimed this hub (protocol v0.4.0). It returns the credential
// once claimed (200), or pending with when to ask again (202).
func (c *Client) Claim(ctx context.Context, req v1.ClaimRequest) (v1.EnrollResponse, *v1.ClaimPending, error) {
	var res v1.EnrollResponse
	b, err := json.Marshal(req)
	if err != nil {
		return res, nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+v1.PathAgentClaim, bytes.NewReader(b))
	if err != nil {
		return res, nil, err
	}
	hr.Header.Set("User-Agent", c.UserAgent)
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(hr)
	if err != nil {
		return res, nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return res, nil, json.NewDecoder(resp.Body).Decode(&res)
	case http.StatusAccepted:
		var p v1.ClaimPending
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			return res, nil, err
		}
		return res, &p, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return res, nil, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
}
