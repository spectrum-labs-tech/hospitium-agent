// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadVerifiesDigestAndScopesToken(t *testing.T) {
	body := []byte("fake apk bytes")
	sum := sha256.Sum256(body)
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := New(srv.URL, "secret", "test")
	dst := filepath.Join(dir, "a.apk")
	if err := c.Download(context.Background(), srv.URL+"/v1/artifacts/x", hex.EncodeToString(sum[:]), dst); err != nil {
		t.Fatal(err)
	}
	if sawAuth != "Bearer secret" {
		t.Fatalf("token not sent to control plane: %q", sawAuth)
	}

	bad := filepath.Join(dir, "b.apk")
	err := c.Download(context.Background(), srv.URL+"/v1/artifacts/x", strings.Repeat("0", 64), bad)
	if err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("expected integrity failure, got %v", err)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatal("unverified artifact left on disk")
	}

	// Third-party host must not receive the agent token.
	c.BaseURL = "https://hospitium.example"
	_ = c.Download(context.Background(), srv.URL+"/x", hex.EncodeToString(sum[:]), filepath.Join(dir, "c.apk"))
	if sawAuth != "" {
		t.Fatalf("token leaked to third-party host: %q", sawAuth)
	}
}
