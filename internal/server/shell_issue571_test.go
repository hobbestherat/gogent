package server

// Issue #571 — the daemon half of `!`-prefixed shell commands. POST /api/shell
// runs a command out-of-band at the daemon workspace root and returns its
// stdout/stderr/exit, never starting an agent turn. These tests pin:
//
//   (1) GOAL MATCH — a command executes and its stdout/exit come back.
//   (3) NO REGRESSIONS — it runs at the workspace root (same Dir contract as the
//       agent shell tool), a non-zero exit is a 200 carrying exit_code (NOT a
//       500), and auth is respected: an anonymous remote caller is 401, a
//       peer/agent-scoped token is 403 (the requireHuman gate), loopback is human.
//   (4) HOLISTIC — reuses internal/shell.Execute and the existing AuthRequired +
//       requireHuman endpoint pattern; no new machinery.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogent/internal/gogent"
)

// shellJSON issues POST /api/shell from a loopback (human) caller with the given
// JSON body and returns the response recorder.
func shellJSON(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/shell", strings.NewReader(body)))
	return rec
}

// TestShellEndpointRunsCommand: POST /api/shell {"command":"echo hi"} → 200 with
// stdout "hi\n" and no error. The headline behaviour (acceptance #1, remote mode).
func TestShellEndpointRunsCommand(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := shellJSON(t, srv, `{"command":"echo hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var v shellView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode shellView: %v (body=%s)", err, rec.Body.String())
	}
	if v.Stdout != "hi\n" {
		t.Errorf("stdout = %q, want %q", v.Stdout, "hi\n")
	}
	if v.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", v.ExitCode)
	}
	if v.Timeout {
		t.Errorf("timeout = true, want false")
	}
	if v.Error != "" {
		t.Errorf("error = %q, want empty on a successful run", v.Error)
	}
}

// TestShellEndpointRunsAtWorkspaceRoot: the command executes at the daemon
// workspace root (the Dir=WorkspaceRoot contract), not the process cwd. `pwd`
// must report the same root g.GetWorkspaceRoot() returns. Uses a real temp dir
// so chdir succeeds (a non-existent root makes the command fail with no output).
func TestShellEndpointRunsAtWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	g := gogent.NewGogentWithWorkspace(t.TempDir(), root)
	srv := NewServer(g, Options{Password: "x"})

	rec := shellJSON(t, srv, `{"command":"pwd"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var v shellView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := strings.TrimSpace(v.Stdout); got != g.GetWorkspaceRoot() {
		t.Errorf("pwd = %q, want workspace root %q (Dir must be WorkspaceRoot)", got, g.GetWorkspaceRoot())
	}
}

// TestShellEndpointEmptyCommandIs400: a missing/blank command is a client error,
// not a 500 and not an executed empty shell.
func TestShellEndpointEmptyCommandIs400(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	for _, body := range []string{`{"command":""}`, `{"command":"   "}`, `{}`} {
		rec := shellJSON(t, srv, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400; body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

// TestShellEndpointNonZeroExitIs200: a failing command is a NORMAL result carried
// in exit_code, not a server error. This is the out-of-band contract: only a
// launch/transport failure is a 5xx, never a non-zero exit. (internal/shell
// collapses any non-zero exit to exit_code=1 — pinned by shell_test.go — so the
// assertion is "non-zero", matching that established contract, not a specific
// code.)
func TestShellEndpointNonZeroExitIs200(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := shellJSON(t, srv, `{"command":"sh -c 'exit 3'"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a non-zero exit is not a server error); body=%s",
			rec.Code, rec.Body.String())
	}
	var v shellView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.ExitCode == 0 {
		t.Errorf("exit_code = 0, want non-zero for a failing command")
	}
}

// TestShellEndpointStderrSurfaceed: stderr is returned distinctly from stdout so
// the client can render it as such.
func TestShellEndpointStderrSurfaced(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := shellJSON(t, srv, `{"command":"sh -c 'echo out; echo err 1>&2'"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var v shellView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(v.Stdout, "out") {
		t.Errorf("stdout = %q, want to contain 'out'", v.Stdout)
	}
	if !strings.Contains(v.Stderr, "err") {
		t.Errorf("stderr = %q, want to contain 'err'", v.Stderr)
	}
}

// shellReqFrom builds a POST /api/shell request with an explicit RemoteAddr and
// (optional) bearer token, to exercise the auth/scope paths that loopbackReq
// (always human) cannot.
func shellReqFrom(remote, token, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/shell", strings.NewReader(body))
	r.RemoteAddr = remote
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

// TestShellEndpointAnonymousRemoteIs401: a remote caller with no credential is
// rejected — /shell is AuthRequired, like every non-public endpoint.
func TestShellEndpointAnonymousRemoteIs401(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, shellReqFrom("10.0.0.5:5000", "", `{"command":"echo hi"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous remote status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

// TestShellEndpointPeerTokenForbidden: a peer/agent-scoped token is rejected by
// the requireHuman gate (403) — an agent token must not drive the out-of-band
// shell (safety gate #5).
func TestShellEndpointPeerTokenForbidden(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Token: "peer-tok", TokenScope: scopePeer})
	rec := serveOne(t, srv, shellReqFrom("10.0.0.5:5000", "peer-tok", `{"command":"echo hi"}`))
	if rec.Code != http.StatusForbidden {
		t.Errorf("peer-token status = %d, want 403 (requireHuman); body=%s", rec.Code, rec.Body.String())
	}
}

// TestShellEndpointHumanTokenAllowed: a human-scoped token IS allowed (the
// positive side of the requireHuman gate), so an SSH-attached human can run !cmd.
func TestShellEndpointHumanTokenAllowed(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Token: "human-tok", TokenScope: scopeHuman})
	rec := serveOne(t, srv, shellReqFrom("10.0.0.5:5000", "human-tok", `{"command":"echo hi"}`))
	if rec.Code != http.StatusOK {
		t.Errorf("human-token status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestShellEndpointMethodAndPath: the route is POST /api/shell; GET is not wired
// (webapi returns 405), confirming the endpoint is registered at the right path
// and method.
func TestShellEndpointMethodAndPath(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	// GET must not be handled as a successful shell call.
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/shell", nil))
	if rec.Code == http.StatusOK {
		t.Errorf("GET /api/shell returned 200; only POST is wired (got %d)", rec.Code)
	}
}
