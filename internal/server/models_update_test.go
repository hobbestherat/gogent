package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"gogent/internal/config"
	"gogent/internal/gogent"
)

// Issues #537 / #538 — regression coverage for the remote (daemon) model-edit
// round-trip. webapi binds the JSON request body ONLY into the handler's index-1
// parameter, so modelsSvc.Update must declare the body struct SECOND and the
// :name path param THIRD: Update(r, req, name). The old (r, name, req) order made
// webapi skip the body and persist a near-zero updateModelRequest, silently
// wiping every editable field on Models… → Edit → Save (only the path name and,
// via the empty-key-preserve rule, the api_key survived).
//
// issue532_models_validation_test.go already pins the body-binding essence
// (TestUpdateModel_ValidBody_Mutates_BodyIsBound), the unroutable→400 path and
// the not-found→404 path. This file adds the #538 deliverables that are NOT
// covered there:
//   - a MAXIMAL all-fields round-trip (every ModelConfig field, not just two),
//   - the empty-key-preserve path across a redacted GET→PUT (the TUI never
//     re-sends the api_key it cannot see),
//   - a DISK-level assertion via config.LoadConfig (what actually persists, not
//     just the in-memory GET view),
//   - the faithful client emulation (re-marshal the redacted modelView the TUI
//     receives, then PUT it back),
// plus the inverse key-replace branch, the path-name-wins invariant and the
// human-scope guard.

// newModelUpdateServer builds a loopback (human-scoped) /api server over a fresh,
// isolated core (the manual idiom from default_model_issue507_test.go) and returns
// the server plus its home dir. The home is needed to reload the persisted config
// via config.LoadConfig; srv.g is the live core for seeding via g.AddModel. No
// model provider is contacted — only config CRUD over the loopback /api — so,
// unlike newTestServer, no GOGENT_MODEL_URL fake backend is required.
func newModelUpdateServer(t *testing.T) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	srv := NewServer(gogent.NewGogent(home), Options{Password: "x"})
	return srv, home
}

// seedModel is a fully-configured, routable model that populates EVERY ModelConfig
// field so the round-trip assertion surface is complete and robust to future
// additions. The non-empty endpoint+model+api_type make it routable, so it passes
// the #532 save-time validation AddModel/UpdateModel enforce. Every omitempty
// field is given a non-zero value so it actually serialises and survives the
// round-trip (a zero value would be omitted and silently pass an equality check).
func seedModel() config.ModelConfig {
	return config.ModelConfig{
		Name:            "work",
		DisplayName:     "Work",
		Model:           "gpt-x",
		Endpoint:        "https://api.example.com/v1",
		APIType:         "openai",
		APIKey:          "seed-secret-key",
		Temperature:     0.4,
		TopP:            0.9,
		MaxTokens:       2048,
		ContextWindow:   128000,
		ReasoningEffort: "medium",
		EffortOptions:   []string{"low", "medium", "high"},
		Thinking:        boolPtr(true),
		Project:         "proj",
		Location:        "us-central1",
		Free:            true,
	}
}

func boolPtr(b bool) *bool { return &b }

