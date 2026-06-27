package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"gogent/internal/config"
)

// Issue #532 — GOAL 1 HTTP seam. POST/PUT /models map an unroutable config to 400
// (client error), classified via errors.Is(err, gogent.ErrModelInvalid), distinct
// from 409 (duplicate name) and 404 (not found).
//
// These tests also pin the modelsSvc.Update signature reorder — webapi binds the
// JSON body to the index-1 parameter, so the body MUST precede the :name path
// param. TestUpdateModel_ValidBody_Mutates_BodyIsBound is the load-bearing check:
// a valid PUT must take effect (proving the body is read); if the body were
// silently dropped, the zero-value config would be rejected as unroutable (400).

// unroutableCreateBody is {name, api_key, rest empty}: a model with no api_type and
// no endpoint — the headline unroutable shape from the issue.
func unroutableCreateBody(t *testing.T, name string) []byte {
	t.Helper()
	b, err := json.Marshal(config.ModelConfig{Name: name, APIKey: "k"})
	if err != nil {
		t.Fatalf("marshal unroutable body: %v", err)
	}
	return b
}

// getModelView fetches GET /api/models and returns the redacted entry with the given
// name (nil if absent), so a test can assert field-level mutation.
func getModelView(t *testing.T, srv *Server, name string) *modelView {
	t.Helper()
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/models status = %d", rec.Code)
	}
	var views []modelView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode models list: %v", err)
	}
	for i := range views {
		if views[i].Name == name {
			return &views[i]
		}
	}
	return nil
}

// TestCreateModel_Unroutable_Returns400_NotPersisted: POSTing an unroutable config
// is a 400 and the entry is NOT created.
func TestCreateModel_Unroutable_Returns400_NotPersisted(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})

	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/models", bytes.NewReader(unroutableCreateBody(t, "unroutable"))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST unroutable status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if modelExists(t, srv, "unroutable") {
		t.Fatal("an unroutable model must not be persisted")
	}
}

// TestCreateModel_HostedGatewayEmptyModel_Returns400: an openrouter entry with an
// empty model is also unroutable and must be a 400.
func TestCreateModel_HostedGatewayEmptyModel_Returns400(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	body, _ := json.Marshal(config.ModelConfig{Name: "gw", APIType: "openrouter", APIKey: "k"})
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/models", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST openrouter-empty-model status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if modelExists(t, srv, "gw") {
		t.Fatal("an openrouter-empty-model entry must not be persisted")
	}
}

// TestCreateModel_UnroutableBodyNamesModel: the 400 response carries the model-named,
// actionable detail so a remote caller can see what was wrong (not just "400").
func TestCreateModel_UnroutableBodyNamesModel(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/models", bytes.NewReader(unroutableCreateBody(t, "named-bad"))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("named-bad")) {
		t.Errorf("400 body should name the rejected model; got %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("misconfigured")) {
		t.Errorf("400 body should explain the misconfiguration; got %s", rec.Body.String())
	}
}

// TestUpdateModel_UnroutableBody_Returns400_ExistingIntact: a PUT with an unroutable
// body is a 400 and leaves the existing entry untouched (not wiped to an unroutable
// shape).
func TestUpdateModel_UnroutableBody_Returns400_ExistingIntact(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	if rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/models", bytes.NewReader(modelCreateBody(t, "upd")))); rec.Code != http.StatusOK {
		t.Fatalf("seed POST status = %d; body=%s", rec.Code, rec.Body.String())
	}

	rec := serveOne(t, srv, loopbackReq(http.MethodPut, "/api/models/upd", bytes.NewReader(unroutableCreateBody(t, "upd"))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT unroutable status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	v := getModelView(t, srv, "upd")
	if v == nil {
		t.Fatal("the existing model vanished after a rejected PUT")
	}
	if v.Model != "catalog-model-id" || v.Endpoint != "https://catalog.example.com/v1" {
		t.Errorf("the existing model was mutated by a rejected PUT: model=%q endpoint=%q", v.Model, v.Endpoint)
	}
}

// TestUpdateModel_ValidBody_Mutates_BodyIsBound is the critical body-binding check
// for the signature reorder. A PUT with a DIFFERENT valid model id + endpoint must
// take effect — proving the handler reads the JSON body (and is not silently dropping
// it for the :name path param). If the body were dropped, the zero-value config would
// be rejected as unroutable (400), failing the 200 assertion.
func TestUpdateModel_ValidBody_Mutates_BodyIsBound(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	if rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/models", bytes.NewReader(modelCreateBody(t, "mut")))); rec.Code != http.StatusOK {
		t.Fatalf("seed POST status = %d; body=%s", rec.Code, rec.Body.String())
	}
	newBody, _ := json.Marshal(config.ModelConfig{
		Name:     "mut", // ignored — overwritten by the :name path param
		APIType:  "openai",
		Model:    "rewritten-model-id",
		Endpoint: "https://rewritten.example.com/v1",
	})
	rec := serveOne(t, srv, loopbackReq(http.MethodPut, "/api/models/mut", bytes.NewReader(newBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT valid status = %d, want 200 (if 400, the body is being dropped — signature regression); body=%s", rec.Code, rec.Body.String())
	}
	v := getModelView(t, srv, "mut")
	if v == nil {
		t.Fatal("the model vanished after a valid PUT")
	}
	if v.Model != "rewritten-model-id" || v.Endpoint != "https://rewritten.example.com/v1" {
		t.Errorf("the PUT body was not applied (body-binding broken): model=%q endpoint=%q", v.Model, v.Endpoint)
	}
}

// TestUpdateModel_NotFound_Returns404: PUT on an unknown name is 404, not 400 (the
// not-found path wins over validation).
func TestUpdateModel_NotFound_Returns404(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	rec := serveOne(t, srv, loopbackReq(http.MethodPut, "/api/models/never-existed", bytes.NewReader(unroutableCreateBody(t, "never-existed"))))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PUT unknown status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUpdateModel_DuplicateStill409OnCreate (regression guard): the validation 400
// path must not have swallowed the existing 409 duplicate-name path on POST.
func TestCreateModel_DuplicateStill409(t *testing.T) {
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	body := modelCreateBody(t, "dup-532")
	if rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/models", bytes.NewReader(body))); rec.Code != http.StatusOK {
		t.Fatalf("first POST status = %d; body=%s", rec.Code, rec.Body.String())
	}
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/models", bytes.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate POST status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}
