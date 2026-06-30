package ui

import (
	"testing"

	"gogent/internal/config"
	"gogent/internal/gogent"
)

// effortTestModels returns two models for the effort-selector tests: one with
// reasoning-effort options (mirroring GLM-5.2) and one with none (greyed out).
func effortTestModels() []*config.ModelConfig {
	return []*config.ModelConfig{
		{Name: "glm", DisplayName: "GLM", Model: "glm-5.2", Caps: config.ModelCapabilities{EffortOptions: []string{"high", "max"}}},
		{Name: "plain", DisplayName: "Plain", Model: "plain"},
	}
}

// newEffortWorkbench builds a workbench whose first model carries effort options
// and second does not, so a window's effort selector starts enabled.
func newEffortWorkbench(t *testing.T) *Workbench {
	t.Helper()
	return NewWorkbench(effortTestModels())
}

// TestEffortOptionsForModelWithEffort checks a model that offers effort values
// gets the selector "(default) high max" and an enabled control (issue #177).
func TestEffortOptionsForModelWithEffort(t *testing.T) {
	w := newEffortWorkbench(t)
	sw := w.NewSession()

	want := []string{"(default)", "high", "max"}
	got := sw.effortSelect.Options
	if len(got) != len(want) {
		t.Fatalf("effort options = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("effort option[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if !sw.effortEnabled {
		t.Error("effort selector should be enabled for a model with effort options")
	}
	// "(default)" selected => no override.
	if e := sw.selectedEffort(); e != "" {
		t.Errorf("selectedEffort() = %q, want \"\" for (default)", e)
	}
}

// TestEffortSelectorDisabledForModelWithoutOptions checks the selector is greyed
// out (disabled) and reports no override for a model with no effort options.
func TestEffortSelectorDisabledForModelWithoutOptions(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{
		{Name: "plain", DisplayName: "Plain", Model: "plain"},
	})
	sw := w.NewSession()

	if sw.effortEnabled {
		t.Error("effort selector should be disabled for a model with no effort options")
	}
	if got := sw.effortSelect.Options; len(got) != 1 || got[0] != "(default)" {
		t.Errorf("effort options = %v, want [(default)]", got)
	}
	if e := sw.selectedEffort(); e != "" {
		t.Errorf("selectedEffort() = %q, want \"\" when disabled", e)
	}
}

// TestEffortRebuildsOnModelChange checks switching the model rebuilds the effort
// options and enabled state (issue #177): from a no-effort model the selector is
// disabled; selecting the effort model enables it with that model's values.
func TestEffortRebuildsOnModelChange(t *testing.T) {
	w := newEffortWorkbench(t)
	sw := w.NewSession()

	// Select the second (no-effort) model and rebuild.
	sw.modelSelect.SetSelected(1)
	sw.rebuildEffortOptions()
	if sw.effortEnabled {
		t.Fatal("effort selector should disable after switching to a no-effort model")
	}
	if got := sw.effortSelect.Options; len(got) != 1 {
		t.Fatalf("effort options after no-effort model = %v, want [(default)]", got)
	}

	// Switch back to the effort model.
	sw.modelSelect.SetSelected(0)
	sw.rebuildEffortOptions()
	if !sw.effortEnabled {
		t.Fatal("effort selector should re-enable after switching back to an effort model")
	}
	if got := sw.effortSelect.Options; len(got) != 3 {
		t.Errorf("effort options after effort model = %v, want 3 entries", got)
	}
}

// TestEffortSelectionSendsOverride checks the chosen effort is read by the submit
// path's accessor (issue #177): picking "max" makes selectedEffort() return it.
func TestEffortSelectionSendsOverride(t *testing.T) {
	w := newEffortWorkbench(t)
	sw := w.NewSession()

	// index 2 == "max" for the GLM model.
	sw.effortSelect.SetSelected(2)
	if e := sw.selectedEffort(); e != "max" {
		t.Errorf("selectedEffort() = %q, want \"max\"", e)
	}
}

// TestEffortLayoutRoundTrip checks the per-session effort is captured into the
// layout and re-applied on restore (issue #177).
func TestEffortLayoutRoundTrip(t *testing.T) {
	w := newEffortWorkbench(t)
	var saved gogent.Layout
	w.handlers.SaveLayout = func(l gogent.Layout) { saved = l }
	sw := w.NewSession()

	sw.effortSelect.SetSelected(1) // "high"
	w.persistLayout()

	e := saved.Entry(sw.id)
	if e == nil {
		t.Fatalf("layout has no entry for %s", sw.id)
	}
	if e.Effort != "high" {
		t.Fatalf("persisted effort = %q, want \"high\"", e.Effort)
	}

	// Re-apply onto a fresh window (default "(default)") and confirm it restores.
	sw2 := w.NewSession()
	if sw2.selectedEffort() != "" {
		t.Fatalf("fresh window effort = %q, want \"\"", sw2.selectedEffort())
	}
	sw2.applyEffort(e.Effort)
	if got := sw2.selectedEffort(); got != "high" {
		t.Errorf("restored effort = %q, want \"high\"", got)
	}
}

// TestEffortControlHidesOnNarrowWindow checks the effort control collapses (hidden)
// when the window is too narrow to show it without overlapping the model selector,
// and is shown on a wide window (issue #177).
func TestEffortControlHidesOnNarrowWindow(t *testing.T) {
	w := newEffortWorkbench(t)
	sw := w.NewSession()

	// Wide: model selector right edge ~31, effort control fits to the right.
	sw.layoutEffortControl(120, 31)
	if sw.effortHidden {
		t.Error("effort control should be visible on a wide window")
	}
	if sw.effortSelect.Component.Bounds.W == 0 {
		t.Error("effort selector should have non-zero width when visible")
	}

	// Narrow: no room between the model selector and the right edge.
	sw.layoutEffortControl(35, 31)
	if !sw.effortHidden {
		t.Error("effort control should be hidden on a narrow window")
	}
	if sw.effortSelect.Component.Bounds.W != 0 {
		t.Error("effort selector should have zero-width bounds when hidden")
	}
}
