package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gogent/internal/config"
	"gogent/internal/modelsdev"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Issue #509: the unified "Models…" dialog (showModelsDialog) is the single home for
// add (Catalog + Empty-slot) / edit / remove / set-default. These exercise the
// user-facing behaviour and the design criteria:
//   (1) goal — opens with ZERO models (no early-return), Empty-slot add works
//       offline (no catalog), the standalone catalog menu/palette entries are gone;
//   (2) usability — rows show marker + display + model id + api type, the right
//       buttons are enabled/disabled per state, Remove confirms, a blocked removal
//       surfaces its reason, and changes refresh the live dropdowns;
//   (3) no regressions — the shared modelForm drives Edit/Add Empty, reusing the
//       old editor's field set;
//   (4) the dialog is pure composition of existing turbotui primitives.
//
// They drive the REAL dialog (clicking its buttons via the rendered label, exactly
// like theme_issue462_test.go) so a wiring or layout regression is caught at the
// seam. ui/tui stays free of internal/daemon|server imports (Handlers stubs only).

// modelsDialogHasText reports whether text appears within the TOP dialog's own
// rectangle (not the whole screen). Localizing to the dialog avoids collisions with
// unrelated chrome such as the app's "Edit" menu bar, which would otherwise make
// absence assertions (e.g. "no Edit affordance in the zero-model state") unreliable.
func modelsDialogHasText(t *testing.T, w *Workbench, text string) bool {
	t.Helper()
	b := dialogBounds(w)
	if b == (tv.Rect{}) {
		return false
	}
	grid := editorGrid(w)
	for y := b.Y; y < b.Y+b.H; y++ {
		if y < 0 || y >= len(grid) {
			continue
		}
		row := grid[y]
		x0, x1 := b.X, b.X+b.W
		if x0 < 0 {
			x0 = 0
		}
		if x1 > len(row) {
			x1 = len(row)
		}
		if x0 < x1 && strings.Contains(string(row[x0:x1]), text) {
			return true
		}
	}
	return false
}

func TestModelsDialogOpensWithZeroModels(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetModels: func() []config.ModelConfig { return nil },
		AddModel:  func(config.ModelConfig) error { return nil },
	})
	w.showModelsDialog()

	if w.desktop.TopLayer() == nil {
		t.Fatal("dialog did not open with zero models (the old showModelEditor early-return regression)")
	}
	// Empty-list state: only Add Empty + Done are actionable.
	if !modelsDialogHasText(t, w, "Empty") {
		t.Error("Add Empty button missing in the zero-model state")
	}
	if !modelsDialogHasText(t, w, "Done") {
		t.Error("Done button missing in the zero-model state")
	}
	if !modelsDialogHasText(t, w, "No models configured") {
		t.Error("empty-list placeholder missing")
	}
	// Edit/Remove/Set Default are absent (nothing to act on); Catalog is absent
	// because GetModelCatalog is not wired (the offline case Empty-slot must still
	// cover).
	for _, absent := range []string{"Edit", "Remove", "Set Default", "Catalog"} {
		if modelsDialogHasText(t, w, absent) {
			t.Errorf("zero-model state should not surface a %q affordance", absent)
		}
	}
}

func TestModelsDialogShowsRowContent(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetModels: func() []config.ModelConfig {
			return []config.ModelConfig{{Name: "gpt4o", DisplayName: "GPT-4o", Model: "gpt-4o", APIType: "openai"}}
		},
		GetDefaultModel: func() string { return "gpt4o" },
		UpdateModel:     func(config.ModelConfig) error { return nil },
		AddModel:        func(config.ModelConfig) error { return nil },
		RemoveModel:     func(string) error { return nil },
		SetDefaultModel: func(string) error { return nil },
	})
	w.showModelsDialog()

	// The row carries display name + model id + api type, and the default marker.
	for _, text := range []string{"GPT-4o", "gpt-4o", "openai"} {
		if !modelsDialogHasText(t, w, text) {
			t.Errorf("row should show %q (display / model id / api type)", text)
		}
	}
	if _, _, ok := findRunes(editorGrid(w), "✓ GPT-4o"); !ok {
		t.Error("default row should be marked with ✓ before the display name")
	}
}

