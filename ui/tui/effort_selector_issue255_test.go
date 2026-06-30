package ui

import (
	"testing"

	"gogent/internal/config"
	"gogent/internal/gogent"
)

// Tests for issue #255: a new session's Effort selector must show the selected
// model's configured reasoning_effort (when it is one of the model's
// effort_options) instead of the "(default)" sentinel, while still preserving an
// explicit user pick that a switched-to model still offers, and falling back to
// "(default)" otherwise.
//
// The behaviour lives in (sw *SessionWindow).rebuildEffortOptions(), which runs
// at window construction (newSessionWindow) and on every model change
// (modelSelect.OnChange). These tests exercise both entry points as well as the
// persistence round-trip and the degenerate nil/disabled cases.
//
// Run on a Pi5 without -race, per the issue constraints.

// effort255Models returns a fixed set of model configs that branch on every
// condition rebuildEffortOptions decides on. Index 0 (the default-selected
// model) mirrors the issue's GLM-5.2 example.
func effort255Models() []*config.ModelConfig {
	return []*config.ModelConfig{
		// 0 — GLM-5.2 from the issue: configured "high", offered in options.
		{Name: "glm", DisplayName: "GLM", Model: "glm-5.2", ReasoningEffort: "high", Caps: config.ModelCapabilities{EffortOptions: []string{"high", "max"}}},
		// 1 — configured value NOT among its offered options (-> fallback).
		{Name: "cfg-other", DisplayName: "CfgOther", Model: "m-other", ReasoningEffort: "ultra", Caps: config.ModelCapabilities{EffortOptions: []string{"high", "max"}}},
		// 2 — reasoning model with options but no configured reasoning_effort.
		{Name: "no-cfg", DisplayName: "NoCfg", Model: "m-nocfg", Caps: config.ModelCapabilities{EffortOptions: []string{"high", "max"}}},
		// 3 — reasoning_effort set but no effort_options (-> greyed-out).
		{Name: "disabled", DisplayName: "Disabled", Model: "m-disabled", ReasoningEffort: "high"},
		// 4 — plain non-reasoning model (no effort control at all).
		{Name: "plain", DisplayName: "Plain", Model: "plain"},
		// 5 — only offers "max", configured "max".
		{Name: "max-only", DisplayName: "MaxOnly", Model: "m-maxonly", ReasoningEffort: "max", Caps: config.ModelCapabilities{EffortOptions: []string{"max"}}},
		// 6 — only offers "high", configured "high".
		{Name: "high-only", DisplayName: "HighOnly", Model: "m-highonly", ReasoningEffort: "high", Caps: config.ModelCapabilities{EffortOptions: []string{"high"}}},
	}
}

// newEffort255Workbench builds a workbench on the #255 model set whose first
// (default) model is the GLM-5.2-like reasoning model, so a fresh session starts
// enabled and on a model with a configured effort.
func newEffort255Workbench(t *testing.T) *Workbench {
	t.Helper()
	return NewWorkbench(effort255Models())
}

// switchModel255 moves the model selector and fires its OnChange, mirroring the
// real commit() path (SetSelected then OnChange -> rebuildEffortOptions).
func switchModel255(t *testing.T, sw *SessionWindow, idx int) {
	t.Helper()
	sw.modelSelect.SetSelected(idx)
	if sw.modelSelect.OnChange != nil {
		sw.modelSelect.OnChange(idx)
	}
}

// pickEffort255 sets the effort selector as if the user chose an option from the
// dropdown (SetSelected == a committed pick; it does not itself rebuild).
func pickEffort255(t *testing.T, sw *SessionWindow, idx int) {
	t.Helper()
	sw.effortSelect.SetSelected(idx)
}

// --- Fresh-session seeding (the core fix) -------------------------------

// TestEffort255_FreshSessionSeedsConfiguredEffort is the headline assertion of
// the issue: a brand-new session on GLM-5.2 (configured reasoning_effort "high",
// offered in effort_options) shows "high" — never the "(default)" sentinel.
func TestEffort255_FreshSessionSeedsConfiguredEffort(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.NewSession()

	if got := sw.effortSelect.Value(); got != "high" {
		t.Errorf("effort selector value = %q, want %q (the model's configured reasoning_effort)", got, "high")
	}
	if sw.effortSelect.Selected != 1 {
		t.Errorf("effort selector Selected index = %d, want 1 (index of \"high\" in [(default),high,max])", sw.effortSelect.Selected)
	}
	// The selector must reflect the seed through the accessor the submit path
	// reads — the bug was that the label showed "(default)" i.e. selectedEffort "".
	if got := sw.selectedEffort(); got != "high" {
		t.Errorf("selectedEffort() = %q, want %q", got, "high")
	}
	if !sw.effortEnabled {
		t.Error("effort selector must be enabled for a model that offers effort options")
	}
}

