package gogent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogent/internal/config"
	"gogent/internal/model"
)

// Issue #532 — GOAL 1 (save time). AddModel/UpdateModel must REJECT an unroutable
// ModelConfig (no api_type AND no endpoint, or a hosted gateway with no model)
// before any mutation or SaveConfig, wrapping the model-named detail with the
// gogent.ErrModelInvalid sentinel so the HTTP seam maps it to 400 (distinct from
// 409 duplicate-name and 404 not-found). A rejected save leaves g.config untouched
// and writes nothing to disk.

// readConfigBytes returns the contents of <home>/.gogent/config.json, failing the
// test if it is absent. Shared by the save/load tests to assert "no disk write".
func readConfigBytes(t *testing.T, home string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".gogent", "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	return b
}

// assertNoConfigFile fails the test if <home>/.gogent/config.json exists — a
// rejected save must not create the file.
func assertNoConfigFile(t *testing.T, home string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(home, ".gogent", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("config.json should not exist after a rejected save; stat err=%v", err)
	}
}

// TestAddModel_RejectsUnroutable_NoMutationNoDisk is the headline save-time guard.
// A config with no api_type AND no endpoint cannot be persisted; the rejection
// leaves g.config.ModelConfigs untouched and writes nothing to disk.
func TestAddModel_RejectsUnroutable_NoMutationNoDisk(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	before := len(g.Models())

	err := g.AddModel(config.ModelConfig{Name: "bad", APIKey: "k"}) // no api_type, no endpoint

	if err == nil {
		t.Fatal("AddModel of an unroutable config = nil, want error")
	}
	// Classified as invalid (the 400 path) via the sentinel...
	if !errors.Is(err, ErrModelInvalid) {
		t.Errorf("errors.Is(ErrModelInvalid) = false; the rejection must wrap the sentinel, got %v", err)
	}
	// ...and the actionable, model-named detail survives the wrap (errors.As through %w:%w).
	var me *model.ModelError
	if !errors.As(err, &me) {
		t.Errorf("errors.As(*ModelError) = false; the actionable detail must survive the wrap, got %T", err)
	} else if !strings.Contains(me.Message, `model "bad"`) {
		t.Errorf("error must name the model; got %q", me.Message)
	}
	if !strings.Contains(err.Error(), "api_type and endpoint are both empty") {
		t.Errorf("error must explain the routability failure; got %q", err.Error())
	}
	// No mutation, no disk write.
	if got := len(g.Models()); got != before {
		t.Fatalf("Models() count changed after a rejected add: before=%d after=%d", before, got)
	}
	assertNoConfigFile(t, home)
}

// TestAddModel_RejectsHostedGatewayEmptyModel: an openrouter/zai entry with an
// empty model is almost certainly wrong and must not be persistable.
func TestAddModel_RejectsHostedGatewayEmptyModel(t *testing.T) {
	for _, apiType := range []string{"openrouter", "zai"} {
		t.Run(apiType, func(t *testing.T) {
			home := t.TempDir()
			g := NewGogent(home)
			before := len(g.Models())

			err := g.AddModel(config.ModelConfig{Name: "gw", APIType: apiType, APIKey: "k"}) // empty model
			if err == nil {
				t.Fatal("AddModel of a hosted-gateway entry with empty model = nil, want error")
			}
			if !errors.Is(err, ErrModelInvalid) {
				t.Errorf("hosted-gateway empty-model must wrap ErrModelInvalid; got %v", err)
			}
			if len(g.Models()) != before {
				t.Fatalf("Models() changed after a rejected add: before=%d after=%d", before, len(g.Models()))
			}
		})
	}
}

// TestAddModel_ValidPersists: a routable config is still accepted and persisted —
// the guard must not over-reject any previously-valid shape.
func TestAddModel_ValidPersists(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	cfg := config.ModelConfig{Name: "ok", APIType: "openai", Model: "m", Endpoint: "https://api.example.com/v1"}
	if err := g.AddModel(cfg); err != nil {
		t.Fatalf("AddModel of a valid config: %v", err)
	}
	loaded, err := config.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	var found bool
	for _, m := range loaded.ModelConfigs {
		if m != nil && m.Name == "ok" {
			found = true
		}
	}
	if !found {
		t.Fatal("a valid AddModel did not persist the entry to config.json")
	}
}

