package main

// Issue #571 — embedded wiring of the `!`-prefixed shell affordance. The embedded
// handler set (cmd/embedded_handlers.go) must wire OnShell to internal/shell at
// the workspace root, mirroring the agent shell tool's Dir=WorkspaceRoot contract,
// WITHOUT starting an agent turn. These tests pin:
//
//   (1) GOAL MATCH — OnShell is wired and runs a real command, returning stdout.
//   (3) NO REGRESSIONS — it runs at the workspace root (pwd == GetWorkspaceRoot),
//       and a non-zero exit is a normal result (err == nil), not a Go error.
//   (4) HOLISTIC — reuses internal/shell.Execute; ui/tui stays exec-free (the
//       exec lives here, in cmd/, which already imports core packages).

import (
	"strings"
	"testing"

	"gogent/internal/gogent"
	tuipkg "gogent/ui/tui"
)

// TestEmbeddedOnShellWiredAndRunsCommand: the embedded handler set wires a
// non-nil OnShell that actually executes a command and returns its stdout.
func TestEmbeddedOnShellWiredAndRunsCommand(t *testing.T) {
	g := gogent.NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	wb := tuipkg.NewWorkbench(nil)
	h := embeddedHandlersFor(g, wb, false)

	if h.OnShell == nil {
		t.Fatal("embedded handlers must wire OnShell so the TUI's !cmd affordance works in-process")
	}
	res, err := h.OnShell("echo hi")
	if err != nil {
		t.Fatalf("OnShell(echo hi) error = %v, want nil (a successful run is not a Go error)", err)
	}
	if res.Stdout != "hi\n" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "hi\n")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Timeout {
		t.Errorf("Timeout = true, want false")
	}
}

// TestEmbeddedOnShellRunsAtWorkspaceRoot: the command runs at g's workspace root,
// the same Dir the agent shell tool uses — not the process cwd.
func TestEmbeddedOnShellRunsAtWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	g := gogent.NewGogentWithWorkspace(t.TempDir(), root)
	wb := tuipkg.NewWorkbench(nil)
	h := embeddedHandlersFor(g, wb, false)

	res, err := h.OnShell("pwd")
	if err != nil {
		t.Fatalf("OnShell(pwd) error = %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != g.GetWorkspaceRoot() {
		t.Errorf("pwd = %q, want workspace root %q (OnShell must run at the workspace root)", got, g.GetWorkspaceRoot())
	}
}

// TestEmbeddedOnShellNonZeroExitNotError: a failing command is a normal result
// carried in ExitCode, not a Go error — so the TUI renders it inline rather than
// as a launch failure. (internal/shell collapses any non-zero exit to 1, per its
// own test, so assert non-zero.)
func TestEmbeddedOnShellNonZeroExitNotError(t *testing.T) {
	g := gogent.NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	wb := tuipkg.NewWorkbench(nil)
	h := embeddedHandlersFor(g, wb, false)

	res, err := h.OnShell("sh -c 'exit 3'")
	if err != nil {
		t.Fatalf("OnShell(exit 3) error = %v, want nil (a non-zero exit is not a Go error)", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want non-zero for a failing command")
	}
}

// TestEmbeddedOnShellStderrDistinct: stderr is returned separately from stdout,
// so the transcript can render it distinctly (the embedded and remote paths both
// forward Stderr).
func TestEmbeddedOnShellStderrDistinct(t *testing.T) {
	g := gogent.NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	wb := tuipkg.NewWorkbench(nil)
	h := embeddedHandlersFor(g, wb, false)

	res, err := h.OnShell("sh -c 'echo out; echo err 1>&2'")
	if err != nil {
		t.Fatalf("OnShell: %v", err)
	}
	if !strings.Contains(res.Stdout, "out") {
		t.Errorf("Stdout = %q, want to contain 'out'", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "err") {
		t.Errorf("Stderr = %q, want to contain 'err'", res.Stderr)
	}
}
