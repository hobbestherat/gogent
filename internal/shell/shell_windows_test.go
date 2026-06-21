//go:build windows

package shell

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecuteSuccessWindows(t *testing.T) {
	res, err := Execute("echo hello", ShellConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// cmd's echo appends CRLF; match leniently.
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Fatalf("got stdout %q, want %q", res.Stdout, "hello")
	}
	if res.Timeout {
		t.Fatal("did not expect timeout")
	}
	if res.ExitCode != 0 {
		t.Fatalf("got exit code %d, want 0", res.ExitCode)
	}
}

func TestExecuteFailureWindows(t *testing.T) {
	res, err := Execute("exit 3", ShellConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 1 {
		t.Fatalf("got exit code %d, want 1", res.ExitCode)
	}
	if res.Error == "" {
		t.Fatal("expected an error message for a failing command")
	}
}

func TestExecuteDirWindows(t *testing.T) {
	dir := t.TempDir()
	// `cd` with no args prints the current directory on Windows.
	res, err := Execute("cd", ShellConfig{Timeout: 5 * time.Second, Dir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := strings.TrimSpace(res.Stdout)
	if !strings.HasSuffix(got, filepath.Base(dir)) {
		t.Fatalf("got cwd %q, want it to end with %q", got, filepath.Base(dir))
	}
}

func TestExecuteTimeoutWindows(t *testing.T) {
	// `ping -n 31` blocks ~30s; the timeout must fire and cancel the command.
	res, err := Execute("ping -n 31 127.0.0.1 >NUL", ShellConfig{Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Timeout {
		t.Fatal("expected Timeout to be true")
	}
	if res.Error == "" {
		t.Fatal("expected a timeout error message")
	}
}