// TestUpdateModelRoundTripPersistsAllFields is the #538 headline guard. It drives
// the exact round-trip the TUI performs — GET the redacted view, edit one field,
// re-marshal and PUT it back WITHOUT the api_key — then reloads config from disk
// and asserts EVERY field persisted: the one edited field changed, every other
// non-redacted field is unchanged, and the api_key is PRESERVED despite being
// absent from the PUT body. It fails on the old (r, name, req) ordering (the body
// is dropped → a near-zero config → either a 400 from #532 validation or a wiped
// entry) and passes on the fixed (r, req, name) ordering.
func TestUpdateModelRoundTripPersistsAllFields(t *testing.T) {
	srv, home := newModelUpdateServer(t)
	seed := seedModel()
	if err := srv.g.AddModel(seed); err != nil {
		t.Fatalf("AddModel seed: %v", err)
	}

	// 2. GET /api/models — the redacted modelView array the TUI receives.
	get := serveOne(t, srv, loopbackReq(http.MethodGet, "/api/models", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET /models status = %d; body=%s", get.Code, get.Body.String())
	}
	var views []modelView
	if err := json.Unmarshal(get.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode []modelView: %v (body=%s)", err, get.Body.String())
	}
	view, ok := findModelView(views, "work")
	if !ok {
		t.Fatalf("seeded model %q absent from GET /models", "work")
	}
	if !view.HasAPIKey {
		t.Error("GET has_api_key = false, want true (the seed has a key)")
	}
	if bytes.Contains(get.Body.Bytes(), []byte(seed.APIKey)) {
		t.Error("GET /models leaked the api_key in the redacted view")
	}

	// 3. Re-marshal the redacted view as the PUT body, mutating exactly one field.
	// modelView carries no api_key, so the body omits it — exactly what the TUI
	// does — which also exercises the empty-key-preserves-stored-key path.
	view.DisplayName = "Work Renamed"
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal PUT body: %v", err)
	}

	// 4. PUT /api/models/work.
	put := serveOne(t, srv, loopbackReq(http.MethodPut, "/api/models/work", bytes.NewReader(body)))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT /models/work status = %d, want 200 (a 400 means the body was dropped — signature regression); body=%s",
			put.Code, put.Body.String())
	}
	if bytes.Contains(put.Body.Bytes(), []byte(seed.APIKey)) {
		t.Error("PUT response leaked the api_key")
	}

	// 5. Reload config FROM DISK and assert the full round-trip.
	cfg, err := config.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got := findModelConfig(cfg, "work")
	if got == nil {
		t.Fatal("model \"work\" missing from the reloaded on-disk config")
	}
	want := seed
	want.DisplayName = "Work Renamed" // the one edited field
	assertModelEquals(t, "round-trip", *got, want)

	// 6. The PUT response must be a non-empty modelView reflecting the update —
	// not the zero-value husk #537 produced (empty model/endpoint). This is a
	// second, response-level discriminator independent of the disk reload.
	var resp modelView
	if err := json.Unmarshal(put.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode PUT response: %v (body=%s)", err, put.Body.String())
	}
	if resp.Name != "work" {
		t.Errorf("response name = %q, want work", resp.Name)
	}
	if resp.DisplayName != "Work Renamed" {
		t.Errorf("response display_name = %q, want Work Renamed", resp.DisplayName)
	}
	if resp.Model == "" || resp.Endpoint == "" {
		t.Errorf("response is a zero-value husk: model=%q endpoint=%q", resp.Model, resp.Endpoint)
	}
	if !resp.HasAPIKey {
		t.Error("response has_api_key = false, want true (the stored key was preserved)")
	}
}

