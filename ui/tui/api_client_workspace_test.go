package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Issue #570: APIClient.Workspace GETs /api/workspace for the daemon's own working
// directory, backing the attached status-line path affordance. These pin the wire
// shape (method, path, auth), the JSON decode of the mirrored workspaceView
// (root + optional git), and that a non-2xx surfaces as a Go error — mirroring
// api_client_remove_model_test.go (#509) and the established per-endpoint DTO-mirror
// convention.

// TestAPIClientWorkspaceRequest pins the round-trip: GET, /api/workspace path, the
// bearer token, and that the daemon root decodes into WorkspaceDTO.Root.
func TestAPIClientWorkspaceRequest(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"root":"/daemon/workspace"}`))
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	ws, err := client.Workspace()
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/api/workspace" {
		t.Errorf("path = %q, want /api/workspace (c.do prepends /api)", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q, want Bearer tok", gotAuth)
	}
	if ws.Root != "/daemon/workspace" {
		t.Errorf("Root = %q, want /daemon/workspace", ws.Root)
	}
}

// TestAPIClientWorkspaceDecodesGit covers the forward-use Git field: when the daemon
// returns a git block (a repo workspace), it decodes into WorkspaceDTO.Git verbatim —
// a faithful mirror of the server's workspaceView/gitInfo, kept for a later status-line
// git decoration even though only Root is consumed today.
func TestAPIClientWorkspaceDecodesGit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"root":"/daemon/workspace","git":{"branch":"main","dirty":true}}`))
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	ws, err := client.Workspace()
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if ws.Git == nil {
		t.Fatal("Git = nil, want decoded gitInfo (forward-use mirror of workspaceView)")
	}
	if ws.Git.Branch != "main" || !ws.Git.Dirty {
		t.Errorf("Git = {Branch:%q Dirty:%v}, want {Branch:main Dirty:true}", ws.Git.Branch, ws.Git.Dirty)
	}
}

// TestAPIClientWorkspaceOmitsGitWhenAbsent: when the daemon omits the git block (a
// non-repo workspace, or root with no git), Git decodes to nil (omitempty) and Root
// still decodes — the status-line path must not depend on git info.
func TestAPIClientWorkspaceOmitsGitWhenAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"root":"/daemon/workspace"}`))
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	ws, err := client.Workspace()
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if ws.Git != nil {
		t.Errorf("Git = %+v, want nil when the daemon omits the git block", ws.Git)
	}
	if ws.Root != "/daemon/workspace" {
		t.Errorf("Root = %q, want /daemon/workspace even without a git block", ws.Root)
	}
}

// TestAPIClientWorkspaceErrorOnNon2xx: a server error must surface as a Go error
// (so the caller can degrade gracefully rather than treat an empty body as the
// daemon's root) and return an empty DTO.
func TestAPIClientWorkspaceErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	ws, err := client.Workspace()
	if err == nil {
		t.Fatal("Workspace on 500 = nil error, want an error (c.do must surface non-2xx)")
	}
	if ws.Root != "" {
		t.Errorf("Root = %q on error, want empty DTO", ws.Root)
	}
}
