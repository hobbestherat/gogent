package ui

import (
	"testing"

	"gogent/internal/config"
	"gogent/internal/modelsdev"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Issue #542: the "Add from Catalog…" review form must surface every models.dev
// aspect with consistent "(from catalog)" provenance while keeping routing,
// persistence and validation semantics unchanged. These tests drive the REAL
// showCatalogReviewStep (opening it directly with a hand-built catalog) and assert
// on the rendered grid, the persisted draft (via a capturing AddModel stub), and
// the effort-Select interaction — so a wiring, layout, or persistence regression is
// caught at the seam. ui/tui stays free of internal/daemon|server imports (Handlers
// stubs only), matching the rest of the catalog suite.

// openCatalogReview builds a one-provider catalog, opens its review step directly,
// and returns the workbench. Driving showCatalogReviewStep (not the picker steps)
// keeps the assertions about the REVIEW form itself, not the wizard navigation.
func openCatalogReview(t *testing.T, providerID string, p modelsdev.Provider, modelID string) *Workbench {
	t.Helper()
	if p.Models == nil {
		p.Models = map[string]modelsdev.Model{}
	}
	w := newTestWorkbench(t)
	w.showCatalogReviewStep(modelsdev.Catalog{providerID: p}, providerID, modelID, nil)
	return w
}

// typeCatalogField types value into the editable TextBox found immediately to the
// right of a labelled row (e.g. "API key:"). The review fields start empty, so no
// select-all is needed (and Ctrl+A on an empty box would eat the first rune).
func typeCatalogField(t *testing.T, w *Workbench, label, value string) {
	t.Helper()
	grid := editorGrid(w)
	row, col, ok := findRunes(grid, label)
	if !ok {
		t.Fatalf("label %q not rendered in the review form", label)
	}
	labelEnd := col + len([]rune(label))
	var best *tv.VisualComponent
	bestDX := 1 << 30
	for _, c := range dialogDescendants(w) {
		if c.CopyFn == nil { // editable TextBox only, not a read-only label
			continue
		}
		abs := c.AbsoluteBounds()
		if abs.Y != row || abs.X <= labelEnd {
			continue
		}
		if dx := abs.X - labelEnd; dx < bestDX {
			bestDX = dx
			best = c
		}
	}
	if best == nil {
		t.Fatalf("no editable field found right of %q on row %d", label, row)
	}
	for _, r := range value {
		best.BubbleType(tui.TypeEvent{Key: tui.KeyRune, Rune: r})
	}
	w.desktop.Redraw()
}

// catalogControlOnLabel returns the Focusable control (a Select) rendered on the
// given label's row, so a test can drive its popup. Used for the effort Select.
func catalogControlOnLabel(t *testing.T, w *Workbench, label string) *tv.VisualComponent {
	t.Helper()
	grid := editorGrid(w)
	row, _, ok := findRunes(grid, label)
	if !ok {
		t.Fatalf("label %q not rendered", label)
	}
	for _, c := range dialogDescendants(w) {
		if c.Focusable && c.AbsoluteBounds().Y == row {
			return c
		}
	}
	return nil
}

// selectCatalogEffort opens the reasoning-effort Select and commits option (a real
// popup open + option click), exactly how a user picks an effort value.
func selectCatalogEffort(t *testing.T, w *Workbench, option string) {
	t.Helper()
	sel := catalogControlOnLabel(t, w, "Reasoning:")
	if sel == nil {
		t.Fatalf("reasoning-effort control not found on the Reasoning row")
	}
	sel.OnTypeFn(sel, tui.TypeEvent{Key: tui.KeyEnter}) // open the popup
	w.desktop.Redraw()
	row, col, ok := findRunes(editorGrid(w), option)
	if !ok {
		t.Fatalf("effort option %q not rendered in the open dropdown popup", option)
	}
	top := w.desktop.TopLayer()
	if top == nil {
		t.Fatalf("dropdown popup did not open")
	}
	top.Root.OnClickFn(top.Root, tui.ClickEvent{X: col, Y: row, Down: true})
	top.Root.OnClickFn(top.Root, tui.ClickEvent{X: col, Y: row, Down: false})
	w.desktop.Redraw()
}

// addModelCapturer returns GetModels/AddConnection/AddModel handlers where AddModel
// stashes the saved model draft in *captured (reporting whether it fired via
// *fired) and AddConnection stashes the saved connection draft in *capturedConn.
// Credential/endpoint fields now live on the connection, so tests assert those on
// *capturedConn and Connection/Model/Caps on *captured.
func addModelCapturer(captured **config.ModelConfig, capturedConn **config.ProviderConnection, fired *bool) Handlers {
	return Handlers{
		GetModels: func() []config.ModelConfig { return nil },
		AddConnection: func(pc config.ProviderConnection) error {
			*capturedConn = &pc
			return nil
		},
		AddModel: func(m config.ModelConfig) error {
			*captured = &m
			*fired = true
			return nil
		},
	}
}

// TestCatalogReviewAnthropicSurfacesAllAspects is the headline goal-match check
// (issue #542 acceptance): an Anthropic catalog model's review form surfaces every
// aspect A–J. Anthropic is derive-base (derived indicator), a key provider (env
// hint), and (per the catalog toggle + provider cap) thinking is a no-op on the
// direct Messages API.
func TestCatalogReviewAnthropicSurfacesAllAspects(t *testing.T) {
	p := modelsdev.Provider{
		ID:   "anthropic",
		Name: "Anthropic",
		Env:  []string{"ANTHROPIC_API_KEY"},
		API:  "https://api.anthropic.com/v1",
		Doc:  "https://docs.anthropic.com",
		Models: map[string]modelsdev.Model{"claude-opus-4-6": {
			ID:          "claude-opus-4-6",
			Name:        "Claude Opus 4.6",
			Reasoning:   true,
			ToolCall:    true,
			Attachment:  true,
			Temperature: true,
			Limit:       modelsdev.Limit{Context: 200000, Output: 64000},
			Cost:        modelsdev.Cost{Input: 5, Output: 25},
			ReasoningOptions: []modelsdev.ReasoningOption{
				{Type: "effort", Values: []string{"low", "medium", "high"}},
			},
		}},
	}
	w := openCatalogReview(t, "anthropic", p, "claude-opus-4-6")

	// A — derive-base endpoint indicator (NOT a prefilled box).
	if !modelsDialogHasText(t, w, "derived: https://api.anthropic.com") {
		t.Error("aspect A: derive-base Endpoint indicator missing")
	}
	if modelsDialogHasText(t, w, "https://api.anthropic.com/v1") {
		t.Error("aspect A: anthropic Endpoint box was prefilled with p.API (must stay empty so the persisted endpoint stays blank)")
	}
	// B — context window.
	if !modelsDialogHasText(t, w, "Context 200K") {
		t.Error("aspect B: Context window indicator missing")
	}
	// C — max-tokens output-cap provenance.
	if !modelsDialogHasText(t, w, "from catalog output limit") {
		t.Error("aspect C: Max-tokens output-limit hint missing")
	}
	// D — pricing.
	if !modelsDialogHasText(t, w, "Cost $5 in / $25 out per M") {
		t.Error("aspect D: Cost indicator missing")
	}
	// E — effort Select is in play: the "(from catalog)" provenance hint is on the
	// Reasoning row (the full option set is asserted in TestCatalogReviewEffortSelect).
	if !modelsDialogHasText(t, w, "Reasoning:") {
		t.Error("aspect E: Reasoning row missing")
	}
	// F + I — reasoning-capable and capability flags (tool / vision / temperature).
	if !modelsDialogHasText(t, w, "Capabilities:") {
		t.Error("aspect I: Capabilities row missing")
	}
	for _, cap := range []string{"reasoning", "tool calling", "vision", "custom temperature"} {
		if !modelsDialogHasText(t, w, cap) {
			t.Errorf("aspect F/I: capability %q missing from the Capabilities row", cap)
		}
	}
	// G — thinking-toggle relevance: direct Anthropic drops thinking, so no-op.
	if !modelsDialogHasText(t, w, "(no effect for this model)") {
		t.Error("aspect G: Thinking no-op annotation missing for direct Anthropic (SupportsThinking is false)")
	}
	if modelsDialogHasText(t, w, "(supported)") {
		t.Error("aspect G: Thinking annotated (supported) for direct Anthropic, but gogent drops the param")
	}
	// H — provider credential env var carried forward.
	if !modelsDialogHasText(t, w, "(env: ANTHROPIC_API_KEY)") {
		t.Error("aspect H: provider env-var hint missing next to the API key")
	}
	// J — docs URL.
	if !modelsDialogHasText(t, w, "Docs: https://docs.anthropic.com") {
		t.Error("aspect J: Docs row missing")
	}
}

// TestCatalogReviewVertexDerivedFromProjectLocation: vertex* build the base from
// project/location (unknown until entered), so the indicator says so, and the form
// shows Project/Location (ADC) instead of an API-key field with an env hint.
func TestCatalogReviewVertexDerivedFromProjectLocation(t *testing.T) {
	p := modelsdev.Provider{
		ID: "google-vertex", Name: "Google Vertex AI", API: "ignored",
		Models: map[string]modelsdev.Model{"gemini-2.5-flash": {ID: "gemini-2.5-flash", Name: "Gemini 2.5 Flash"}},
	}
	w := openCatalogReview(t, "google-vertex", p, "gemini-2.5-flash")

	if !modelsDialogHasText(t, w, "derived from Project + Location") {
		t.Error("aspect A: vertex Endpoint indicator should read 'derived from Project + Location'")
	}
	if !modelsDialogHasText(t, w, "Project:") || !modelsDialogHasText(t, w, "Location:") {
		t.Error("vertex review should show Project/Location fields (ADC, no API key)")
	}
	if modelsDialogHasText(t, w, "(env:") {
		t.Error("vertex review should not show an API-key env hint (it authenticates with ADC)")
	}
}

// TestCatalogReviewGatewayPrefillsAPI: OpenAI-compatible gateways (Groq/Together/
// DeepSeek/…) keep the editable Endpoint box prefilled with p.API and show NO
// derived hint — the no-regression guarantee for the gateway path.
func TestCatalogReviewGatewayPrefillsAPI(t *testing.T) {
	p := modelsdev.Provider{
		ID: "groq", Name: "Groq", Env: []string{"GROQ_API_KEY"}, API: "https://api.groq.com/openai/v1",
		Models: map[string]modelsdev.Model{"llama-3.3-70b": {ID: "llama-3.3-70b", Name: "Llama 3.3 70B"}},
	}
	w := openCatalogReview(t, "groq", p, "llama-3.3-70b")

	if !modelsDialogHasText(t, w, "https://api.groq.com/openai/v1") {
		t.Error("gateway Endpoint box should be prefilled with p.API (unchanged path)")
	}
	if modelsDialogHasText(t, w, "derived:") {
		t.Error("gateway Endpoint should NOT show a 'derived:' hint (only derive-base providers do)")
	}
}

// TestCatalogReviewZAIThinkingSupported: a provider that emits `thinking` (zai)
// AND a model that advertises a toggle is annotated "(supported)".
func TestCatalogReviewZAIThinkingSupported(t *testing.T) {
	p := modelsdev.Provider{
		ID: "zai", Name: "Z.AI", Env: []string{"ZAI_API_KEY"}, API: "https://api.z.ai/api/paas/v4",
		Models: map[string]modelsdev.Model{"glm-4.6": {
			ID: "glm-4.6", Name: "GLM-4.6",
			ReasoningOptions: []modelsdev.ReasoningOption{
				{Type: "effort", Values: []string{"high", "max"}},
				{Type: "toggle"},
			},
		}},
	}
	w := openCatalogReview(t, "zai", p, "glm-4.6")

	if !modelsDialogHasText(t, w, "derived: https://api.z.ai/api/paas/v4") {
		t.Error("zai is derive-base: its resolved base should be shown")
	}
	if !modelsDialogHasText(t, w, "(supported)") {
		t.Error("aspect G: zai + a catalog toggle should annotate Thinking '(supported)'")
	}
}

// TestCatalogReviewBareModelOmitsOptionals: a model with no reasoning, no caps, and
// a free price omits the Capabilities row and degrades the effort control to the
// free-text fallback — "absent/blank where the model lacks them".
func TestCatalogReviewBareModelOmitsOptionals(t *testing.T) {
	p := modelsdev.Provider{
		ID: "groq", Name: "Groq", Env: []string{"GROQ_API_KEY"}, API: "https://api.groq.com/openai/v1",
		Models: map[string]modelsdev.Model{"gpt-3.5-turbo": {ID: "gpt-3.5-turbo", Name: "GPT 3.5", Limit: modelsdev.Limit{Context: 4096, Output: 4096}}},
	}
	w := openCatalogReview(t, "groq", p, "gpt-3.5-turbo")

	if !modelsDialogHasText(t, w, "Cost Free") {
		t.Error("a zero-cost model should show 'Cost Free (from catalog)'")
	}
	if modelsDialogHasText(t, w, "Capabilities:") {
		t.Error("a model with no capability flags should omit the Capabilities row")
	}
	if modelsDialogHasText(t, w, "Docs:") {
		t.Error("a provider with no Doc should omit the Docs row")
	}
	// No effort options => the Reasoning row is the free-text fallback, so the effort
	// Select's "(none)" sentinel must NOT be present anywhere.
	if sel := catalogControlOnLabel(t, w, "Reasoning:"); sel != nil {
		// A Select on the Reasoning row would be the constrained control; for a bare
		// model there should be no effort Select at all (the control is a TextBox).
		// Distinguish by attempting to open a popup: a TextBox has no popup, so the
		// "(none)" sentinel won't render.
		sel.OnTypeFn(sel, tui.TypeEvent{Key: tui.KeyEnter})
		w.desktop.Redraw()
		if _, _, ok := findRunes(editorGrid(w), effortNoneSentinel()); ok {
			t.Error("bare model (no effort options) should fall back to the free-text Reasoning box, not the constrained Select")
		}
	}
}

// effortNoneSentinel returns the "(none)" string the effort Select uses, via the
// production const, so this test tracks the real sentinel.
func effortNoneSentinel() string { return effortNone }

// TestCatalogReviewEffortSelectOptions: when a model exposes effort options the
// Reasoning control is a Select constrained to ["(none)"] + EffortOptions, opened
// on the catalog default. Opening the popup must render every option.
func TestCatalogReviewEffortSelectOptions(t *testing.T) {
	p := modelsdev.Provider{
		ID: "anthropic", Name: "Anthropic", Env: []string{"ANTHROPIC_API_KEY"}, API: "https://api.anthropic.com/v1",
		Models: map[string]modelsdev.Model{"c": {
			ID: "c", Name: "C",
			ReasoningOptions: []modelsdev.ReasoningOption{{Type: "effort", Values: []string{"low", "medium", "high"}}},
		}},
	}
	w := openCatalogReview(t, "anthropic", p, "c")

	sel := catalogControlOnLabel(t, w, "Reasoning:")
	if sel == nil {
		t.Fatal("reasoning-effort Select not found on the Reasoning row")
	}
	sel.OnTypeFn(sel, tui.TypeEvent{Key: tui.KeyEnter}) // open popup
	w.desktop.Redraw()
	for _, opt := range []string{effortNone, "low", "medium", "high"} {
		if _, _, ok := findRunes(editorGrid(w), opt); !ok {
			t.Errorf("effort Select option %q not rendered in the open popup", opt)
		}
	}
}

// TestCatalogReviewSavePersistsEmptyEndpoint is the no-regression crux: saving a
// derive-base (anthropic) model with the empty Endpoint box persists Endpoint=""
// (the #541 invariant), while the catalog effort default is retained.
func TestCatalogReviewSavePersistsEmptyEndpoint(t *testing.T) {
	var captured *config.ModelConfig
	var capturedConn *config.ProviderConnection
	fired := false
	w := newTestWorkbench(t)
	w.SetHandlers(addModelCapturer(&captured, &capturedConn, &fired))
	p := modelsdev.Provider{
		ID: "anthropic", Name: "Anthropic", Env: []string{"ANTHROPIC_API_KEY"}, API: "https://api.anthropic.com/v1",
		Models: map[string]modelsdev.Model{"c": {
			ID: "c", Name: "C",
			ReasoningOptions: []modelsdev.ReasoningOption{{Type: "effort", Values: []string{"low", "medium", "high"}}},
		}},
	}
	w.showCatalogReviewStep(modelsdev.Catalog{"anthropic": p}, "anthropic", "c", nil)

	typeCatalogField(t, w, "API key:", "test-key")
	clickTopButtonByText(t, w, "Save")

	if !fired || captured == nil {
		t.Fatal("Save did not call AddModel (API key not accepted or Save failed)")
	}
	if capturedConn == nil {
		t.Fatal("Save did not call AddConnection (the credential/endpoint now live on the connection)")
	}
	if capturedConn.Endpoint != "" {
		t.Errorf("persisted derive-base Endpoint = %q, want empty (the #541 invariant: empty endpoint unless the user overrides)", capturedConn.Endpoint)
	}
	if captured.ReasoningEffort != "low" {
		t.Errorf("persisted ReasoningEffort = %q, want the catalog default %q (effort Select preselects EffortOptions[0])", captured.ReasoningEffort, "low")
	}
	if capturedConn.APIKey == "" {
		t.Error("persisted APIKey is empty (the typed key was not read on Save)")
	}
}

// TestCatalogReviewEffortNoneOptOut: picking the leading "(none)" effort option
// clears ReasoningEffort (opts the model out of reasoning), preserving the opt-out
// the pre-#542 free-text box allowed.
func TestCatalogReviewEffortNoneOptOut(t *testing.T) {
	var captured *config.ModelConfig
	var capturedConn *config.ProviderConnection
	fired := false
	w := newTestWorkbench(t)
	w.SetHandlers(addModelCapturer(&captured, &capturedConn, &fired))
	p := modelsdev.Provider{
		ID: "anthropic", Name: "Anthropic", Env: []string{"ANTHROPIC_API_KEY"}, API: "https://api.anthropic.com/v1",
		Models: map[string]modelsdev.Model{"c": {
			ID: "c", Name: "C",
			ReasoningOptions: []modelsdev.ReasoningOption{{Type: "effort", Values: []string{"low", "medium", "high"}}},
		}},
	}
	w.showCatalogReviewStep(modelsdev.Catalog{"anthropic": p}, "anthropic", "c", nil)

	selectCatalogEffort(t, w, effortNone) // opt out of reasoning
	typeCatalogField(t, w, "API key:", "test-key")
	clickTopButtonByText(t, w, "Save")

	if !fired || captured == nil {
		t.Fatal("Save did not call AddModel")
	}
	if captured.ReasoningEffort != "" {
		t.Errorf("persisted ReasoningEffort = %q after picking (none), want empty (the opt-out must clear it)", captured.ReasoningEffort)
	}
}

// TestCatalogReviewSaveGatewayPersistsAPI: a gateway's prefilled p.API endpoint
// survives Save (the gateway path is unchanged end-to-end through persistence).
func TestCatalogReviewSaveGatewayPersistsAPI(t *testing.T) {
	var captured *config.ModelConfig
	var capturedConn *config.ProviderConnection
	fired := false
	w := newTestWorkbench(t)
	w.SetHandlers(addModelCapturer(&captured, &capturedConn, &fired))
	p := modelsdev.Provider{
		ID: "groq", Name: "Groq", Env: []string{"GROQ_API_KEY"}, API: "https://api.groq.com/openai/v1",
		Models: map[string]modelsdev.Model{"llama": {ID: "llama", Name: "Llama"}},
	}
	w.showCatalogReviewStep(modelsdev.Catalog{"groq": p}, "groq", "llama", nil)

	typeCatalogField(t, w, "API key:", "test-key")
	clickTopButtonByText(t, w, "Save")

	if !fired || captured == nil {
		t.Fatal("Save did not call AddModel")
	}
	if capturedConn == nil {
		t.Fatal("Save did not call AddConnection")
	}
	if capturedConn.Endpoint != "https://api.groq.com/openai/v1" {
		t.Errorf("persisted gateway Endpoint = %q, want p.API", capturedConn.Endpoint)
	}
}

// TestCatalogReviewVertexSaveRequiresProjectLocation: vertex uses ADC, so Save must
// reject an empty project/location (validation unchanged) rather than demand an API
// key or silently save an unroutable config.
func TestCatalogReviewVertexSaveRequiresProjectLocation(t *testing.T) {
	var captured *config.ModelConfig
	var capturedConn *config.ProviderConnection
	fired := false
	w := newTestWorkbench(t)
	w.SetHandlers(addModelCapturer(&captured, &capturedConn, &fired))
	p := modelsdev.Provider{
		ID: "google-vertex", Name: "Vertex", API: "ignored",
		Models: map[string]modelsdev.Model{"gemini": {ID: "gemini", Name: "Gemini"}},
	}
	w.showCatalogReviewStep(modelsdev.Catalog{"google-vertex": p}, "google-vertex", "gemini", nil)

	clickTopButtonByText(t, w, "Save")

	if fired {
		t.Error("Save must not call AddModel when vertex project/location are empty")
	}
	if !modelsDialogHasText(t, w, "Vertex models need a GCP project and location.") {
		t.Error("Save should surface the vertex project/location validation message")
	}
}

// TestCatalogReviewLayoutFits asserts the re-laid-out review form fits on the
// minimum terminal (80×24) and the test default (80×25): the MinH/MinW floors
// resolve, the Save button (bottom row) renders, and the deepest content rows
// (Docs) are not clipped. This guards the height-bump 18→21 / width 70→76.
func TestCatalogReviewLayoutFits(t *testing.T) {
	p := modelsdev.Provider{
		ID: "anthropic", Name: "Anthropic", Env: []string{"ANTHROPIC_API_KEY"}, API: "https://api.anthropic.com/v1", Doc: "https://docs.anthropic.com",
		Models: map[string]modelsdev.Model{"claude-opus-4-6": {
			ID: "claude-opus-4-6", Name: "Claude Opus 4.6", Reasoning: true, ToolCall: true, Attachment: true, Temperature: true,
			Limit: modelsdev.Limit{Context: 200000, Output: 64000}, Cost: modelsdev.Cost{Input: 5, Output: 25},
			ReasoningOptions: []modelsdev.ReasoningOption{{Type: "effort", Values: []string{"low", "medium", "high"}}},
		}},
	}
	for _, dim := range []struct{ w, h int }{{80, 25}, {80, 24}} {
		t.Run("", func(t *testing.T) {
			w := newTestWorkbench(t)
			w.app.Resize(dim.w, dim.h)
			w.showCatalogReviewStep(modelsdev.Catalog{"anthropic": p}, "anthropic", "claude-opus-4-6", nil)
			b := dialogBounds(w)
			if b.W < 76 || b.H < 21 {
				t.Errorf("review dialog = %dx%d on %dx%d, want at least 76x21 (MinW/MinH floors)", b.W, b.H, dim.w, dim.h)
			}
			// The Save button sits on the bottom button row; it must render (not clipped).
			if !modelsDialogHasText(t, w, "Save") {
				t.Errorf("Save button not visible at %dx%d (bottom row clipped)", dim.w, dim.h)
			}
			// The Docs row is the deepest content row for this model; it must render.
			if !modelsDialogHasText(t, w, "Docs: https://docs.anthropic.com") {
				t.Errorf("Docs row not visible at %dx%d (deepest content row clipped)", dim.w, dim.h)
			}
		})
	}
}
