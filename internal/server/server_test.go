package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/gogent"
)

// fakeBackend is an httptest server speaking a minimal OpenAI-compatible
// chat-completions protocol, serving canned responses in sequence. It lets the
// server tests drive a real Gogent without a live model provider.
type fakeBackend struct {
	responses []map[string]any
	calls     int
}

func (f *fakeBackend) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	_ = body
	idx := f.calls
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	f.calls++
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f.responses[idx])
}

// newTestServer builds a Server backed by a Gogent pointed at a fake model
// endpoint. The returned URL is the base API URL (no trailing slash).
func newTestServer(t *testing.T, opts Options) (*Server, string, *fakeBackend) {
	t.Helper()
	fake := &fakeBackend{responses: []map[string]any{
		finalResponseMap("Hello from the fake model."),
	}}

	// Point the Gogent config's default model at the fake endpoint before the
	// Gogent instance is built (it bakes the endpoint into its sessions).
	backend := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(backend.Close)
	t.Setenv("GOGENT_MODEL_URL", backend.URL+"/chat/completions")

	home := t.TempDir()
	g := gogent.NewGogent(home)

	srv := NewServer(g, opts)
	return srv, backend.URL, fake
}

// finalResponseMap is a canned OpenAI-style final answer (no tool calls).
func finalResponseMap(content string) map[string]any {
	return map[string]any{
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	}
}

// loopbackReq builds a GET/POST request marked as loopback so the composing
// provider authenticates it as the local (human) user without a token.
func loopbackReq(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.RemoteAddr = "127.0.0.1:1234"
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

// serveOne invokes the assembled webapi API for a single request and returns the
// recorder, for handler-level assertions.
func serveOne(t *testing.T, srv *Server, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

func TestHealthEndpoint(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["status"] != "healthy" {
		t.Fatalf("status = %q, want healthy", got["status"])
	}
}

func TestHealthIsPublic(t *testing.T) {
	// /api/health is AuthNone, so even an anonymous remote caller reaches it.
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	r.RemoteAddr = "10.0.0.5:1" // non-loopback, no credential
	rec := serveOne(t, srv, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("health should be public, got %d", rec.Code)
	}
}

func TestProtectedEndpointRejectsAnonymous(t *testing.T) {
	// /api/sessions requires auth. webapi redirects an anonymous GET to the
	// configured login path (302) and returns 401 for non-GET methods. We assert
	// the redirect here; a separate POST check covers the 401 path.
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	r := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	r.RemoteAddr = "10.0.0.5:1" // non-loopback, no credential
	rec := serveOne(t, srv, r)
	if rec.Code != http.StatusFound && rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous remote GET should redirect to login (302) or be 401, got %d", rec.Code)
	}

	// A non-GET (POST) to a protected endpoint must be 401 for anonymous.
	r2 := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"persisted":false}`))
	r2.RemoteAddr = "10.0.0.5:1"
	r2.Header.Set("Content-Type", "application/json")
	rec2 := serveOne(t, srv, r2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous remote POST should be 401, got %d", rec2.Code)
	}
}

func TestCreateAndGetSession(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})

	// Create a session.
	body := strings.NewReader(`{"title":"My Session","persisted":false}`)
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var created sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("invalid session JSON: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created session has no id")
	}
	if created.Title != "My Session" {
		t.Fatalf("title = %q, want 'My Session'", created.Title)
	}

	// Fetch it back.
	rec = serveOne(t, srv, loopbackReq(http.MethodGet, "/api/sessions/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid session JSON: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("get returned id %q, want %q", got.ID, created.ID)
	}
}

func TestGetUnknownSession404(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/sessions/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestSendMessageAccepted covers the issue #481 contract: Send is non-blocking —
// it returns 200 with an acceptedView carrying the dispatched turn id, and the
// final answer now arrives over the SSE hub (not the response body). (The framework
// cannot emit a literal 202 for a JSON body, so the status is 200; see design §2.)
func TestSendMessageAccepted(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})

	// Create + send a message in one flow.
	body := strings.NewReader(`{"title":"s","persisted":false}`)
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions", body))
	var created sessionView
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Subscribe before sending so the terminal event is not missed.
	sub, unsub := srv.hub.subscribeSession(created.ID)
	defer unsub()

	msg := strings.NewReader(`{"message":"hi"}`)
	rec = serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions/"+created.ID+"/messages", msg))
	if rec.Code != http.StatusOK {
		t.Fatalf("send status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var acc acceptedView
	if err := json.Unmarshal(rec.Body.Bytes(), &acc); err != nil {
		t.Fatalf("invalid accepted JSON: %v", err)
	}
	if acc.TurnID == "" {
		t.Fatalf("accepted response has no turn id; body=%s", rec.Body.String())
	}

	// The final answer arrives over the hub, stamped with the dispatched turn id.
	term := awaitEvent(t, sub, func(ev agent.SessionEvent) bool { return isTerminal(ev) }, 3*time.Second)
	if term.Type != agent.SessionEventFinal {
		t.Fatalf("terminal event = %s, want final", term.Type)
	}
	if !strings.Contains(term.Text, "fake model") {
		t.Fatalf("final text = %q, want it to mention the fake model", term.Text)
	}
	if term.TurnID != acc.TurnID {
		t.Fatalf("final turn id = %q, want %q", term.TurnID, acc.TurnID)
	}
}

func TestMarkBusyRejectsSecondClaim(t *testing.T) {
	// markBusy is the guard behind the 409 a concurrent second turn gets. Test it
	// directly rather than via a hanging model backend, which would need a slow
	// server and timing to reproduce reliably.
	srv, _, _ := newTestServer(t, Options{Password: "x"})

	release1, ok1 := srv.markBusy("session-1")
	if !ok1 {
		t.Fatal("first claim should succeed")
	}
	if _, ok2 := srv.markBusy("session-1"); ok2 {
		t.Fatal("second claim for the same session should be rejected")
	}
	release1()
	// After release, a new claim is allowed again.
	if _, ok3 := srv.markBusy("session-1"); !ok3 {
		t.Fatal("claim after release should succeed")
	}
}

func TestTranscriptEndpoint(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})

	body := strings.NewReader(`{"title":"s","persisted":false}`)
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/sessions", body))
	var created sessionView
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Transcript of a fresh session has no assistant turns yet, but the call
	// should succeed and return a (possibly empty) array.
	rec = serveOne(t, srv, loopbackReq(http.MethodGet, "/api/sessions/"+created.ID+"/transcript", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var msgs []messageView
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("invalid transcript JSON: %v", err)
	}
}

func TestUnknownAgentTranscript404(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/sessions/does-not-exist/transcript?agent=root", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestApprovalsListEmpty(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/approvals", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []approvalView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid approvals JSON: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty approvals, got %d", len(got))
	}
}

func TestDecideUnknownApproval404(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/approvals/apr_bogus/decision", strings.NewReader(`{"decision":"allow"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestAuthMe reflects the loopback caller as authenticated + human scope.
// TestAuthMiddlewareGatesLegacy confirms the AuthMiddleware wrapping legacy
// handlers admits a loopback caller and rejects an anonymous remote one, so the
// legacy /message and /status endpoints are not left open on a LAN bind.
func TestAuthMiddlewareGatesLegacy(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON2(w, http.StatusOK, map[string]string{"ok": "true"})
	})
	wrapped := srv.AuthMiddleware(inner)

	// Loopback caller passes.
	rec := httptest.NewRecorder()
	r := loopbackReq(http.MethodGet, "/message", nil)
	wrapped.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback through middleware = %d, want 200", rec.Code)
	}

	// Anonymous remote caller is rejected.
	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/message", nil)
	r2.RemoteAddr = "10.0.0.5:1"
	wrapped.ServeHTTP(rec2, r2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous remote through middleware = %d, want 401", rec2.Code)
	}
}

