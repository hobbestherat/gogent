package ui

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/config"
)

// This file tests issue #533: the Overall band's model-selector aggregate option
// is capitalised "All models" for consistency with the capitalised "Overall"
// title. The change is a single constant value (overallAllModelsOption), so the
// interesting test surface is the INVARIANT the capitalisation must not disturb —
// the selector's identity is the config NAME ("" for the aggregate), never the
// display label — plus a literal pin on the exact capitalised form.
//
// Why a dedicated literal pin matters: the pre-existing selector tests build their
// expected option list FROM overallAllModelsOption (overall_stats_selector_test.go
// wantOpts), so they are tautological w.r.t. the label value — they pass whether
// it reads "all models", "All models" or "ALL MODELS". Those tests therefore cannot
// detect a wrong capitalisation. The tests below compare against the literal
// "All models" so a regression is actually caught.

// ----------------------------------------------------------------------------
// Helpers.
// ----------------------------------------------------------------------------

// issue533FirstOrEmpty returns s[0], or "" when s is empty, so an error format can
// reference an option slice without re-checking its length inline.
func issue533FirstOrEmpty(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// issue533Segment reads n cells starting at (x0, y) from the rendered app buffer
// and returns them as a string, rendering NUL/0 cells as spaces. It is the
// headless cell-scan primitive for the rendered-label assertion (the same approach
// the issue #233 separator tests use).
func issue533Segment(w *Workbench, y, x0, n int) string {
	var b strings.Builder
	for dx := 0; dx < n; dx++ {
		ch := w.app.ReadCell(x0+dx, y).Ch
		if ch == 0 {
			ch = ' '
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// ----------------------------------------------------------------------------
// Criterion 1 — Goal match: the aggregate option's VISIBLE label is "All models".
// ----------------------------------------------------------------------------

// TestIssue533_AggregateOptionLiteralIsCapitalised is the core goal pin: the
// constant must hold the exact literal "All models". Compared against the literal
// — not the constant itself — so a wrong capitalisation (or no capitalisation) is
// caught. The existing constant-reference tests cannot do this.
func TestIssue533_AggregateOptionLiteralIsCapitalised(t *testing.T) {
	if overallAllModelsOption != "All models" {
		t.Errorf("overallAllModelsOption = %q, want the exact literal %q",
			overallAllModelsOption, "All models")
	}
	// First rune must be the capital A; the old lowercase lead must be gone.
	runes := []rune(overallAllModelsOption)
	switch {
	case len(runes) == 0:
		t.Errorf("overallAllModelsOption is empty")
	case runes[0] != 'A':
		t.Errorf("overallAllModelsOption first rune = %q, want capital 'A'", string(runes[0]))
	}
	// Guard the regression this issue exists to fix: the lowercase form is gone.
	if overallAllModelsOption == "all models" {
		t.Errorf("overallAllModelsOption is still the lowercase %q — #533 not applied",
			overallAllModelsOption)
	}
}

// TestIssue533_SelectorConstructedWithCapitalisedAggregate pins that the selector
// constructor seeds the dropdown with the capitalised aggregate as its first
// option. The constructor reads the constant (sidebar.go newSelect), so this proves
// the constant flows into the live widget — checked against the literal, not the
// constant, so it is a real regression net.
func TestIssue533_SelectorConstructedWithCapitalisedAggregate(t *testing.T) {
	w := newTestWorkbench(t)
	if w.sidebar.overallSelect == nil {
		t.Fatal("precondition: test workbench has no model selector")
	}
	opts := w.sidebar.overallSelect.Options
	if issue533FirstOrEmpty(opts) != "All models" {
		t.Errorf("selector Options[0] = %q, want literal %q",
			issue533FirstOrEmpty(opts), "All models")
	}
}

// TestIssue533_RebuildOptionsLeadsWithCapitalisedAggregate pins that rebuilding
// the option list (after models are set) still leads with the capitalised
// aggregate label, and — the invariant — that the same index 0 carries the empty
// config-name key.
func TestIssue533_RebuildOptionsLeadsWithCapitalisedAggregate(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{{Name: "glm", DisplayName: "GLM"}})
	s := w.sidebar
	if got := issue533FirstOrEmpty(s.overallSelect.Options); got != "All models" {
		t.Errorf("rebuild Options[0] = %q, want literal %q", got, "All models")
	}
	if len(s.overallModelKeys) == 0 || s.overallModelKeys[0] != "" {
		t.Errorf("rebuild overallModelKeys[0] = %q, want empty config name (aggregate key)",
			issue533FirstOrEmpty(s.overallModelKeys))
	}
}

// ----------------------------------------------------------------------------
// Criterion 2 — Usability: consistent capitalisation, no behaviour change.
// ----------------------------------------------------------------------------

// TestIssue533_AggregateRemainsDefaultSelection pins that capitalising the label
// did not change the default selection: the aggregate is still index 0 and still
// resolves to the aggregate config key.
func TestIssue533_AggregateRemainsDefaultSelection(t *testing.T) {
	w := newTestWorkbench(t)
	sel := w.sidebar.overallSelect
	if got := sel.GetSelected(); got != 0 {
		t.Errorf("initial GetSelected = %d, want 0 (aggregate must stay the default)", got)
	}
	if got := w.sidebar.selectedOverallModel(); got != "" {
		t.Errorf("default selectedOverallModel = %q, want empty (aggregate key)", got)
	}
}

// TestIssue533_LabelWidthUnchangedByCapitalisation pins that the new label is the
// same length as the old one, so the selector's rendered width, ellipsisation and
// band layout are byte-identical. A future "fix" that lengthened the label would
// shift the band and break this.
func TestIssue533_LabelWidthUnchangedByCapitalisation(t *testing.T) {
	const oldForm = "all models"
	if len(overallAllModelsOption) != len(oldForm) {
		t.Errorf("len(overallAllModelsOption) = %d, want %d (same width as the old form — no layout shift)",
			len(overallAllModelsOption), len(oldForm))
	}
}

// ----------------------------------------------------------------------------
// Criterion 3 — No regressions: layout keyed on config NAME, never the label.
// ----------------------------------------------------------------------------

// TestIssue533_AggregateLabelCapitalisedButConfigKeyEmpty is the heart of the
// invariant: the aggregate option's two faces — the visible label and the identity
// key — are independent. The label is capitalised "All models" but the key the
// panel scopes and persists on is still the empty config name. #533 must not let
// the capitalisation leak into the key.
func TestIssue533_AggregateLabelCapitalisedButConfigKeyEmpty(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{{Name: "glm", DisplayName: "GLM"}})
	s := w.sidebar
	s.overallSelect.SetSelected(0) // force the aggregate

	// Visible label is capitalised ...
	if got := s.overallSelect.Value(); got != "All models" {
		t.Errorf("aggregate label (Value) = %q, want capitalised %q", got, "All models")
	}
	// ... but the identity key is still empty.
	if got := s.selectedOverallModel(); got != "" {
		t.Errorf("aggregate config key = %q, want empty (label capitalised, key unchanged)", got)
	}
}

// TestIssue533_LayoutDefaultKeysAggregateAsEmpty pins the default persisted layout:
// the aggregate maps to config name "", never the display label, so capitalising
// the label cannot change what a fresh run persists.
func TestIssue533_LayoutDefaultKeysAggregateAsEmpty(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{{Name: "glm"}})
	if got := w.captureLayout().OverallModel; got != "" {
		t.Errorf("default Layout.OverallModel = %q, want empty (aggregate config name, not the label %q)",
			got, overallAllModelsOption)
	}
}

// TestIssue533_LayoutRoundTripKeysOnConfigNameNotLabel pins the restart round-trip:
// a model selection is captured as its config NAME, restored as the same config
// name, and the aggregate option survives the round trip still capitalised. The
// label is never serialised.
func TestIssue533_LayoutRoundTripKeysOnConfigNameNotLabel(t *testing.T) {
	models := []*config.ModelConfig{
		{Name: "groq-free", DisplayName: "Groq"},
		{Name: "glm", DisplayName: "GLM"},
	}
	w := newTestWorkbench(t)
	w.SetModels(models)
	w.sidebar.setSelectedOverallModel("glm")

	layout := w.captureLayout()
	if layout.OverallModel != "glm" {
		t.Fatalf("captured OverallModel = %q, want config name glm", layout.OverallModel)
	}

	// Restart path: apply on a fresh workbench.
	w2 := newTestWorkbench(t)
	w2.SetModels(models)
	w2.applyLayout(layout)
	if got := w2.sidebar.selectedOverallModel(); got != "glm" {
		t.Errorf("restored selection = %q, want glm (config name round-trips; label never used)", got)
	}
	// The aggregate option is still present and still capitalised after restore.
	if got := issue533FirstOrEmpty(w2.sidebar.overallSelect.Options); got != "All models" {
		t.Errorf("post-restore Options[0] = %q, want capitalised %q", got, "All models")
	}
}

// TestIssue533_BuildOverallStatsDetectsAggregateViaEmptyKeyNotLabel proves the
// scoping branch keys on selectedModel == "" (the config key), NOT on a comparison
// against the display label. If the code naively compared against the label, the
// capitalisation would have broken it; this test enforces that it does not.
// Passing the literal label string "All models" as the selection must NOT be
// treated as the aggregate — it scopes to an unknown model (zeros), never the
// grand total.
func TestIssue533_BuildOverallStatsDetectsAggregateViaEmptyKeyNotLabel(t *testing.T) {
	rep := reportForSelectorTests()

	// The empty key selects the aggregate grand total.
	agg := buildOverallStats(rep, 4, 6, nil, "")
	if agg.Requests != 100 || agg.TokensIn != 1000 {
		t.Errorf("aggregate (key %q) = %+v, want grand total {Req:100 In:1000}", "", agg)
	}
	// Passing the DISPLAY LABEL as the selection must not yield the grand total —
	// the aggregate is detected via the empty key, not the label string.
	byLabel := buildOverallStats(rep, 4, 6, nil, "All models")
	if byLabel.Requests == 100 && byLabel.TokensIn == 1000 {
		t.Errorf("passing the label %q as the selection yielded the grand total — "+
			"aggregate must be detected via the empty key, never the label string", "All models")
	}
	// A real model still scopes correctly (the branch still works for model keys).
	glm := buildOverallStats(rep, 4, 6, nil, "glm")
	if glm.Requests != 70 || glm.TokensIn != 700 {
		t.Errorf("scoped(glm) = %+v, want glm's {Req:70 In:700}", glm)
	}
}

// TestIssue533_LabelCollisionModelNamedAllModelsStaysDistinct is the deepest test
// of the key/label seam. A model whose config Name is "All models" with no
// DisplayName renders identically to the aggregate label, so the dropdown shows
// two "All models" rows. The config-name keying must still tell them apart: the
// aggregate maps to key "" and the model to key "All models". This proves
// capitalising the label cannot corrupt selection, persistence or restore even
// under a pathological label collision.
func TestIssue533_LabelCollisionModelNamedAllModelsStaysDistinct(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{{Name: "All models"}}) // collides with the aggregate label
	s := w.sidebar

	opts := s.overallSelect.Options
	keys := s.overallModelKeys
	if len(opts) != 2 || len(keys) != 2 {
		t.Fatalf("options/keys len = %d/%d, want 2/2 (aggregate + colliding model)", len(opts), len(keys))
	}
	// Two identical labels ...
	if opts[0] != "All models" || opts[1] != "All models" {
		t.Errorf("labels = %v, want both %q (collision)", opts, "All models")
	}
	// ... but distinct keys: aggregate "" at index 0, the model's config name at 1.
	if keys[0] != "" || keys[1] != "All models" {
		t.Errorf("keys = %v, want [\"\", \"All models\"] (distinct identity despite identical labels)", keys)
	}

	// Selecting by the model's config name lands on the model (index 1), NOT the
	// aggregate, even though both render as "All models".
	s.setSelectedOverallModel("All models")
	if got, idx := s.selectedOverallModel(), s.overallSelect.GetSelected(); got != "All models" || idx != 1 {
		t.Errorf("setSelectedOverallModel(%q) -> key=%q idx=%d, want key %q idx 1 (the model, not the aggregate)",
			"All models", got, idx, "All models")
	}
	// Selecting the aggregate by its empty key lands on index 0.
	s.setSelectedOverallModel("")
	if got, idx := s.selectedOverallModel(), s.overallSelect.GetSelected(); got != "" || idx != 0 {
		t.Errorf("setSelectedOverallModel(%q) -> key=%q idx=%d, want key %q idx 0 (aggregate)", "", got, idx, "")
	}

	// Persistence keys on the config name: the colliding model persists as its name.
	s.setSelectedOverallModel("All models")
	if got := w.captureLayout().OverallModel; got != "All models" {
		t.Errorf("model-selected Layout.OverallModel = %q, want config name %q (not the aggregate key)",
			got, "All models")
	}
	s.setSelectedOverallModel("")
	if got := w.captureLayout().OverallModel; got != "" {
		t.Errorf("aggregate-selected Layout.OverallModel = %q, want empty", got)
	}
}

