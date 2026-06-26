package main

// Command-level tests for the ssh:// attach path in runAttached (issue #482).
// The ssh:// branch fails fast (connect/auth) BEFORE any UI is built, so it can
// be exercised headlessly: runAttached returns the wrapped "ssh connect" error
// instead of ever standing up the TUI. This covers the design's "unreachable
// host → fail fast" requirement at the real seam (criterion #2) and confirms the
// ssh:// branch is taken (criterion #1).

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestRSAKey(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	p := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return p
}

// TestRunAttached_SSHUnreachableHostFailsFast: an ssh:// connect to a reserved
// (.invalid) host must return a wrapped, actionable error promptly — never
// reaching the UI, never hanging on the OS TCP timeout.
func TestRunAttached_SSHUnreachableHostFailsFast(t *testing.T) {
	// Determinism: no real agent, a generated key so auth is satisfied, then the
	// dial to a reserved-invalid host fails fast (NXDOMAIN). HOME is pointed at a
	// temp dir so the real ~/.ssh is never consulted for defaults.
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("HOME", t.TempDir())
	savedKey := *sshKey
	*sshKey = writeTestRSAKey(t)
	defer func() { *sshKey = savedKey }()

	start := time.Now()
	err := runAttached(t.TempDir(), "ssh://nonexistent.invalid", "", false)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("runAttached ssh:// to an unreachable host must fail fast, got nil")
	}
	if !strings.Contains(err.Error(), "ssh connect") {
		t.Fatalf("error should be the wrapped ssh-connect failure, got: %v", err)
	}
	// Must be bounded — well under the ~75s OS TCP timeout the design replaced.
	if elapsed > 8*time.Second {
		t.Fatalf("ssh connect should fail fast, took %v", elapsed)
	}
}

// TestRunAttached_SSHBadURLFailsFast: a malformed ssh:// value is rejected before
// any network I/O (the URL is validated in ParseConnectURL, called from
// runAttached before New).
func TestRunAttached_SSHBadURLFailsFast(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	err := runAttached(t.TempDir(), "ssh://", "", false)
	if err == nil {
		t.Fatal("runAttached with a malformed ssh:// URL must fail")
	}
	if !strings.Contains(err.Error(), "bad --connect") {
		t.Fatalf("error should be the bad-URL wrap, got: %v", err)
	}
}