// TestUpdateModelNewAPIKeyReplacesStoredKey covers the INVERSE of the preserve
// rule: when the PUT body carries a non-empty api_key (a user who re-enters a
// key, or a fresh add), the stored key must be REPLACED, not borrowed. It also
// proves the handler accepts a flat config.ModelConfig body (updateModelRequest
// embeds it anonymously) — the wire shape APIClient.UpdateModel actually sends.
func TestUpdateModelNewAPIKeyReplacesStoredKey(t *testing.T) {
	srv, home := newModelUpdateServer(t)
	if err := srv.g.AddModel(seedModel()); err != nil {
		t.Fatalf("AddModel seed: %v", err)
	}

	withNewKey := seedModel()
	withNewKey.APIKey = "brand-new-key" // non-empty → must replace, not preserve
	body, err := json.Marshal(withNewKey)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	rec := serveOne(t, srv, loopbackReq(http.MethodPut, "/api/models/work", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	cfg, err := config.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got := findModelConfig(cfg, "work")
	if got == nil {
		t.Fatal("model \"work\" missing from reloaded config")
	}
	if got.APIKey != "brand-new-key" {
		t.Errorf("APIKey = %q, want %q (a non-empty key must replace the stored key)", got.APIKey, "brand-new-key")
	}
}

// TestUpdateModelPathNameWinsOverBodyName pins the path-name-wins invariant after
// the reorder: the :name path param must bind the string arg and override any
// "name" field in the JSON body. A PUT to /models/work carrying a body whose name
// is "imposter" must update "work" (not create "imposter"), leaving the model
// count unchanged. The existing body-binding test uses the same name in body and
// path, so it does not exercise a divergent body name.
func TestUpdateModelPathNameWinsOverBodyName(t *testing.T) {
	srv, home := newModelUpdateServer(t)
	if err := srv.g.AddModel(seedModel()); err != nil {
		t.Fatalf("AddModel seed: %v", err)
	}
	before := len(configModelNames(t, home))

	body, err := json.Marshal(config.ModelConfig{
		Name:        "imposter", // must be discarded — the path name wins
		DisplayName: "Path Wins",
		APIType:     "openai",
		Model:       "gpt-x",
		Endpoint:    "https://api.example.com/v1",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	rec := serveOne(t, srv, loopbackReq(http.MethodPut, "/api/models/work", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	cfg, err := config.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if findModelConfig(cfg, "imposter") != nil {
		t.Error("the body's name field leaked: a model named \"imposter\" was created; the path name must win")
	}
	got := findModelConfig(cfg, "work")
	if got == nil {
		t.Fatal("model \"work\" vanished; the update must apply to the path name")
	}
	if got.Name != "work" {
		t.Errorf("Name = %q, want work (path name must win over the body name)", got.Name)
	}
	if got.DisplayName != "Path Wins" {
		t.Errorf("DisplayName = %q, want Path Wins (the body must still be applied)", got.DisplayName)
	}
	if after := len(configModelNames(t, home)); after != before {
		t.Errorf("model count changed: got %d want %d (path-name-wins must not create a new entry)", after, before)
	}
}

// TestUpdateModelRequiresHuman mirrors the create/delete human-scope guards: PUT
// /models is gated on the human scope, so an anonymous remote caller is rejected
// (401) and the model is NOT mutated.
func TestUpdateModelRequiresHuman(t *testing.T) {
	srv, home := newModelUpdateServer(t)
	if err := srv.g.AddModel(seedModel()); err != nil {
		t.Fatalf("AddModel seed: %v", err)
	}

	body, err := json.Marshal(seedModel())
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	r := httptest.NewRequest(http.MethodPut, "/api/models/work", bytes.NewReader(body))
	r.RemoteAddr = "10.0.0.5:1" // non-loopback, no credential
	r.Header.Set("Content-Type", "application/json")
	rec := serveOne(t, srv, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous PUT status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}

	// The rejected update must not have mutated the stored model.
	cfg, err := config.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got := findModelConfig(cfg, "work")
	if got == nil {
		t.Fatal("model \"work\" missing after a rejected anonymous PUT")
	}
	if got.DisplayName != "Work" {
		t.Errorf("DisplayName = %q, want Work (an anonymous PUT must not mutate the model)", got.DisplayName)
	}
}

// --- helpers ----------------------------------------------------------------

func findModelView(views []modelView, name string) (modelView, bool) {
	for i := range views {
		if views[i].Name == name {
			return views[i], true
		}
	}
	return modelView{}, false
}

func findModelConfig(cfg *config.Config, name string) *config.ModelConfig {
	for _, m := range cfg.ModelConfigs {
		if m != nil && m.Name == name {
			return m
		}
	}
	return nil
}

func configModelNames(t *testing.T, home string) []string {
	t.Helper()
	cfg, err := config.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	names := make([]string, 0, len(cfg.ModelConfigs))
	for _, m := range cfg.ModelConfigs {
		if m != nil {
			names = append(names, m.Name)
		}
	}
	return names
}

// assertModelEquals compares every ModelConfig field. For the round-trip test the
// caller mutates the single edited field on the `want` seed first, so a match
// means "exactly the edited field changed, everything else (including the
// preserved api_key) round-tripped unchanged."
func assertModelEquals(t *testing.T, label string, got, want config.ModelConfig) {
	t.Helper()
	checks := []struct {
		name string
		ok   bool
	}{
		{"Name", got.Name == want.Name},
		{"DisplayName", got.DisplayName == want.DisplayName},
		{"Model", got.Model == want.Model},
		{"Endpoint", got.Endpoint == want.Endpoint},
		{"APIType", got.APIType == want.APIType},
		{"APIKey", got.APIKey == want.APIKey},
		{"Temperature", got.Temperature == want.Temperature},
		{"TopP", got.TopP == want.TopP},
		{"MaxTokens", got.MaxTokens == want.MaxTokens},
		{"ContextWindow", got.ContextWindow == want.ContextWindow},
		{"ReasoningEffort", got.ReasoningEffort == want.ReasoningEffort},
		{"EffortOptions", reflect.DeepEqual(got.EffortOptions, want.EffortOptions)},
		{"Thinking", reflect.DeepEqual(got.Thinking, want.Thinking)},
		{"Project", got.Project == want.Project},
		{"Location", got.Location == want.Location},
		{"Free", got.Free == want.Free},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s: %s mismatch (got %+v, want %+v)", label, c.name, got, want)
		}
	}
}