// writeJSON2 is a tiny local helper for the middleware test's inner handler.
func writeJSON2(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestAuthMe(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/auth/me", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["authenticated"] != true {
		t.Fatalf("expected authenticated=true, got %v", got["authenticated"])
	}
	if got["scope"] != "human" {
		t.Fatalf("expected scope=human, got %v", got["scope"])
	}
}

// TestLoginSetsCookieOnCorrectPassword drives the auth login handler and asserts
// a signed session cookie is set that then authenticates a subsequent request.
func TestLoginSetsCookieOnCorrectPassword(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "hunter2"})

	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"hunter2"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	setCookies := rec.Result().Header.Values("Set-Cookie")
	if len(setCookies) == 0 {
		t.Fatal("login did not set a cookie")
	}

	// Extract the cookie value and replay it from a remote address to confirm it
	// authenticates (proving the cookie path works without loopback).
	cookie := parseCookieValue(setCookies[0])
	if cookie == "" {
		t.Fatal("could not parse cookie value")
	}
	r := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	r.RemoteAddr = "10.0.0.5:1"
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
	rec = serveOne(t, srv, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("me with valid cookie = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "hunter2"})
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"nope"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want 401", rec.Code)
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/auth/logout", nil))
	// Logout returns nil data => 204 No Content, with a cookie that clears it.
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want 204 or 200", rec.Code)
	}
	setCookies := rec.Result().Header.Values("Set-Cookie")
	if len(setCookies) == 0 {
		t.Fatal("logout did not clear the cookie")
	}
}

func TestModelsListRedactsAPIKey(t *testing.T) {
	// The default config has no API keys, but exercise the redaction path by
	// checking the endpoint returns models with has_api_key:false.
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var models []modelView
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
		t.Fatalf("invalid models JSON: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("expected at least one model")
	}
	for range models {
		// api_key must never appear in the response body.
		if bytes.Contains(rec.Body.Bytes(), []byte(`"api_key"`)) {
			t.Fatal("models response leaked an api_key field")
		}
	}
}

func TestSettingsGetRequiresHuman(t *testing.T) {
	// Loopback (human) can read settings.
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback settings status = %d, want 200", rec.Code)
	}
	var got settingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid settings JSON: %v", err)
	}
}

func TestWorkspaceEndpoint(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/workspace", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got workspaceView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid workspace JSON: %v", err)
	}
	if got.Root == "" {
		t.Fatal("workspace root is empty")
	}
}

// parseCookieValue extracts the cookie's value from a Set-Cookie header line.
func parseCookieValue(setCookie string) string {
	parts := strings.SplitN(setCookie, ";", 2)
	kv := strings.SplitN(parts[0], "=", 2)
	if len(kv) != 2 {
		return ""
	}
	return strings.TrimSpace(kv[1])
}

var _ = fmt.Sprintf
var _ = os.Getenv
