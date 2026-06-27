package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Issue #509: in attached/remote mode, RemoteClient.Handlers wires RemoveModel to
// APIClient.RemoveModel (DELETE /api/models/:name), exactly as AddModel/UpdateModel
// are wired. These mirror default_model_issue507_test.go: they stand up a stub
// daemon, drive the real Handlers().RemoveModel closure, and assert the request the
// attached client actually issues — so a wiring regression (nil handler, wrong
// method, swallowed error) is caught at the seam, not just in APIClient alone.

func TestRemoteHandlersRemoveModelIssuesDelete(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"removed":"alpha"}`))
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	h := NewRemoteClient(client, nil, nil).Handlers()
	if h.RemoveModel == nil {
		t.Fatal("RemoteClient.Handlers().RemoveModel is nil; the remote RemoveModel wiring is missing")
	}
	if err := h.RemoveModel("alpha"); err != nil {
		t.Fatalf("RemoveModel via remote handlers: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("daemon saw %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/models/alpha" {
		t.Errorf("daemon saw path %q, want /api/models/alpha", gotPath)
	}
}

func TestRemoteHandlersRemoveModelSurfacesBlockedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model is the default; set another default first", http.StatusConflict)
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	h := NewRemoteClient(client, nil, nil).Handlers()
	if h.RemoveModel == nil {
		t.Fatal("RemoteClient.Handlers().RemoveModel is nil")
	}
	// A daemon-side block (409) must propagate as a Go error so the Models… dialog
	// can show the reason — it must NOT be silently swallowed into a false success.
	if err := h.RemoveModel("default-one"); err == nil {
		t.Fatal("remote RemoveModel on 409 = nil, want the error propagated to the dialog")
	}
}
