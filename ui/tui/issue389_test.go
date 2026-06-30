package ui

import (
	"errors"
	"reflect"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/config"
)

func issue389Models() []*config.ModelConfig {
	return []*config.ModelConfig{
		{Name: "alpha", DisplayName: "Alpha", Model: "provider-alpha", ReasoningEffort: "low", Caps: config.ModelCapabilities{EffortOptions: []string{"low"}}},
		{Name: "beta", DisplayName: "Beta", Model: "provider-beta", ReasoningEffort: "old", Caps: config.ModelCapabilities{EffortOptions: []string{"old"}}},
	}
}

func TestIssue389SetModelsRefreshesOpenSessionByStableName(t *testing.T) {
	w := NewWorkbench(issue389Models())
	w.SetHandlers(Handlers{GetDefaultModel: func() string { return "beta" }})

	sw := w.NewSession()
	if got := sw.selectedModelName(); got != "beta" {
		t.Fatalf("initial selectedModelName() = %q, want beta", got)
	}
	if got := sw.modelSelect.Value(); got != "Beta" {
		t.Fatalf("initial label = %q, want Beta", got)
	}

	w.SetModels([]*config.ModelConfig{
		{Name: "alpha", DisplayName: "Alpha", Model: "provider-alpha", ReasoningEffort: "low", Caps: config.ModelCapabilities{EffortOptions: []string{"low"}}},
		{Name: "beta", DisplayName: "Beta Renamed", Model: "provider-beta-v2", ReasoningEffort: "deep", Caps: config.ModelCapabilities{EffortOptions: []string{"fresh", "deep"}}},
	})

	if got := sw.modelSelect.Value(); got != "Beta Renamed" {
		t.Fatalf("refreshed label = %q, want Beta Renamed", got)
	}
	if got := sw.selectedModelName(); got != "beta" {
		t.Fatalf("selectedModelName() after DisplayName edit = %q, want beta", got)
	}
	wantEfforts := []string{effortDefaultOption, "fresh", "deep"}
	if !reflect.DeepEqual(sw.effortSelect.Options, wantEfforts) {
		t.Fatalf("effort options = %#v, want %#v", sw.effortSelect.Options, wantEfforts)
	}
	if got := sw.selectedEffort(); got != "deep" {
		t.Fatalf("selected effort after refresh = %q, want deep", got)
	}

	post := w.NewSession()
	if got := post.modelSelect.Value(); got != "Beta Renamed" {
		t.Fatalf("new post-save session label = %q, want Beta Renamed", got)
	}
	if got := post.selectedModelName(); got != "beta" {
		t.Fatalf("new post-save selectedModelName() = %q, want beta", got)
	}
}

func TestIssue389SetModelsClampsWhenSelectedModelRemoved(t *testing.T) {
	w := NewWorkbench(issue389Models())
	w.SetHandlers(Handlers{GetDefaultModel: func() string { return "beta" }})
	sw := w.NewSession()

	w.SetModels([]*config.ModelConfig{
		{Name: "alpha", DisplayName: "Alpha Updated", Model: "provider-alpha", ReasoningEffort: "low", Caps: config.ModelCapabilities{EffortOptions: []string{"low"}}},
	})

	if got := sw.modelSelect.Value(); got != "Alpha Updated" {
		t.Fatalf("label after selected model removal = %q, want Alpha Updated", got)
	}
	if got := sw.selectedModelName(); got != "alpha" {
		t.Fatalf("selectedModelName() after selected model removal = %q, want alpha", got)
	}
}

func TestIssue389SetModelsPreservesDuplicateDisplayLabelByStableName(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{
		{Name: "alpha", DisplayName: "Shared", Model: "provider-alpha", ReasoningEffort: "low", Caps: config.ModelCapabilities{EffortOptions: []string{"low"}}},
		{Name: "beta", DisplayName: "Shared", Model: "provider-beta", ReasoningEffort: "deep", Caps: config.ModelCapabilities{EffortOptions: []string{"deep"}}},
	})
	sw := w.NewSession()
	sw.modelSelect.SetSelected(1)
	sw.rebuildEffortOptions()
	if got := sw.selectedModelName(); got != "beta" {
		t.Fatalf("pre-refresh selectedModelName() with duplicate labels = %q, want beta", got)
	}

	w.SetModels([]*config.ModelConfig{
		{Name: "alpha", DisplayName: "Shared", Model: "provider-alpha-v2", ReasoningEffort: "low", Caps: config.ModelCapabilities{EffortOptions: []string{"low"}}},
		{Name: "beta", DisplayName: "Shared", Model: "provider-beta-v2", ReasoningEffort: "wide", Caps: config.ModelCapabilities{EffortOptions: []string{"wide"}}},
	})

	if got := sw.selectedModelName(); got != "beta" {
		t.Fatalf("selectedModelName() after duplicate-label refresh = %q, want beta", got)
	}
	wantEfforts := []string{effortDefaultOption, "wide"}
	if !reflect.DeepEqual(sw.effortSelect.Options, wantEfforts) {
		t.Fatalf("effort options after duplicate-label refresh = %#v, want %#v", sw.effortSelect.Options, wantEfforts)
	}
}

