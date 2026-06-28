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

// TestGetWorkspaceRootWiredInRemoteHandlersIssue570 FLIPS the #551 pin for issue
// #570: the remote (attached) handler set now WIRES GetWorkspaceRoot to the daemon's
// workspace root over GET /api/workspace, so an attached client shows the same
// status-line path the local TUI does. This composes the attached handler set exactly
// as runAttached does (rc.Handlers() then installPresentationHandlers), mirroring the
// #507 daemon-owned precedent: like the default model, the workspace root is
// daemon-owned and so is wired in RemoteClient.Handlers, NOT by
// installPresentationHandlers (whose local g would report the client cwd). The value
// the handler returns is asserted in TestAttachedGetWorkspaceRootReturnsDaemonRootIssue570.
func TestGetWorkspaceRootWiredInRemoteHandlersIssue570(t *testing.T) {
	_, client := daemonWithModelsIssue507(t, "daemon-model", "daemon-model")
	clientG := gogent.NewGogent(t.TempDir())

	wb := tuipkg.NewWorkbench(nil)
	handlers := tuipkg.NewRemoteClient(client, wb.EmitSessionEvent, wb).Handlers()
	installPresentationHandlers(&handlers, clientG, wb, false)

	if handlers.GetWorkspaceRoot == nil {
		t.Fatal("attached handlers must wire GetWorkspaceRoot (issue #570) so the status line shows the daemon root")
	}
}
