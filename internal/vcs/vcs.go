// Package vcs provides a thin, safe wrapper around the system git binary. It
// executes git with explicit argument vectors (never a shell string, so there
// is no shell-injection surface), bounds every call with a timeout, and
// disables git's interactive affordances (pager, credential/terminal prompts)
// so a call can never block waiting for a human. It backs the native `git` tool
// and the git-status section injected into the system prompt.
package vcs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout bounds a git invocation when the caller passes a non-positive
// timeout. Git plumbing is fast; this is a guard against a wedged hook or pager.
const DefaultTimeout = 30 * time.Second

// maxStatusBytes caps the status summary injected into the system prompt so a
// huge working tree cannot blow the context budget.
const maxStatusBytes = 4 * 1024

// Result is the outcome of a single git invocation.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Timeout  bool
}

// OK reports whether the command ran to completion with a zero exit code.
func (r *Result) OK() bool { return r != nil && !r.Timeout && r.ExitCode == 0 }

// Run executes `git <args...>` in dir and returns its result. A non-zero git
// exit (e.g. "nothing to commit") is reported via Result.ExitCode, not as a Go
// error; an error is returned only when git cannot be launched at all (e.g. not
// installed). dir empty uses the process working directory.
func Run(dir string, timeout time.Duration, args ...string) (*Result, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // launches the trusted local git binary with an explicit argument vector
	if dir != "" {
		cmd.Dir = dir
	}
	// Strip git's interactive behaviour so a call can never hang on a pager, an
	// editor, or a credential/terminal prompt waiting for a human.
	cmd.Env = append(os.Environ(),
		"GIT_PAGER=cat",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := &Result{Stdout: stdout.String(), Stderr: stderr.String()}

	if ctx.Err() == context.DeadlineExceeded {
		res.Timeout = true
		return res, nil
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		// git could not be launched (not installed, not executable, ...).
		return res, fmt.Errorf("run git: %w", err)
	}
	return res, nil
}

// Available reports whether the git binary can be found on PATH.
func Available() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// IsRepo reports whether dir lies inside a git working tree.
func IsRepo(dir string) bool {
	res, err := Run(dir, 5*time.Second, "rev-parse", "--is-inside-work-tree")
	return err == nil && res.OK() && strings.TrimSpace(res.Stdout) == "true"
}

// StatusSummary returns a concise `git status` (branch + porcelain entries)
// suitable for the system prompt, or "" when dir is not a repo or git is
// unavailable. The output is size-capped.
func StatusSummary(dir string) string {
	res, err := Run(dir, 5*time.Second, "status", "--short", "--branch")
	if err != nil || !res.OK() {
		return ""
	}
	out := strings.TrimRight(res.Stdout, "\n")
	if len(out) > maxStatusBytes {
		out = out[:maxStatusBytes] + "\n… (status truncated)"
	}
	return out
}