// TestAddModel_DuplicateNotClassifiedAsInvalid pins the 400/409 boundary: a
// duplicate-name rejection is NOT ErrModelInvalid (it is the plain duplicate error
// that the HTTP seam maps to 409), so the two cases stay distinguishable.
func TestAddModel_DuplicateNotClassifiedAsInvalid(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	valid := config.ModelConfig{Name: "dup", APIType: "openai", Model: "m", Endpoint: "https://api.example.com/v1"}
	if err := g.AddModel(valid); err != nil {
		t.Fatalf("first AddModel: %v", err)
	}
	err := g.AddModel(valid)
	if err == nil {
		t.Fatal("duplicate AddModel = nil, want error")
	}
	if errors.Is(err, ErrModelInvalid) {
		t.Errorf("duplicate-name must NOT be ErrModelInvalid (409, not 400); got %v", err)
	}
}

// TestUpdateModel_RejectsUnroutable_KeepsExisting_NoDisk: a found entry must not
// be overwritten with — or persisted as — an unroutable config; the existing entry
// stays byte-for-byte intact and nothing is written to disk.
func TestUpdateModel_RejectsUnroutable_KeepsExisting_NoDisk(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	if err := g.AddModel(config.ModelConfig{
		Name: "keep", APIType: "openai", Model: "orig-model", Endpoint: "https://orig.example.com/v1",
	}); err != nil {
		t.Fatalf("seed AddModel: %v", err)
	}
	before := readConfigBytes(t, home)

	err := g.UpdateModel(config.ModelConfig{Name: "keep", APIKey: "k"}) // unroutable replacement
	if err == nil {
		t.Fatal("UpdateModel with an unroutable replacement = nil, want error")
	}
	if !errors.Is(err, ErrModelInvalid) {
		t.Errorf("unroutable update must wrap ErrModelInvalid; got %v", err)
	}

	// The existing entry is untouched (still has its model id + endpoint).
	found := false
	for _, m := range g.Models() {
		if m.Name == "keep" {
			found = true
			if m.Model != "orig-model" || m.Endpoint != "https://orig.example.com/v1" {
				t.Errorf("existing entry was mutated by a rejected update: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("the existing entry vanished after a rejected update")
	}

	// No disk write.
	if after := readConfigBytes(t, home); string(after) != string(before) {
		t.Error("config.json was rewritten by a rejected update; it must not be")
	}
}

// TestUpdateModel_NotFoundWinsOverValidation pins the ordering: not-found is
// checked before validation, so an unroutable body for an UNKNOWN name reports
// not-found (404), not invalid (400).
func TestUpdateModel_NotFoundWinsOverValidation(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	err := g.UpdateModel(config.ModelConfig{Name: "ghost", APIKey: "k"}) // unknown name AND unroutable body
	if err == nil {
		t.Fatal("UpdateModel of an unknown model = nil, want error")
	}
	if errors.Is(err, ErrModelInvalid) {
		t.Errorf("not-found must win over validation; got ErrModelInvalid (%v)", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown-name update must report not-found; got %q", err.Error())
	}
}

// TestUpdateModel_ValidOverwritesPersists: a found entry is replaced by a valid
// config and the change is persisted.
func TestUpdateModel_ValidOverwritesPersists(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	if err := g.AddModel(config.ModelConfig{
		Name: "upd", APIType: "openai", Model: "old", Endpoint: "https://old.example.com/v1",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := g.UpdateModel(config.ModelConfig{
		Name: "upd", APIType: "openai", Model: "new", Endpoint: "https://new.example.com/v1",
	}); err != nil {
		t.Fatalf("UpdateModel valid: %v", err)
	}
	found := false
	for _, m := range g.Models() {
		if m.Name == "upd" {
			found = true
			if m.Model != "new" || m.Endpoint != "https://new.example.com/v1" {
				t.Errorf("valid update did not overwrite in memory: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("updated entry not found after a valid update")
	}
	// And it landed on disk.
	loaded, err := config.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	for _, m := range loaded.ModelConfigs {
		if m != nil && m.Name == "upd" && m.Endpoint == "https://new.example.com/v1" {
			return // persisted
		}
	}
	t.Error("valid update did not persist the new endpoint to config.json")
}