// TestRefreshModelsListClearsLiveDropdownsWhenEmpty is the direct D1 guard: unlike
// refreshModelsAfterSave (which skips SetModels on an empty list), refreshModelsList
// must push the empty list so removing the last model clears the sidebar + open
// session dropdowns immediately.
func TestRefreshModelsListClearsLiveDropdownsWhenEmpty(t *testing.T) {
	w := newTestWorkbench(t) // starts with one model ("test")
	w.SetHandlers(Handlers{GetModels: func() []config.ModelConfig { return nil }})

	w.refreshModelsList()

	if len(w.modelNames) != 0 {
		t.Errorf("modelNames = %v after an empty refresh, want empty (D1: last-model removal must clear dropdowns)", w.modelNames)
	}
	if len(w.models) != 0 {
		t.Errorf("models = %+v after an empty refresh, want empty", w.models)
	}
}

func TestRefreshModelsListUpdatesWhenNonEmpty(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{GetModels: func() []config.ModelConfig {
		return []config.ModelConfig{{Name: "a", DisplayName: "Alpha", Model: "ma"}}
	}})

	w.refreshModelsList()

	if len(w.models) != 1 || w.models[0].Name != "a" {
		t.Errorf("models = %+v, want the refreshed [a]", w.models)
	}
	if len(w.modelNames) != 1 || w.modelNames[0] != "Alpha" {
		t.Errorf("modelNames = %v, want [Alpha]", w.modelNames)
	}
}

// TestRefreshModelsAfterSavePreservesDropdownsWhenGetModelsEmpty locks the B1 fix:
// addEmpty / edit / setDefault use refreshModelsAfterSave (the GUARDED refresh),
// which must NOT blank the live dropdowns when GetModels returns empty — exactly
// what happens in remote mode when ListModels fails right after a successful
// mutate. Contrast with refreshModelsList (used only by remove), which is
// intentionally empty-aware (D1).
func TestRefreshModelsAfterSavePreservesDropdownsWhenGetModelsEmpty(t *testing.T) {
	w := newTestWorkbench(t) // starts with one model ("test")
	before := len(w.modelNames)
	w.SetHandlers(Handlers{GetModels: func() []config.ModelConfig { return nil }})

	w.refreshModelsAfterSave()

	if len(w.modelNames) != before {
		t.Errorf("modelNames = %v after a guarded refresh that returned empty, want preserved %d "+
			"(B1: a transient remote GetModels failure must not blank the live dropdowns)", w.modelNames, before)
	}
	if len(w.models) != before {
		t.Errorf("models = %+v after a guarded empty refresh, want preserved", w.models)
	}
}

// TestModelsDialogAddCatalogCancelReturnsToList locks the B2 fix: the catalog
// wizard now threads an onClose continuation so cancelling (or finishing) it
// returns the user to a fresh Models… list, instead of leaving them with no dialog.
// It drives the real async load (stubbed catalog) via drainPosted, then cancels at
// the provider step and asserts the models-dialog layer is back on top.
func TestModelsDialogAddCatalogCancelReturnsToList(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetModels: func() []config.ModelConfig {
			return []config.ModelConfig{{Name: "x", DisplayName: "X", Model: "m", APIType: "openai"}}
		},
		GetDefaultModel: func() string { return "x" },
		UpdateModel:     func(config.ModelConfig) error { return nil },
		AddModel:        func(config.ModelConfig) error { return nil },
		RemoveModel:     func(string) error { return nil },
		SetDefaultModel: func(string) error { return nil },
		GetModelCatalog: func(context.Context, bool) (modelsdev.Catalog, error) {
			return modelsdev.Catalog{
				"prov": modelsdev.Provider{ID: "prov", Name: "Prov", Models: map[string]modelsdev.Model{"m": {ID: "m", Name: "M"}}},
			}, nil
		},
	})
	w.showModelsDialog()

	clickTopButtonByText(t, w, "Catalog") // "Add from Catalog…" (mnemonic stripped on render)
	drainPostedEventually(t, w)           // off-thread catalog load -> provider picker

	if got := topLayerName(w); got != "catalog-picker" {
		t.Fatalf("after Add from Catalog, top layer = %q, want catalog-picker (the wizard did not advance)", got)
	}

	clickTopButtonByText(t, w, "Cancel") // abandon the wizard at the provider step

	if got := topLayerName(w); got != "models-dialog" {
		t.Fatalf("after cancelling the catalog wizard, top layer = %q, want models-dialog "+
			"(B2: cancelling must return to the Models… list, not leave no dialog open)", got)
	}
}

