package gogent

import (
	"testing"

	"gogent/internal/config"
)

// Issue #296: the Config → Models dialog can mark a model as the default for new
// sessions. This exercises the backend handler the dialog's "Set Default" button
// invokes (g.SetDefaultModel) — validation of the name and persistence across a
// relaunch, mirroring TestSetThemePersistsNoShadow for #215.
func TestSetDefaultModelPersists(t *testing.T) {
	dir := t.TempDir()
	g := NewGogent(dir)
	g.config = &config.Config{
		DefaultModel: "main",
		ModelConfigs: []*config.ModelConfig{
			{Name: "main", Model: "m1"},
			{Name: "alt", Model: "m2"},
		},
	}

	// Unknown model is rejected and leaves the default untouched.
	if err := g.SetDefaultModel("ghost"); err == nil {
		t.Fatalf("SetDefaultModel(ghost) = nil, want error for an unconfigured model")
	}
	if got := g.DefaultModelName(); got != "main" {
		t.Fatalf("default changed to %q after a rejected set, want main", got)
	}

	// A valid, non-default model becomes the default in memory...
	if err := g.SetDefaultModel("alt"); err != nil {
		t.Fatalf("SetDefaultModel(alt): %v", err)
	}
	if got := g.DefaultModelName(); got != "alt" {
		t.Fatalf("DefaultModelName() = %q, want alt", got)
	}

	// ...and persists across a relaunch (reload from disk).
	g2 := NewGogent(dir)
	if got := g2.DefaultModelName(); got != "alt" {
		t.Fatalf("relaunched DefaultModelName() = %q, want alt — SetDefaultModel did not persist", got)
	}
}
