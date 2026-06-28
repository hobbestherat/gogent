package main

// Issue #570 — acceptance (1)/(2). Beyond the wiring pin
// (TestGetWorkspaceRootWiredInRemoteHandlersIssue570), the attached
// GetWorkspaceRoot must actually return the DAEMON's workspace root — where !
// shell commands and the agent's shell tool calls run — fetched over GET
// /api/workspace, NOT the attached client core's cwd. This builds a loopback daemon
// rooted at a known path, composes the handler set exactly as runAttached does
// (rc.Handlers() then installPresentationHandlers), and asserts the async-fetched
// value resolves to the daemon's root.

import (
	"net/http/httptest"
	"testing"
	"time"

	"gogent/internal/gogent"
	"gogent/internal/server"
	tuipkg "gogent/ui/tui"
)

// daemonWithWorkspaceRootIssue570 builds a loopback /api daemon whose core is rooted
// at root, returning the live daemon core (for direct root comparison) and a
// credential-less loopback HTTP client. Mirrors daemonWithModelsIssue507 minus model
// seeding: only the workspace root matters here.
func daemonWithWorkspaceRootIssue570(t *testing.T, root string) (*gogent.Gogent, *tuipkg.APIClient) {
	t.Helper()
	g := gogent.NewGogentWithWorkspace(t.TempDir(), root)
	srv := server.NewServer(g, server.Options{})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	client, err := tuipkg.NewAPIClient(httpSrv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	return g, client
}

// TestAttachedGetWorkspaceRootReturnsDaemonRootIssue570: after composing
// rc.Handlers() + installPresentationHandlers, GetWorkspaceRoot resolves (via an
// async background fetch, so it must not block the UI thread) to the DAEMON's root,
// not the client core's cwd. This is the end-to-end proof of the fix: the attached
// status line shows where the daemon's shell/tool calls run.
func TestAttachedGetWorkspaceRootReturnsDaemonRootIssue570(t *testing.T) {
	// A real temp dir as the daemon root keeps vcs.IsRepo well-defined (not a repo)
	// and guarantees a non-empty, existing workspace path.
	daemonG, client := daemonWithWorkspaceRootIssue570(t, t.TempDir())
	clientG := gogent.NewGogent(t.TempDir()) // client core rooted at the process cwd

	// Setup sanity: the daemon and client roots must differ so a leak of the client
	// root would be observable. Compare via each core's own getter so path cleaning
	// inside the constructors cannot mask a mismatch.
	if daemonG.GetWorkspaceRoot() == "" {
		t.Fatal("setup: daemon workspace root is empty")
	}
	if daemonG.GetWorkspaceRoot() == clientG.GetWorkspaceRoot() {
		t.Fatalf("setup: daemon root %q == client cwd %q; the split must be observable",
			daemonG.GetWorkspaceRoot(), clientG.GetWorkspaceRoot())
	}

	wb := tuipkg.NewWorkbench(nil)
	handlers := tuipkg.NewRemoteClient(client, wb.EmitSessionEvent, wb).Handlers()
	installPresentationHandlers(&handlers, clientG, wb, false)

	if handlers.GetWorkspaceRoot == nil {
		t.Fatal("attached handlers must wire GetWorkspaceRoot (issue #570)")
	}

	// The fetch is async (it kicks a background GET /api/workspace and returns "" until
	// it lands); poll for the value within a bounded deadline so the test stays
	// deterministic on loopback.
	deadline := time.Now().Add(3 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if got = handlers.GetWorkspaceRoot(); got != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got == "" {
		t.Fatal("attached GetWorkspaceRoot stayed empty: the background GET /api/workspace never landed")
	}
	if got != daemonG.GetWorkspaceRoot() {
		t.Errorf("attached GetWorkspaceRoot = %q, want daemon root %q (where shell/tool calls run), not the client cwd",
			got, daemonG.GetWorkspaceRoot())
	}
	if got == clientG.GetWorkspaceRoot() {
		t.Errorf("attached GetWorkspaceRoot leaked the CLIENT cwd %q; the daemon-owned handler must read the daemon, not the client core",
			got)
	}
}
