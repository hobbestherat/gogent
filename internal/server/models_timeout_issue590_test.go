package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"gogent/internal/config"
)

// Issue #590 Part B — the per-model ModelTimeoutSeconds override must survive the
// full daemon round-trip: TUI edits a model → PUT /api/models/:name → stored on
// disk → GET /api/models returns it → TUI re-edits. The implementation extended
// modelView (GET), updateModelRequest (PUT/POST, via the embedded config.ModelConfig)
// and api_client.ModelDTO/ToModelConfig to carry the field. These tests pin that
// wire path end-to-end at the server, including a DISK-level reload — the only place
// a silent drop (a forgotten field in a projection) would otherwise hide.
//
// They reuse the #537/#538 loopback-CRUD harness (newModelUpdateServer seeds a real
// isolated core; no model provider is contacted). Hermetic: no network.

// modelWithTimeout returns a fully-routable seed model with the per-model timeout
// override set, so the round-trip assertion surface includes the new field.
func modelWithTimeout(seconds int) config.ModelConfig {
	m := seedModel()
	m.ModelTimeoutSeconds = seconds
	return m
}

// TestModelTimeoutSecondsProjectionPinsTheView is the unit-level guard: modelToView
// (the redacted GET projection) carries the override through, in both directions.
func TestModelTimeoutSecondsProjectionPinsTheView(t *testing.T) {
	if got := modelToView(&config.ModelConfig{Name: "m", ModelTimeoutSeconds: 900}).ModelTimeoutSeconds; got != 900 {
		t.Errorf("modelToView override = %d, want 900", got)
	}
	if got := modelToView(&config.ModelConfig{Name: "m"}).ModelTimeoutSeconds; got != 0 {
		t.Errorf("modelToView unset override = %d, want 0", got)
	}
}

// TestModelTimeoutSecondsOmittedOnWireWhenZero pins the omitempty wire shape: an
// unset override must NOT appear in the GET JSON (so old clients/old configs are
// byte-stable), while a set override is emitted under its documented key.
func TestModelTimeoutSecondsOmittedOnWireWhenZero(t *testing.T) {
	srv, _ := newModelUpdateServer(t)
	if err := srv.g.AddModel(modelWithTimeout(0)); err != nil {
		t.Fatalf("AddModel (unset): %v", err)
	}
	if err := srv.g.AddModel(func() config.ModelConfig { m := seedModel(); m.Name = "slow"; m.ModelTimeoutSeconds = 901; return m }()); err != nil {
		t.Fatalf("AddModel (set): %v", err)
	}
	get := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/models", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d; body=%s", get.Code, get.Body.String())
	}
	body := get.Body.Bytes()
	// The unset ("work") model must not carry the key anywhere in the body — but the
	// set ("slow") model does, so assert per-entry by decoding rather than whole-body.
	var views []modelView
	if err := json.Unmarshal(body, &views); err != nil {
		t.Fatalf("decode views: %v", body)
	}
	unset, _ := findModelView(views, "work")
	if unset.ModelTimeoutSeconds != 0 {
		t.Errorf("unset model override decoded = %d, want 0", unset.ModelTimeoutSeconds)
	}
	set, _ := findModelView(views, "slow")
	if set.ModelTimeoutSeconds != 901 {
		t.Errorf("set model override decoded = %d, want 901", set.ModelTimeoutSeconds)
	}
	// The raw body must carry the set entry's key with its value.
	if !bytes.Contains(body, []byte(`"model_timeout_seconds":901`)) {
		t.Errorf("GET body omits the set override: %s", body)
	}
}

