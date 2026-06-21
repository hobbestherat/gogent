//go:build !windows

package shell

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecuteSuccess(t *testing.T) {
	res, err := Execute("printf 'hello'", ShellConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Stdout != "hello" {
		t.Fatalf("got stdout %q, want %q", res.Stdout, "hello")
	}
	if res.Timeout {
		t.Fatal("did not expect timeout")
	}
	if res.ExitCode != 0 {
		t.Fatalf("got exit code %d, want 0", res.ExitCode)
	}
}

func TestExecuteStderrAndFailure(t *testing.T) {
	res, err := Execute("printf 'oops' >&2; exit 3", ShellConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Stderr != "oops" {
		t.Fatalf("got stderr %q, want %q", res.Stderr, "oops")
	}
	if res.ExitCode != 1 {
		t.Fatalf("got exit code %d, want 1", res.ExitCode)
	}
	if res.Error == "" {
		t.Fatal("expected an error message for a failing command")
	}
}

func TestExecuteDir(t *testing.T) {
	dir := t.TempDir()
	res, err := Execute("pwd", ShellConfig{Timeout: 5 * time.Second, Dir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// macOS symlinks /tmp -> /private/tmp; just check the dir is reflected.
	got := strings.TrimSpace(res.Stdout)
	if !strings.HasSuffix(got, filepath.Base(dir)) {
		t.Fatalf("got pwd %q, want it to end with %q", got, filepath.Base(dir))
	}
}

func TestExecuteTimeout(t *testing.T) {
	res, err := Execute("printf 'before'; sleep 30", ShellConfig{Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Timeout {
		t.Fatal("expected Timeout to be true")
	}
	if res.Error == "" {
		t.Fatal("expected a timeout error message")
	}
	// Output written before the timeout should be captured without a data race.
	if res.Stdout != "before" {
		t.Fatalf("got stdout %q, want %q", res.Stdout, "before")
	}
}

// TestExecuteTimeoutKillsChildren verifies that a timeout kills the whole
// process group, not just the sh parent, so grandchildren do not orphan.
func TestExecuteTimeoutKillsChildren(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	// The shell backgrounds a long sleep and records its PID, then waits.
	// On timeout the entire process group must be killed.
	command := "sh -c 'echo $$ > " + pidFile + "; sleep 30' & sleep 30"
	res, err := Execute(command, ShellConfig{Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Timeout {
		t.Fatal("expected Timeout to be true")
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skipf("child pid file not written, cannot verify kill: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("bad pid file contents %q: %v", data, err)
	}

	// Give the kill a moment to propagate, then assert the child is gone.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			// ESRCH: no such process => child was killed. Good.
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild process %d still alive after timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