// TestModelsDialogRemoveLastClearsLiveDropdowns drives the full Remove flow and
// asserts the live dropdowns go empty — the end-to-end D1 acceptance criterion.
func TestModelsDialogRemoveLastClearsLiveDropdowns(t *testing.T) {
	backend := []config.ModelConfig{{Name: "solo", DisplayName: "Solo", Model: "m", APIType: "openai"}}
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetModels:       func() []config.ModelConfig { return append([]config.ModelConfig(nil), backend...) },
		GetDefaultModel: func() string { return "solo" },
		RemoveModel: func(name string) error {
			out := backend[:0:0]
			for _, m := range backend {
				if m.Name != name {
					out = append(out, m)
				}
			}
			backend = out
			return nil
		},
	})
	w.showModelsDialog()

	clickTopButtonByText(t, w, "Remove") // confirm: "Remove Solo? …"
	clickTopButtonByText(t, w, "Yes")    // -> RemoveModel("solo") -> backend empty -> refresh + reopen

	if len(w.modelNames) != 0 {
		t.Errorf("after removing the last model, modelNames = %v, want empty (live dropdowns not cleared)", w.modelNames)
	}
	if len(w.models) != 0 {
		t.Errorf("models = %+v after removing the last, want empty", w.models)
	}
	// The reopened dialog shows the empty-list placeholder.
	if !modelsDialogHasText(t, w, "No models configured") {
		t.Error("the dialog did not return to the empty-list state after removing the last model")
	}
}

// TestModelsDialogRemoveDefaultBlockedSurfacesMessage: removing the default (with
// others remaining) is blocked; the reason reaches the user as a confirm dialog.
func TestModelsDialogRemoveDefaultBlockedSurfacesMessage(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetModels: func() []config.ModelConfig {
			return []config.ModelConfig{
				{Name: "main", DisplayName: "Main", Model: "m1", APIType: "openai"},
				{Name: "alt", DisplayName: "Alt", Model: "m2", APIType: "openai"},
			}
		},
		GetDefaultModel: func() string { return "main" },
		RemoveModel: func(name string) error {
			if name == "main" {
				return errors.New("model is the default; set another default first")
			}
			return nil
		},
	})
	w.showModelsDialog()
	// The first row ("main", the default) is selected by default.
	clickTopButtonByText(t, w, "Remove")
	clickTopButtonByText(t, w, "Yes")

	if !modelsDialogHasText(t, w, "Could not remove") {
		t.Error("a blocked removal should surface an error confirm, not fail silently")
	}
	if !modelsDialogHasText(t, w, "default") {
		t.Error("the block message should tell the user the model is the default")
	}
}

// TestModelsDialogAddEmptyOpensForm: the Add Empty… button opens the shared model
// form (offline — no catalog touched).
func TestModelsDialogAddEmptyOpensForm(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetModels: func() []config.ModelConfig { return nil },
		AddModel:  func(config.ModelConfig) error { return nil },
	})
	w.showModelsDialog()

	clickTopButtonByText(t, w, "Empty")

	if got := topLayerName(w); got != "model-form" {
		t.Fatalf("Add Empty should open the shared model form layer; top layer = %q", got)
	}
}