// TestModelTimeoutSecondsRoundTripsThroughRedactedEdit is the #590 headline wire
// guard: the exact TUI flow — GET the redacted view, change the override, re-marshal
// the view (no api_key) and PUT it back — persists the override to DISK. It fails if
// modelView, updateModelRequest or the handler drops the field anywhere on the path.
func TestModelTimeoutSecondsRoundTripsThroughRedactedEdit(t *testing.T) {
	srv, home := newModelUpdateServer(t)
	if err := srv.g.AddModel(modelWithTimeout(900)); err != nil {
		t.Fatalf("AddModel: %v", err)
	}

	// GET the redacted view the TUI receives.
	get := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/models", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d", get.Code)
	}
	var views []modelView
	if err := json.Unmarshal(get.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode: %v", err)
	}
	view, ok := findModelView(views, "work")
	if !ok {
		t.Fatal("seeded model absent from GET")
	}
	if view.ModelTimeoutSeconds != 900 {
		t.Fatalf("GET override = %d, want 900 (modelView must carry it)", view.ModelTimeoutSeconds)
	}

	// Edit only the override and PUT the redacted view straight back (the TUI never
	// re-sends the api_key it cannot see).
	view.ModelTimeoutSeconds = 120
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	put := serveOne(t, srv, loopbackReq(http.MethodPut, "/api/models/work", bytes.NewReader(body)))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", put.Code, put.Body.String())
	}

	// Reload from DISK and assert the override persisted (and only it changed).
	cfg, err := config.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got := findModelConfig(cfg, "work")
	if got == nil {
		t.Fatal("model missing from reloaded config")
	}
	if got.ModelTimeoutSeconds != 120 {
		t.Errorf("on-disk override = %d, want 120 (dropped on the PUT path)", got.ModelTimeoutSeconds)
	}
	// The PUT response must also echo it (a second, response-level discriminator).
	var resp modelView
	if err := json.Unmarshal(put.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	if resp.ModelTimeoutSeconds != 120 {
		t.Errorf("PUT response override = %d, want 120", resp.ModelTimeoutSeconds)
	}
}

// TestModelTimeoutSecondsPreservedOnUnrelatedEdit covers the "field survives a
// GET→PUT that didn't touch it" case: a model with an override is round-tripped
// with a DIFFERENT field edited, and the override must remain intact (not wiped to
// 0 by a projection that forgot to forward it).
func TestModelTimeoutSecondsPreservedOnUnrelatedEdit(t *testing.T) {
	srv, home := newModelUpdateServer(t)
	if err := srv.g.AddModel(modelWithTimeout(600)); err != nil {
		t.Fatalf("AddModel: %v", err)
	}
	get := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/models", nil))
	var views []modelView
	_ = json.Unmarshal(get.Body.Bytes(), &views)
	view, _ := findModelView(views, "work")
	if view.ModelTimeoutSeconds != 600 {
		t.Fatalf("GET override = %d, want 600", view.ModelTimeoutSeconds)
	}
	view.DisplayName = "Unrelated Change" // touch a different field only
	body, _ := json.Marshal(view)
	put := serveOne(t, srv, loopbackReq(http.MethodPut, "/api/models/work", bytes.NewReader(body)))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status = %d; body=%s", put.Code, put.Body.String())
	}
	cfg, _ := config.LoadConfig(home)
	got := findModelConfig(cfg, "work")
	if got.DisplayName != "Unrelated Change" {
		t.Errorf("DisplayName = %q, want the unrelated edit applied", got.DisplayName)
	}
	if got.ModelTimeoutSeconds != 600 {
		t.Errorf("override wiped by unrelated edit: got %d, want 600 (must be preserved)", got.ModelTimeoutSeconds)
	}
}

// TestCreateModelPersistsModelTimeoutSeconds covers the POST path (Add Empty… / Add
// from catalog): a freshly created model with an override stores it and serves it.
func TestCreateModelPersistsModelTimeoutSeconds(t *testing.T) {
	srv, home := newModelUpdateServer(t)
	m := modelWithTimeout(333)
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// updateModelRequest embeds config.ModelConfig, so the create body is the flat
	// config the APIClient sends.
	rec := serveOne(t, srv, loopbackReq(http.MethodPost, "/api/models", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp modelView
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode POST response: %v", err)
	}
	if resp.ModelTimeoutSeconds != 333 {
		t.Errorf("POST response override = %d, want 333", resp.ModelTimeoutSeconds)
	}
	cfg, _ := config.LoadConfig(home)
	got := findModelConfig(cfg, "work")
	if got == nil {
		t.Fatal("created model missing from disk")
	}
	if got.ModelTimeoutSeconds != 333 {
		t.Errorf("on-disk override after create = %d, want 333", got.ModelTimeoutSeconds)
	}
}
