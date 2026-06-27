package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Regression coverage for issue #507: in attached mode the default model is
// daemon-owned, so RemoteClient.Handlers wires GetDefaultModel/SetDefaultModel to
// /api/settings exactly the way GetBudget/SetBudget do (riding GET/PUT /settings, no
// dedicated APIClient method).
//
// Design criteria under test:
//   (1) goal — GetDefaultModel reads the daemon's default_model; SetDefaultModel writes
//       it via a read-modify-write that returns the error (unlike the error-swallowing
//       rc.mutateSettings used by budget).
//   (2) usability — an invalid name (daemon 400) propagates as a Go error the model
//       editor surfaces; a read failure ALSO propagates (it must not be silently
//       swallowed into a false success).
//   (3) no regressions — the RMW preserves the daemon's other settings (it changes only
//       default_model), so a "Set as default" cannot clobber budget/timeouts/etc.
//   (4) holistic — ui/tui only speaks HTTP via APIClient (no daemon import); these tests
//       pin the request shape against a stub daemon.

// startSettingsStubIssue507 serves a minimal /api/settings surface and records each call:
// GET returns getBody with getStatus (200 when 0), PUT records the decoded body and
// returns putStatus (200 when 0). It returns the server plus channels carrying each
// recorded PUT body and each GET.
func startSettingsStubIssue507(t *testing.T, getBody string, getStatus, putStatus int) (*httptest.Server, chan map[string]any, chan struct{}) {
	t.Helper()
	puts := make(chan map[string]any, 8)
	gets := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case http.MethodGet + " /api/settings":
			gets <- struct{}{}
			if getStatus != 0 && getStatus != http.StatusOK {
				http.Error(w, "stub get failure", getStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(getBody))
		case http.MethodPut + " /api/settings":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			puts <- body
			if putStatus != 0 && putStatus != http.StatusOK {
				http.Error(w, `{"error":"model \"x\" not found"}`, putStatus)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, puts, gets
}

// TestRemoteGetDefaultModelIssue507ReadsDaemon: GetDefaultModel issues GET /api/settings
// and returns the daemon's default_model.
func TestRemoteGetDefaultModelIssue507ReadsDaemon(t *testing.T) {
	srv, _, gets := startSettingsStubIssue507(t,
		`{"default_model":"daemon-default","budget":{"token_budget":5}}`, 0, 0)

	client, err := NewAPIClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	h := NewRemoteClient(client, nil, nil).Handlers()

	if got := h.GetDefaultModel(); got != "daemon-default" {
		t.Fatalf("GetDefaultModel = %q, want daemon-default (read from the daemon)", got)
	}
	select {
	case <-gets:
	case <-time.After(time.Second):
		t.Fatalf("GetDefaultModel did not issue GET /api/settings")
	}
}

// TestRemoteGetDefaultModelIssue507ReturnsEmptyOnGetError: a failed GET degrades to ""
// (mirroring GetBudget's error→zero-value), which NewSession resolves to index 0.
func TestRemoteGetDefaultModelIssue507ReturnsEmptyOnGetError(t *testing.T) {
	srv, _, _ := startSettingsStubIssue507(t, `{}`, http.StatusInternalServerError, 0)
	client, _ := NewAPIClient(srv.URL, "")
	if got := NewRemoteClient(client, nil, nil).Handlers().GetDefaultModel(); got != "" {
		t.Fatalf("GetDefaultModel on a GET failure = %q, want empty (graceful degrade)", got)
	}
}

// TestRemoteGetDefaultModelIssue507NilWhenNoHandler: before attach wires the handlers,
// GetDefaultModel is nil — the editor/NewSession must tolerate that (the control hides).
// This pins the contract the attach wiring relies on.
func TestRemoteGetDefaultModelIssue507NilWhenNoHandler(t *testing.T) {
	var h Handlers
	if h.GetDefaultModel != nil {
		t.Fatalf("zero-value Handlers.GetDefaultModel must be nil until wired")
	}
	if h.SetDefaultModel != nil {
		t.Fatalf("zero-value Handlers.SetDefaultModel must be nil until wired")
	}
}

// TestRemoteSetDefaultModelIssue507ReadModifyWrite: SetDefaultModel does a GET then a PUT,
// sets ONLY default_model, preserves the daemon's other settings, and returns nil on 200.
func TestRemoteSetDefaultModelIssue507ReadModifyWrite(t *testing.T) {
	srv, puts, gets := startSettingsStubIssue507(t,
		`{"default_model":"old","budget":{"token_budget":42},"review_edits":true}`, 0, 0)
	client, _ := NewAPIClient(srv.URL, "")
	h := NewRemoteClient(client, nil, nil).Handlers()

	if err := h.SetDefaultModel("new-default"); err != nil {
		t.Fatalf("SetDefaultModel returned error on 200: %v", err)
	}

	// Exactly one GET followed by exactly one PUT.
	select {
	case <-gets:
	case <-time.After(time.Second):
		t.Fatalf("SetDefaultModel did not issue a GET /api/settings first")
	}
	var putBody map[string]any
	select {
	case putBody = <-puts:
	case <-time.After(time.Second):
		t.Fatalf("SetDefaultModel did not issue a PUT /api/settings")
	}
	if dm, _ := putBody["default_model"].(string); dm != "new-default" {
		t.Fatalf("PUT default_model = %v, want new-default", putBody["default_model"])
	}
	// The RMW must preserve the other fields it read from the daemon.
	budget, _ := putBody["budget"].(map[string]any)
	if budget == nil {
		t.Fatalf("PUT body dropped the budget block: %#v", putBody)
	}
	if tb, _ := budget["token_budget"].(float64); int(tb) != 42 {
		t.Fatalf("RMW clobbered the daemon's budget: token_budget=%v, want 42", budget["token_budget"])
	}
	if re, _ := putBody["review_edits"].(bool); !re {
		t.Fatalf("RMW clobbered the daemon's review_edits: %#v", putBody["review_edits"])
	}
}

// TestRemoteSetDefaultModelIssue507Propagates400: a daemon 400 (invalid model name)
// comes back as a non-nil Go error so the model editor can surface it.
func TestRemoteSetDefaultModelIssue507Propagates400(t *testing.T) {
	srv, _, _ := startSettingsStubIssue507(t, `{"default_model":"old"}`, 0, http.StatusBadRequest)
	client, _ := NewAPIClient(srv.URL, "")
	if err := NewRemoteClient(client, nil, nil).Handlers().SetDefaultModel("bad"); err == nil {
		t.Fatalf("SetDefaultModel returned nil on a daemon 400; want a propagated error")
	}
}

// TestRemoteSetDefaultModelIssue507PropagatesGetError: a GET failure must propagate as a
// non-nil error — it must NOT be swallowed into a false success (the trap if someone
// copied rc.mutateSettings' log-and-return). This is the regression guard for the
// "read settings" error branch.
func TestRemoteSetDefaultModelIssue507PropagatesGetError(t *testing.T) {
	srv, _, _ := startSettingsStubIssue507(t, `{}`, http.StatusInternalServerError, 0)
	client, _ := NewAPIClient(srv.URL, "")
	if err := NewRemoteClient(client, nil, nil).Handlers().SetDefaultModel("x"); err == nil {
		t.Fatalf("SetDefaultModel returned nil on a GET failure; want a propagated error (must not be swallowed)")
	}
}