// Issue #509: the standalone model editor (showModelEditor) was removed and its
// field set folded into the shared showModelForm builder the unified Models…
// dialog uses for Edit and Add Empty…. These two tests preserve the #389 invariant
// (a model edit refreshes the open session dropdown from the authoritative backend
// list, by stable Name) by driving that form directly in Edit mode with
// refreshModelsList as the post-save hook — the exact wiring the dialog's Edit
// action uses.
func TestIssue389ModelFormSaveRefreshesWorkbenchFromAuthoritativeModels(t *testing.T) {
	backend := []config.ModelConfig{{Name: "solo", DisplayName: "Before", Model: "provider-before"}}
	w := NewWorkbench([]*config.ModelConfig{{Name: "solo", DisplayName: "Before", Model: "provider-before"}})
	sw := w.NewSession()
	w.SetHandlers(Handlers{
		GetModels: func() []config.ModelConfig {
			out := make([]config.ModelConfig, len(backend))
			copy(out, backend)
			return out
		},
		UpdateModel: func(config.ModelConfig) error {
			backend[0].DisplayName = "After"
			backend[0].Model = "provider-after"
			return nil
		},
	})

	w.showModelForm("Edit model — solo", backend[0], false, w.handlers.UpdateModel, w.refreshModelsList)
	clickModelFormSave(t, w)

	if got := sw.modelSelect.Value(); got != "After" {
		t.Fatalf("open session label after form save = %q, want After", got)
	}
	if got := sw.selectedModelName(); got != "solo" {
		t.Fatalf("selectedModelName() after form save = %q, want solo", got)
	}
	if got := w.models[0].Model; got != "provider-after" {
		t.Fatalf("workbench model id after form save = %q, want provider-after", got)
	}
}

func TestIssue389ModelEditorDoesNotRefreshAfterFailedUpdate(t *testing.T) {
	backend := []config.ModelConfig{{Name: "solo", DisplayName: "Before", Model: "provider-before"}}
	w := NewWorkbench([]*config.ModelConfig{{Name: "solo", DisplayName: "Before", Model: "provider-before"}})
	sw := w.NewSession()
	var getCalls int
	w.SetHandlers(Handlers{
		GetModels: func() []config.ModelConfig {
			getCalls++
			out := make([]config.ModelConfig, len(backend))
			copy(out, backend)
			return out
		},
		UpdateModel: func(config.ModelConfig) error {
			backend[0].DisplayName = "After Failed Save"
			return errors.New("write failed")
		},
	})

	w.showModelForm("Edit model — solo", backend[0], false, w.handlers.UpdateModel, w.refreshModelsList)
	getCalls = 0 // the form does not call GetModels on open; reset keeps the assertion honest
	clickModelFormSave(t, w)

	if getCalls != 0 {
		t.Fatalf("GetModels called %d time(s) after failed UpdateModel; onSaved must not run on a failed save", getCalls)
	}
	if got := sw.modelSelect.Value(); got != "Before" {
		t.Fatalf("open session label changed after failed save = %q, want Before", got)
	}
}

func clickModelFormSave(t *testing.T, w *Workbench) {
	t.Helper()
	top := w.desktop.TopLayer()
	if top == nil || top.Root == nil {
		t.Fatal("model form did not open")
	}
	root := top.Root
	saveBounds := tv.Rect{X: root.Bounds.W - 24, Y: root.Bounds.H - 3, W: 9, H: 1}
	save := issue389FindComponent(root, func(c *tv.VisualComponent) bool {
		return c.Bounds == saveBounds && c.OnClickFn != nil
	})
	if save == nil {
		t.Fatalf("save button not found at %+v", saveBounds)
	}
	abs := save.AbsoluteBounds()
	x, y := abs.X+1, abs.Y
	save.OnClickFn(save, tui.ClickEvent{X: x, Y: y, Button: tui.MouseLeft, Down: true})
	save.OnClickFn(save, tui.ClickEvent{X: x, Y: y, Button: tui.MouseLeft, Down: false})
}

func issue389FindComponent(root *tv.VisualComponent, match func(*tv.VisualComponent) bool) *tv.VisualComponent {
	if root == nil {
		return nil
	}
	if match(root) {
		return root
	}
	for _, child := range root.Children() {
		if got := issue389FindComponent(child, match); got != nil {
			return got
		}
	}
	return nil
}
