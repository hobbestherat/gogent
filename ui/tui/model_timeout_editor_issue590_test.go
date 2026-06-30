package ui

import (
	"testing"

	"gogent/internal/config"
)

// Issue #590 Part B (UI) — the model add/edit form gained an optional "Model
// timeout:" field (showModelForm) that seeds from ModelTimeoutSeconds and writes it
// back on save. These drive the REAL form (open it, type, click Save via the same
// helpers the #389/#509 tests use) and capture what onSave receives, so a wiring or
// save-logic regression in the editor is caught at the seam — not just at the config
// accessor.
//
// Save-logic branches under test:
//   - a set override is seeded into the field and preserved on save;
//   - an unset override seeds a BLANK field and saves back as 0 (use-global);
//   - typing a value into the blank field sets the override;
//   - garbage appended to a seeded value leaves the prior value untouched.
//
// Hermetic: no ScanModels backend is wired (the Scan button is omitted), so no
// network is contacted.

// openModelTimeoutForm opens the model form in edit mode over initial and returns a
// pointer to the cfg onSave receives (nil until Save is clicked).
func openModelTimeoutForm(t *testing.T, w *Workbench, initial config.ModelConfig) *config.ModelConfig {
	t.Helper()
	var captured *config.ModelConfig
	// No app.Resize: clickModelFormSave locates the Save button by its dialog-relative
	// bounds (root.Bounds.W-24), and an explicit resize changes the resolved width so
	// that helper can click the wrong widget. The default workbench size (cf. the
	// #389/#509 tests) places the button where the helper expects.
	w.showModelForm("Edit model — "+initial.Name, initial, false, /* nameEditable */
		func(cfg config.ModelConfig) error {
			c := cfg
			captured = &c
			return nil
		},
		nil) // onSaved: not needed; onSave capture is the assertion surface
	return captured
}

func TestModelTimeoutFieldSeedsAndPreservesOverrideOnSave(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "slow", Model: "m"}})
	initial := config.ModelConfig{Name: "slow", Model: "m", ModelTimeoutSeconds: 600}
	captured := openModelTimeoutForm(t, w, initial)
	clickModelFormSave(t, w)
	if captured == nil {
		t.Fatal("Save did not invoke onSave (validation rejected a named model?)")
	}
	if captured.ModelTimeoutSeconds != 600 {
		t.Errorf("saved ModelTimeoutSeconds = %d, want 600 (field must seed from the override and preserve it)", captured.ModelTimeoutSeconds)
	}
}

func TestModelTimeoutFieldUnsetSeedsBlankAndSavesZero(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "fast", Model: "m"}})
	initial := config.ModelConfig{Name: "fast", Model: "m"} // ModelTimeoutSeconds == 0
	captured := openModelTimeoutForm(t, w, initial)
	clickModelFormSave(t, w)
	if captured == nil {
		t.Fatal("Save did not invoke onSave")
	}
	if captured.ModelTimeoutSeconds != 0 {
		t.Errorf("saved ModelTimeoutSeconds = %d, want 0 (unset override seeds blank and saves 0 = use global)", captured.ModelTimeoutSeconds)
	}
}

func TestModelTimeoutFieldTypingANewValueSetsTheOverride(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	initial := config.ModelConfig{Name: "m", Model: "m"} // blank field
	captured := openModelTimeoutForm(t, w, initial)
	typeCatalogField(t, w, "Model timeout:", "999")
	clickModelFormSave(t, w)
	if captured == nil {
		t.Fatal("Save did not invoke onSave")
	}
	if captured.ModelTimeoutSeconds != 999 {
		t.Errorf("saved ModelTimeoutSeconds = %d, want 999 (typed value must set the override)", captured.ModelTimeoutSeconds)
	}
}

func TestModelTimeoutFieldTypingZeroExplicitlyClearsToGlobal(t *testing.T) {
	// Typing "0" (rather than leaving it blank) must also resolve to 0 = use global,
	// so a user who explicitly enters 0 is not surprised.
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	initial := config.ModelConfig{Name: "m", Model: "m"}
	captured := openModelTimeoutForm(t, w, initial)
	typeCatalogField(t, w, "Model timeout:", "0")
	clickModelFormSave(t, w)
	if captured == nil {
		t.Fatal("Save did not invoke onSave")
	}
	if captured.ModelTimeoutSeconds != 0 {
		t.Errorf("saved ModelTimeoutSeconds = %d, want 0 (explicit 0 = use global)", captured.ModelTimeoutSeconds)
	}
}

func TestModelTimeoutFieldGarbageLeavesPriorValueUntouched(t *testing.T) {
	// A seeded override of 600; appending garbage yields "600abc", which Atoi rejects,
	// so the save closure leaves the prior value (600) in place — a stray keystroke
	// can neither wipe nor corrupt the override.
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	initial := config.ModelConfig{Name: "m", Model: "m", ModelTimeoutSeconds: 600}
	captured := openModelTimeoutForm(t, w, initial)
	typeCatalogField(t, w, "Model timeout:", "abc") // box now reads "600abc"
	clickModelFormSave(t, w)
	if captured == nil {
		t.Fatal("Save did not invoke onSave")
	}
	if captured.ModelTimeoutSeconds != 600 {
		t.Errorf("saved ModelTimeoutSeconds = %d, want 600 (garbage must leave the prior value untouched)", captured.ModelTimeoutSeconds)
	}
}
