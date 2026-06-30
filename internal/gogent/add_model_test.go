package gogent

import (
	"testing"

	"gogent/internal/config"
)

// Issue #486: the catalog-assisted "Add model" flow needs a CREATE path. Until
// now UpdateModel could only replace an existing entry. AddModel must append a
// new entry, persist it, and refuse to clobber an existing name — these guard
// the no-clobber guarantee the POST /api/models 409 semantics rely on.

func TestAddModelAppendsAndPersists(t *testing.T) {
	dir := t.TempDir()
	g := NewGogent(dir)

	before := len(g.Models())
	cfg := config.ModelConfig{
		Name:        "catalog-test-opus",
		DisplayName: "Opus (catalog)",
		Connection:  "local-lan",
		Model:       "claude-opus-4-6",
		Temperature: 0.7,
		MaxTokens:   8192,
		Caps:        config.ModelCapabilities{ContextWindow: 200000},
	}
	if err := g.AddModel(cfg); err != nil {
		t.Fatalf("AddModel: %v", err)
	}

	after := len(g.Models())
	if after != before+1 {
		t.Fatalf("model count before=%d after=%d, want %d", before, after, before+1)
	}

	// The new entry is visible through Models().
	found := false
	for _, m := range g.Models() {
		if m.Name == "catalog-test-opus" {
			found = true
			if m.Model != "claude-opus-4-6" {
				t.Errorf("stored Model = %q, want claude-opus-4-6", m.Model)
			}
		}
	}
	if !found {
		t.Fatal("added model not returned by Models()")
	}

	// And it survives a reload from disk (AddModel persisted via SaveConfig).
	loaded, err := config.LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	persisted := false
	for _, m := range loaded.ModelConfigs {
		if m != nil && m.Name == "catalog-test-opus" {
			persisted = true
		}
	}
	if !persisted {
		t.Fatal("AddModel did not persist the new entry to config.json")
	}
}

func TestAddModelRejectsDuplicateName(t *testing.T) {
	dir := t.TempDir()
	g := NewGogent(dir)

	// A routable config (references a seeded routable connection) so save-time
	// validation (issue #532) is satisfied — this test exercises the
	// duplicate-name guard, not validation.
	cfg := config.ModelConfig{Name: "catalog-dup", Model: "m1", Connection: "local-lan"}
	if err := g.AddModel(cfg); err != nil {
		t.Fatalf("first AddModel: %v", err)
	}
	// A second add with the same Name must be rejected (the authority behind the
	// 409 the HTTP layer returns).
	if err := g.AddModel(cfg); err == nil {
		t.Fatal("second AddModel of the same name = nil, want an error")
	}

	// Exactly one entry for that name survives (no silent clobber/append).
	count := 0
	for _, m := range g.Models() {
		if m.Name == "catalog-dup" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("found %d entries named catalog-dup, want 1", count)
	}
}

// AddModel must not collide with any pre-existing (seeded default) name either.
func TestAddModelRejectsDuplicateOfExistingSeededName(t *testing.T) {
	dir := t.TempDir()
	g := NewGogent(dir)
	existing := g.Models()
	if len(existing) == 0 {
		t.Skip("no seeded models to collide with")
	}
	dup := existing[0]
	dup.DisplayName = "should-not-overwrite"
	// Trying to re-add an existing name fails; the original is untouched.
	if err := g.AddModel(dup); err == nil {
		t.Fatal("AddModel of an existing seeded name = nil, want error")
	}
}

func TestHomeDirReturnsConstructorDir(t *testing.T) {
	dir := t.TempDir()
	g := NewGogent(dir)
	if got := g.HomeDir(); got != dir {
		t.Errorf("HomeDir() = %q, want %q", got, dir)
	}
}