// TestEffort255_SentinelStaysInMenu confirms the "(default)" opt-in is retained
// as the first menu entry even when the configured value seeds the selection
// (the issue recommends keeping it as a manual "track the config" choice).
func TestEffort255_SentinelStaysInMenu(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.NewSession()

	want := []string{"(default)", "high", "max"}
	got := sw.effortSelect.Options
	if len(got) != len(want) {
		t.Fatalf("options = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("options[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// --- Fallbacks to "(default)" -------------------------------------------

// TestEffort255_FallbackWhenConfiguredValueNotOffered: a model whose configured
// reasoning_effort is not among its effort_options must fall back to
// "(default)" rather than inventing an entry or selecting a wrong index.
//
// To exercise the seed branch (rather than prev-preservation) we arrive at the
// model from the disabled "Plain" model, so the carried prev is the "(default)"
// sentinel — the same state a fresh session on that model would be in.
func TestEffort255_FallbackWhenConfiguredValueNotOffered(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.NewSession()
	switchModel255(t, sw, 4) // Plain: prev becomes the "(default)" sentinel
	switchModel255(t, sw, 1) // CfgOther: ReasoningEffort "ultra", options [high,max]

	if got := sw.effortSelect.Value(); got != "(default)" {
		t.Errorf("value = %q, want %q (configured \"ultra\" is not offered)", got, "(default)")
	}
	if sw.effortSelect.Selected != 0 {
		t.Errorf("Selected = %d, want 0 for an un-offered configured value", sw.effortSelect.Selected)
	}
	if got := sw.selectedEffort(); got != "" {
		t.Errorf("selectedEffort() = %q, want \"\" for (default)", got)
	}
	if !sw.effortEnabled {
		t.Error("selector should remain enabled — it still offers high/max")
	}
}

// TestEffort255_FallbackWhenNoConfiguredEffort: a model with effort options but
// no configured reasoning_effort stays on "(default)". (Arrived at from the
// Plain model so the carried prev is the sentinel, exercising the seed branch.)
func TestEffort255_FallbackWhenNoConfiguredEffort(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.NewSession()
	switchModel255(t, sw, 4) // Plain: prev becomes the "(default)" sentinel
	switchModel255(t, sw, 2) // NoCfg: no ReasoningEffort, options [high,max]

	if got := sw.effortSelect.Value(); got != "(default)" {
		t.Errorf("value = %q, want %q (no configured effort to seed)", got, "(default)")
	}
	if got := sw.selectedEffort(); got != "" {
		t.Errorf("selectedEffort() = %q, want \"\"", got)
	}
	if !sw.effortEnabled {
		t.Error("selector should remain enabled — it still offers high/max")
	}
}

// TestEffort255_SeededValueIsStickyAcrossSwitch documents a deliberate, subtle
// consequence of the plan's operational definition: once a value is seeded into
// the selector (here GLM's configured "high"), switching to a model that STILL
// OFFERS that value preserves it — even though it was a config seed, not a human
// pick. rebuildEffortOptions cannot distinguish a seeded value from an explicit
// one (both are just effortSelect.Value()), so step 1 (prev-preservation) wins.
//
// This matches the plan ("prev is a real value… and the new model still offers
// it, keep it"). If you intend ONLY human picks to survive a switch, provenance
// tracking would be needed — flagged as a design note, not asserted as a bug.
func TestEffort255_SeededValueIsStickyAcrossSwitch(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.NewSession()
	if sw.effortSelect.Value() != "high" {
		t.Fatalf("precondition: seeded value = %q, want high", sw.effortSelect.Value())
	}
	// CfgOther offers "high" too; the seeded "high" therefore survives the switch
	// rather than being re-seeded (CfgOther's own config "ultra" is never reached).
	switchModel255(t, sw, 1)
	if got := sw.effortSelect.Value(); got != "high" {
		t.Errorf("seeded value across switch = %q, want %q (preserved as a carried prev)", got, "high")
	}
}

// TestEffort255_DisabledWhenNoEffortOptions: a model with a configured
// reasoning_effort but no effort_options has a disabled selector that still
// reports "(default)" and no override — the configured value is applied by the
// backend, never pinned by the selector.
func TestEffort255_DisabledWhenNoEffortOptions(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.NewSession()
	switchModel255(t, sw, 3) // Disabled: ReasoningEffort "high", no options

	if sw.effortEnabled {
		t.Error("selector should be disabled for a model with no effort options")
	}
	if got := sw.effortSelect.Options; len(got) != 1 || got[0] != "(default)" {
		t.Errorf("options = %v, want [(default)]", got)
	}
	if got := sw.effortSelect.Value(); got != "(default)" {
		t.Errorf("value = %q, want %q", got, "(default)")
	}
	if got := sw.selectedEffort(); got != "" {
		t.Errorf("selectedEffort() = %q, want \"\" when disabled", got)
	}
}

// TestEffort255_NonReasoningModelUnchanged: a plain non-reasoning model leaves
// the selector disabled on "(default)" — unchanged by this fix.
func TestEffort255_NonReasoningModelUnchanged(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.NewSession()
	switchModel255(t, sw, 4) // Plain

	if sw.effortEnabled {
		t.Error("selector should be disabled for a non-reasoning model")
	}
	if got := sw.effortSelect.Value(); got != "(default)" {
		t.Errorf("value = %q, want %q", got, "(default)")
	}
}

// --- Model-switch seeding -----------------------------------------------

// TestEffort255_SwitchFromSentinelSeedsConfig: arriving at a reasoning model
// from a state whose prior value was the "(default)" sentinel (either a fresh
// "(default)" model or a disabled model) seeds that model's configured effort.
// This is the model-switch half of the fix.
func TestEffort255_SwitchFromSentinelSeedsConfig(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.NewSession()

	// Land on the plain (disabled) model first: prev becomes "(default)".
	switchModel255(t, sw, 4)
	if sw.effortSelect.Value() != "(default)" {
		t.Fatalf("plain model value = %q, want (default) before switching back", sw.effortSelect.Value())
	}

	// Switch to GLM: prior "(default)" must NOT block seeding "high".
	switchModel255(t, sw, 0)
	if got := sw.effortSelect.Value(); got != "high" {
		t.Errorf("after switching to GLM from (default): value = %q, want %q", got, "high")
	}
	if got := sw.selectedEffort(); got != "high" {
		t.Errorf("selectedEffort() = %q, want %q", got, "high")
	}
}

// TestEffort255_ExplicitPickSurvivesSwitch: an explicit user pick that the
// switched-to model still offers is preserved — it is NOT overwritten by the new
// model's configured value. (Unchanged behaviour, now coexisting with seeding.)
func TestEffort255_ExplicitPickSurvivesSwitch(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.NewSession()

	pickEffort255(t, sw, 2) // choose "max" on GLM
	if sw.effortSelect.Value() != "max" {
		t.Fatalf("precondition: pick value = %q, want max", sw.effortSelect.Value())
	}

	// CfgOther offers "max" but is itself configured "ultra" (not offered). Only the
	// preserve branch yields "max" here — the config-seed branch would fall to
	// "(default)". This makes the assertion discriminating, not a tautology.
	switchModel255(t, sw, 1)
	if got := sw.effortSelect.Value(); got != "max" {
		t.Errorf("after switching to a model still offering the pick: value = %q, want %q", got, "max")
	}
}

// TestEffort255_ExplicitPickNotOfferedSeedsNewConfig: when the switched-to model
// does NOT offer the prior explicit pick, the selection falls through to that new
// model's configured effort (not left dangling, not stuck on "(default)" if the
// model has a configured value to seed).
func TestEffort255_ExplicitPickNotOfferedSeedsNewConfig(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.NewSession()

	pickEffort255(t, sw, 2) // "max" on GLM
	// highOnly does not offer "max"; it is configured "high".
	switchModel255(t, sw, 6)
	if got := sw.effortSelect.Value(); got != "high" {
		t.Errorf("after switching to highOnly (configured high): value = %q, want %q", got, "high")
	}
	if got := sw.selectedEffort(); got != "high" {
		t.Errorf("selectedEffort() = %q, want %q", got, "high")
	}
}

// TestEffort255_SwitchToPlainClearsToDefault: leaving a reasoning model (whose
// effort was seeded) for a plain model drops back to a disabled "(default)".
func TestEffort255_SwitchToPlainClearsToDefault(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.NewSession()
	if sw.effortSelect.Value() != "high" {
		t.Fatalf("precondition: seeded value = %q, want high", sw.effortSelect.Value())
	}

	switchModel255(t, sw, 4) // Plain
	if sw.effortEnabled {
		t.Error("selector should be disabled after switching to a plain model")
	}
	if got := sw.effortSelect.Value(); got != "(default)" {
		t.Errorf("value = %q, want %q", got, "(default)")
	}
	if got := sw.selectedEffort(); got != "" {
		t.Errorf("selectedEffort() = %q, want \"\"", got)
	}
}

// TestEffort255_RebuildIsIdempotent: calling rebuildEffortOptions again on the
// same model does not flip the seeded value. (Guards against a future change
// that re-seeds every redraw and clobbers the user's view.)
func TestEffort255_RebuildIsIdempotent(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.NewSession()
	before := sw.effortSelect.Selected

	sw.rebuildEffortOptions()
	sw.rebuildEffortOptions()

	if sw.effortSelect.Selected != before {
		t.Errorf("Selected changed across idempotent rebuilds: got %d, want %d", sw.effortSelect.Selected, before)
	}
	if got := sw.effortSelect.Value(); got != "high" {
		t.Errorf("value after rebuild = %q, want %q", got, "high")
	}
}

// TestEffort255_ExplicitPickHeldAcrossRebuild: once an explicit value is picked,
// re-running rebuildEffortOptions (same model) keeps it — the preserve path must
// beat the config-seed path for a real (non-sentinel) prior value.
func TestEffort255_ExplicitPickHeldAcrossRebuild(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.NewSession()
	pickEffort255(t, sw, 2) // "max" on GLM (configured "high")

	sw.rebuildEffortOptions()

	if got := sw.effortSelect.Value(); got != "max" {
		t.Errorf("explicit pick not held across rebuild: value = %q, want %q", got, "max")
	}
}

// --- Persistence round-trip ---------------------------------------------

// TestEffort255_LayoutCapturesSeededValue: the config-seeded value flows into
// captureLayout, so a freshly-created GLM session persists effort "high" (not
// ""). This is the "pins the value in effect at session-create time" property.
func TestEffort255_LayoutCapturesSeededValue(t *testing.T) {
	w := newEffort255Workbench(t)
	var saved gogent.Layout
	w.handlers.SaveLayout = func(l gogent.Layout) { saved = l }
	sw := w.NewSession()

	if got := sw.selectedEffort(); got != "high" {
		t.Fatalf("seeded effort = %q, want high before persist", got)
	}
	w.persistLayout()

	e := saved.Entry(sw.id)
	if e == nil {
		t.Fatalf("layout has no entry for %s", sw.id)
	}
	if e.Effort != "high" {
		t.Errorf("persisted effort = %q, want %q", e.Effort, "high")
	}
}

// TestEffort255_RestoredExplicitPickBeatsConfigSeed: a persisted explicit pick
// ("max") overrides the config seed ("high") on restore — applyEffort runs after
// the construction-time seed and wins.
func TestEffort255_RestoredExplicitPickBeatsConfigSeed(t *testing.T) {
	w := newEffort255Workbench(t)
	var saved gogent.Layout
	w.handlers.SaveLayout = func(l gogent.Layout) { saved = l }
	sw := w.NewSession()

	pickEffort255(t, sw, 2) // "max" on GLM
	w.persistLayout()
	e := saved.Entry(sw.id)
	if e == nil || e.Effort != "max" {
		t.Fatalf("persisted effort = %q, want max", effortOrEmpty(e))
	}

	// A fresh window on the same model seeds "high" from config...
	sw2 := w.NewSession()
	if got := sw2.selectedEffort(); got != "high" {
		t.Fatalf("fresh window seeded = %q, want high (config) before restore", got)
	}
	// ...then restore: the explicit "max" must win.
	sw2.applyEffort(e.Effort)
	if got := sw2.selectedEffort(); got != "max" {
		t.Errorf("restored effort = %q, want %q (explicit pick beats config seed)", got, "max")
	}
}

// TestEffort255_RestoredEmptyEffortReseedsFromConfig: a session persisted on the
// "(default)" sentinel (effort "") re-seeds the model's configured value on
// restore, because applyEffort("") is a no-op and the construction-time seed
// stands. This documents the issue's intended tradeoff (a session pins the value
// in effect at create/restore time rather than tracking later config edits).
func TestEffort255_RestoredEmptyEffortReseedsFromConfig(t *testing.T) {
	w := newEffort255Workbench(t)
	var saved gogent.Layout
	w.handlers.SaveLayout = func(l gogent.Layout) { saved = l }
	sw := w.NewSession()

	pickEffort255(t, sw, 0) // "(default)" opt-in
	if got := sw.selectedEffort(); got != "" {
		t.Fatalf("precondition: selectedEffort = %q, want \"\" for (default)", got)
	}
	w.persistLayout()
	e := saved.Entry(sw.id)
	if e == nil {
		t.Fatalf("layout has no entry for %s", sw.id)
	}
	if e.Effort != "" {
		t.Fatalf("persisted effort = %q, want \"\" for (default)", e.Effort)
	}

	sw2 := w.NewSession()     // seeds "high" from config
	sw2.applyEffort(e.Effort) // "" -> no-op
	if got := sw2.selectedEffort(); got != "high" {
		t.Errorf("restored (default) session effort = %q, want %q (re-seeded from config)", got, "high")
	}
}

// --- Degenerate inputs / guards ----------------------------------------

// TestEffort255_EmptyModelsNoPanic: with no models configured the selector must
// not panic, stays disabled on "(default)", and reports no override.
func TestEffort255_EmptyModelsNoPanic(t *testing.T) {
	w := NewWorkbench(nil)
	sw := w.NewSession() // must not panic

	if sw.effortEnabled {
		t.Error("selector should be disabled when there are no models")
	}
	if got := sw.effortSelect.Value(); got != "(default)" {
		t.Errorf("value = %q, want %q", got, "(default)")
	}
	if got := sw.selectedEffort(); got != "" {
		t.Errorf("selectedEffort() = %q, want \"\"", got)
	}
	// Re-running must remain safe.
	sw.rebuildEffortOptions()
}

// TestEffort255_AnalysisWindowNilGuard: a read-only analysis window has no
// effort selector (effortSelect == nil). rebuildEffortOptions must short-circuit
// and selectedEffort must report "" rather than dereferencing nil.
func TestEffort255_AnalysisWindowNilGuard(t *testing.T) {
	w := newEffort255Workbench(t)
	sw := w.OpenAnalysisSession(RestoredSession{ID: "analysis-1", Title: "A"})

	if sw.effortSelect != nil {
		t.Fatalf("analysis window effortSelect = %v, want nil", sw.effortSelect)
	}
	// Neither call may panic on a nil selector.
	sw.rebuildEffortOptions()
	if got := sw.selectedEffort(); got != "" {
		t.Errorf("selectedEffort() = %q, want \"\" for a window with no selector", got)
	}
}

// --- Reproducing the exact issue scenario -------------------------------

// TestEffort255_GLMRealWorldScenario is the end-to-end reproduction of the
// filed report: GLM-5.2 is configured reasoning_effort "high" with effort_options
// ["high","max"], and a fresh session must show "high", not "(default)".
func TestEffort255_GLMRealWorldScenario(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{
		{Name: "glm", DisplayName: "GLM-5.2", Model: "glm-5.2", ReasoningEffort: "high", Caps: config.ModelCapabilities{EffortOptions: []string{"high", "max"}}},
	})
	sw := w.NewSession()

	if got, want := sw.effortSelect.Value(), "high"; got != want {
		t.Errorf("fresh GLM-5.2 session effort = %q, want %q (issue #255 regression)", got, want)
	}
	if got, want := sw.selectedEffort(), "high"; got != want {
		t.Errorf("fresh GLM-5.2 session selectedEffort() = %q, want %q", got, want)
	}
}

// effortOrEmpty returns e.Effort or "" when e is nil (test helper for messages).
func effortOrEmpty(e *gogent.LayoutEntry) string {
	if e == nil {
		return ""
	}
	return e.Effort
}
