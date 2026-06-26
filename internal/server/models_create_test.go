package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gogent/internal/config"
)

// Issue #486: POST /api/models creates a NEW model entry (the catalog-assisted
// add). The handler mirrors the existing model endpoints: it requires the human
// scope and returns 409 on a name conflict. The request body is the flat
// config.ModelConfig (updateModelRequest embeds it anonymously), matching what
// APIClient.AddModel posts.

func modelCreateBody(t *testing.T, name string) []byte {
	t.Helper()
	b, err := json.Marshal(config.ModelConfig{
		Name:        name,
		DisplayName: "Catalog " + name,
		APIType:     "openai",
		Model:       "catalog-model-id",
		Endpoint:    "https://catalog.example.com/v1",
		APIKey:      "super-secret-key",
		Temperature: 0.7,
		MaxTokens:   4096,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return b
}

func TestCreateModel(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})

	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/models", bytes.NewReader(modelCreateBody(t, "create-ok"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// The response is the redacted modelView (a new entry, not an array).
	var view modelView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode modelView: %v", err)
	}
	if view.Name != "create-ok" {
		t.Errorf("response name = %q, want create-ok", view.Name)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("super-secret-key")) {
		t.Error("create response leaked the api_key")
	}

	// The new entry is immediately listed by GET /models.
	list := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/models", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d", list.Code)
	}
	if !bytes.Contains(list.Body.Bytes(), []byte(`"create-ok"`)) {
		t.Fatal("created model not present in GET /models")
	}
	if bytes.Contains(list.Body.Bytes(), []byte("super-secret-key")) {
		t.Error("list response leaked the api_key")
	}
}

func TestCreateModelConflictReturns409(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	body := modelCreateBody(t, "create-conflict")

	first := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/models", bytes.NewReader(body)))
	if first.Code != http.StatusOK {
		t.Fatalf("first create status = %d, want 200; body=%s", first.Code, first.Body.String())
	}

	// Re-POSTing the same name must conflict (AddModel rejects the duplicate).
	second := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/models", bytes.NewReader(body)))
	if second.Code != http.StatusConflict {
		t.Fatalf("duplicate create status = %d, want 409; body=%s", second.Code, second.Body.String())
	}
}

// POST /models — like GET/PUT/scan — is gated on the human scope: an anonymous
// remote caller is rejected and the entry is NOT created.
func TestCreateModelRequiresHuman(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	body := modelCreateBody(t, "create-anon")

	r := httptest.NewRequest(http.MethodPost, "/api/models", bytes.NewReader(body))
	r.RemoteAddr = "10.0.0.5:1" // non-loopback, no credential
	r.Header.Set("Content-Type", "application/json")
	rec := serveOne(t, srv, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous POST status = %d, want 401", rec.Code)
	}

	// The rejected entry must not have leaked into the configured models.
	list := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/models", nil))
	if bytes.Contains(list.Body.Bytes(), []byte(`"create-anon"`)) {
		t.Fatal("anonymous create should not persist a model")
	}
}

// POST /models stores a blank key verbatim (a new entry has no prior key to
// preserve, unlike PUT). The catalog flow's review form is responsible for the
// credential; until then the entry simply has none.
func TestCreateModelStoresBlankKey(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	body, _ := json.Marshal(config.ModelConfig{
		Name:    "create-nokey",
		APIType: "openai",
		Model:   "m",
		APIKey:  "", // blank — must NOT be borrowed from anywhere
	})
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/models", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var view modelView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.HasAPIKey {
		t.Error("HasAPIKey = true, want false for a blank-key create")
	}
}
