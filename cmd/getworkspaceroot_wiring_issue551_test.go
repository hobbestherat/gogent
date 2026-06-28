package main

// Issue #551 — criterion (1)/(4) wiring. The embedded handler set must wire
// GetWorkspaceRoot to the local g (mirroring ListWorkspaceFiles / ReadWorkspaceFile),
// so the status line can show the session's immutable workspace root. Mirrors the
// issue #532 embedded-wiring test composition (embeddedHandlersFor(g, wb, false)).

import (
	"testing"

	"gogent/internal/gogent"
	tuipkg "gogent/ui/tui"
)

// TestGetWorkspaceRootWiredEmbeddedIssue551 verifies the embedded handler set
// wires GetWorkspaceRoot to g.GetWorkspaceRoot(), returning the session's
// workspace root for the status-line affordance.
func TestGetWorkspaceRootWiredEmbeddedIssue551(t *testing.T) {
	home := t.TempDir()
	const root = "/test/workspace/root"
	g := gogent.NewGogentWithWorkspace(home, root)

	wb := tuipkg.NewWorkbench(nil)
	h := embeddedHandlersFor(g, wb, false)

	if h.GetWorkspaceRoot == nil {
		t.Fatal("embedded handlers must wire GetWorkspaceRoot so the status line can show the cwd")
	}
	// The handler must return exactly what g reports — robust to any path
	// cleaning inside NewGogentWithWorkspace.
	if got, want := h.GetWorkspaceRoot(), g.GetWorkspaceRoot(); got != want {
		t.Errorf("GetWorkspaceRoot() = %q, want g.GetWorkspaceRoot() = %q", got, want)
	}
	if g.GetWorkspaceRoot() == "" {
		t.Errorf("precondition: g.GetWorkspaceRoot() is empty for root %q", root)
	}
}

// TestGetWorkspaceRootAbsentFromRemoteHandlers documents the v1 scope: the remote
// (attached) handler set does not expose the daemon's root (no protocol field),
// so an attached client shows no path. This composes the attached handler set
// exactly as runAttached does (rc.Handlers() then installPresentationHandlers),
// mirroring the #507/#532 nil-while-attached precedent.
func TestGetWorkspaceRootAbsentFromRemoteHandlersIssue551(t *testing.T) {
	_, client := daemonWithModelsIssue507(t, "daemon-model", "daemon-model")
	clientG := gogent.NewGogent(t.TempDir())

	wb := tuipkg.NewWorkbench(nil)
	handlers := tuipkg.NewRemoteClient(client, wb.EmitSessionEvent, wb).Handlers()
	installPresentationHandlers(&handlers, clientG, wb, false)

	if handlers.GetWorkspaceRoot != nil {
		t.Fatal("attached handlers must leave GetWorkspaceRoot nil in v1 (no protocol field for the daemon root)")
	}
}