// TestModelFormAddCallsAddModel: the Add form (nameEditable) hands the assembled
// config to AddModel on Save — the offline add path that must work with zero models.
func TestModelFormAddCallsAddModel(t *testing.T) {
	w := newTestWorkbench(t)
	var added *config.ModelConfig
	w.SetHandlers(Handlers{
		AddModel:  func(m config.ModelConfig) error { cp := m; added = &cp; return nil },
		GetModels: func() []config.ModelConfig { return nil },
	})
	// Pre-fill Name + Model so Save passes validation; nameEditable=true (Add mode).
	w.showModelForm("Add model — empty",
		config.ModelConfig{Name: "newmodel", DisplayName: "New", Model: "m1", APIType: "openai"},
		true, w.handlers.AddModel, nil)
	clickModelFormSave(t, w)

	if added == nil {
		t.Fatal("AddModel was not called on Add-form Save")
	}
	if added.Name != "newmodel" {
		t.Errorf("added model Name = %q, want newmodel", added.Name)
	}
	if added.Model != "m1" {
		t.Errorf("added model id = %q, want m1", added.Model)
	}
}

// TestModelFormEditIsPreFilled: the Edit form is pre-filled from the selected model.
func TestModelFormEditIsPreFilled(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{UpdateModel: func(config.ModelConfig) error { return nil }})
	w.showModelForm("Edit model — x",
		config.ModelConfig{Name: "x", DisplayName: "MyDisplay", Model: "mymodel", APIType: "openai"},
		false, w.handlers.UpdateModel, nil)

	grid := editorGrid(w)
	if _, _, ok := findRunes(grid, "MyDisplay"); !ok {
		t.Error("the Edit form's display field should be pre-filled with MyDisplay")
	}
	if _, _, ok := findRunes(grid, "mymodel"); !ok {
		t.Error("the Edit form's model-id field should be pre-filled with mymodel")
	}
}

func TestModelsDialogUnreachableWhenGetModelsNil(t *testing.T) {
	w := newTestWorkbench(t)
	// No GetModels handler wired — the dialog must not panic and must explain.
	w.showModelsDialog()
	if !modelsDialogHasText(t, w, "unavailable") {
		t.Error("with GetModels unwired, the dialog should report model management unavailable")
	}
}

// TestMenuAndPaletteSingleModelsEntryNoCatalog pins criterion (1): the standalone
// "Add Model from Catalog…" entry is gone from both the Config menu and the command
// palette, leaving a single "Models…" entry that opens the unified dialog.
func TestMenuAndPaletteSingleModelsEntryNoCatalog(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetSettings: func() config.SubAgentConfig { return config.SubAgentConfig{} },
		SetSettings: func(config.SubAgentConfig) {},
	})

	// Config menu (settingsItems): exactly one Models… entry, no Catalog entry.
	menuModels, menuCatalog := 0, 0
	for _, it := range w.settingsItems() {
		if strings.Contains(it.Label, "Models") {
			menuModels++
		}
		if strings.Contains(it.Label, "Catalog") {
			menuCatalog++
		}
	}
	if menuModels != 1 {
		t.Errorf("Config menu Models entries = %d, want exactly 1", menuModels)
	}
	if menuCatalog != 0 {
		t.Errorf("Config menu Catalog entries = %d, want 0 (standalone catalog item removed)", menuCatalog)
	}

	// Command palette (rawActions): single Models, no Add-model-from-catalog.
	palModels, palCatalog := 0, 0
	for _, a := range w.rawActions() {
		if a.name == "Models" {
			palModels++
		}
		if a.name == "Add model from catalog" {
			palCatalog++
		}
	}
	if palModels != 1 {
		t.Errorf("palette Models entries = %d, want 1", palModels)
	}
	if palCatalog != 0 {
		t.Errorf("palette 'Add model from catalog' entries = %d, want 0", palCatalog)
	}
}
