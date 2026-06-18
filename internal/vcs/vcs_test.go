package vcs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// initRepo creates a fresh git repository in a temp dir with a deterministic
// identity, skipping the test if git is unavailable.
func initRepo(t *testing.T) string {
	t.Helper()
	if !Available() {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		res, err := Run(dir, DefaultTimeout, args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		if !res.OK() {
			t.Fatalf("git %v failed: exit=%d stderr=%s", args, res.ExitCode, res.Stderr)
		}
	}
	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestIsRepo(t *testing.T) {
	if !Available() {
		t.Skip("git not installed")
	}
	repo := initRepo(t)
	if !IsRepo(repo) {
		t.Errorf("IsRepo(%q) = false, want true", repo)
	}

	plain := t.TempDir()
	if IsRepo(plain) {
		t.Errorf("IsRepo(%q) = true, want false", plain)
	}
}

func TestRunReportsExitCode(t *testing.T) {
	repo := initRepo(t)
	// A bogus subcommand exits non-zero but is not a launch error.
	res, err := Run(repo, DefaultTimeout, "not-a-real-subcommand")
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (non-zero exit, not launch failure)", err)
	}
	if res.OK() {
		t.Errorf("OK() = true, want false for failed subcommand")
	}
	if res.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want non-zero")
	}
}

func TestCommitAndLog(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "a.txt", "hello\n")

	if res, _ := Run(repo, DefaultTimeout, "add", "a.txt"); !res.OK() {
		t.Fatalf("add failed: %s", res.Stderr)
	}
	res, _ := Run(repo, DefaultTimeout, "commit", "-m", "feat: add a")
	if !res.OK() {
		t.Fatalf("commit failed: exit=%d stderr=%s", res.ExitCode, res.Stderr)
	}

	log, _ := Run(repo, DefaultTimeout, "log", "--oneline")
	if !strings.Contains(log.Stdout, "feat: add a") {
		t.Errorf("log missing commit, got: %q", log.Stdout)
	}
}

func TestStatusSummary(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "untracked.txt", "x\n")

	summary := StatusSummary(repo)
	if summary == "" {
		t.Fatal("StatusSummary returned empty for a repo with changes")
	}
	if !strings.Contains(summary, "untracked.txt") {
		t.Errorf("summary missing untracked file, got: %q", summary)
	}

	// Not a repo -> empty.
	if s := StatusSummary(t.TempDir()); s != "" {
		t.Errorf("StatusSummary(non-repo) = %q, want empty", s)
	}
}

func TestRunTimeout(t *testing.T) {
	if !Available() {
		t.Skip("git not installed")
	}
	repo := initRepo(t)
	// A context that is already expired before exec starts must be reported via
	// the Timeout flag, not as a Go error.
	res, err := Run(repo, 1*time.Nanosecond, "status")
	if err != nil {
		t.Fatalf("Run() error = %v, want nil on timeout", err)
	}
	if !res.Timeout {
		t.Errorf("Timeout = false, want true for an already-expired deadline")
	}
}
