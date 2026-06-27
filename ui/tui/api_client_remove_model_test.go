package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Issue #509: APIClient.RemoveModel DELETEs /api/models/:name to remove an entry on
// the daemon. These mirror api_client_add_model_test.go (#486): they pin the wire
// shape (method, path, auth) — including URL-escaping of the name — and that a
// non-2xx (404 unknown, 409 blocked) surfaces as a Go error the Models… dialog shows.

func TestAPIClientRemoveModelRequest(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		// r.URL.RawPath carries the escaped segment; the recorded path is the
		// EscapedPath so a slash in the name survives round-trip verbatim.
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"removed":"alpha/beta"}`))
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	if err := client.RemoveModel("alpha/beta"); err != nil {
		t.Fatalf("RemoveModel: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	// A slash in the model name must be URL-escaped so it stays one path segment.
	if gotPath != "/api/models/alpha%2Fbeta" {
		t.Errorf("path = %q, want /api/models/alpha%%2Fbeta (url.PathEscape)", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q, want Bearer tok", gotAuth)
	}
}

func TestAPIClientRemoveModelUnknownSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	if err := client.RemoveModel("ghost"); err == nil {
		t.Fatal("RemoveModel on 404 = nil, want error (c.do must surface non-2xx)")
	}
}

func TestAPIClientRemoveModelBlockedSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model is the default; set another default first", http.StatusConflict)
	}))
	defer srv.Close()

	client, err := NewAPIClient(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	if err := client.RemoveModel("default-one"); err == nil {
		t.Fatal("RemoveModel on 409 = nil, want error (a blocked removal must propagate)")
	}
}