// ----------------------------------------------------------------------------
// Error handling — the constant-backed consumer paths stay nil-safe.
// ----------------------------------------------------------------------------

// TestIssue533_SelectorNilGuardsHold pins that a sidebar whose Overall panel was
// never built (a nil selector) degrades gracefully across every method that
// surfaces the capitalised label. Each of those methods guards
// `s.overallSelect == nil` as its first statement, so a zero-value sidebar — whose
// selector is nil by construction — exercises the guards. #533 changed the label
// they carry, so this pins that the change did not introduce a nil-deref.
func TestIssue533_SelectorNilGuardsHold(t *testing.T) {
	s := &sidebar{}
	if s.overallSelect != nil {
		t.Fatal("precondition: zero-value sidebar must have a nil overallSelect")
	}
	if got := s.selectedOverallModel(); got != "" {
		t.Errorf("selectedOverallModel with nil selector = %q, want empty", got)
	}
	// These must be no-ops, not panics.
	s.setSelectedOverallModel("glm")
	s.setSelectedOverallModel("")
	s.rebuildModelOptions()
}

// ----------------------------------------------------------------------------
// Criterion 1 (rendered) — "All models" actually reaches the screen.
// ----------------------------------------------------------------------------

// TestIssue533_RendersCapitalisedAggregateInline is the only non-tautological,
// rendered assertion of the visible change. The collapsed selector renders its
// current value inline (turbotui Select.draw writes s.Value()), so with the
// aggregate selected (index 0, the default) the selector's row must spell
// "All models" in the cell buffer.
func TestIssue533_RendersCapitalisedAggregateInline(t *testing.T) {
	w := newTestWorkbench(t)
	// Lay the panel out tall enough that the band (and selector) show. panel.SetBounds
	// runs the LayoutFn synchronously (turbotui component.go), sizing and unhiding
	// the selector at panel-relative Y = bandTop + overallSeparatorLines.
	w.sidebar.panel.SetBounds(tv.Rect{X: 48, Y: 1, W: defaultSidebarWidth, H: overallBandHeight + 20})
	if w.sidebar.overallBandH == 0 {
		t.Fatal("precondition: band dropped at this height — selector hidden")
	}
	root := w.sidebar.overallSelect.Root()
	if !root.Visible {
		t.Fatal("precondition: selector hidden though band shown")
	}
	if got := w.sidebar.overallSelect.GetSelected(); got != 0 {
		t.Fatalf("precondition: default selection = %d, want 0 (aggregate) so the inline value is the label", got)
	}

	// Paint a frame and read the selector's row back from the cell buffer.
	w.desktop.Redraw()
	abs := root.AbsoluteBounds()
	if got := issue533Segment(w, abs.Y, abs.X, len("All models")); got != "All models" {
		t.Errorf("rendered selector row = %q, want %q", got, "All models")
	}
}
