package ui

import (
	"testing"

	"gogent/internal/config"
)

// Issue #306: a freshly created session must seed its model dropdown to the
// configured default model (GetDefaultModel), not silently to index 0. The
// backend's NewSession already builds on the default connection; without the UI
// seeding, selectedModelName() (which the next send reads) disagrees with the
// backend and the header shows the wrong model.

func newDefaultModel306Workbench(defaultName string) *Workbench {
	w := NewWorkbench([]*config.ModelConfig{
		{Name: "main", DisplayName: "Main", Model: "m1"},
		{Name: "alt", DisplayName: "Alt Model", Model: "m2"},
	})
	w.SetHandlers(Handlers{
		GetDefaultModel: func() string { return defaultName },
	})
	return w
}

func TestNewSessionSeedsDefaultModel(t *testing.T) {
	w := newDefaultModel306Workbench("alt")
	sw := w.NewSession()
	if sw == nil || sw.modelSelect == nil {
		t.Fatalf("no session window / model select")
	}
	if got := sw.modelSelect.Value(); got != "Alt Model" {
		t.Fatalf("new session model = %q, want %q (the configured default, not index 0)", got, "Alt Model")
	}
	if got := sw.selectedModelName(); got != "alt" {
		t.Fatalf("selectedModelName() = %q, want %q", got, "alt")
	}
}

func TestNewSessionEmptyDefaultKeepsIndexZero(t *testing.T) {
	w := newDefaultModel306Workbench("") // no default configured
	sw := w.NewSession()
	if got := sw.modelSelect.Value(); got != "Main" {
		t.Fatalf("new session with no default = %q, want first model %q", got, "Main")
	}
}

func TestNewSessionUnknownDefaultKeepsIndexZero(t *testing.T) {
	w := newDefaultModel306Workbench("ghost") // default names a model not in the list
	sw := w.NewSession()
	if got := sw.modelSelect.Value(); got != "Main" {
		t.Fatalf("new session with unknown default = %q, want first model %q (backend fallback)", got, "Main")
	}
}

// Without any GetDefaultModel handler wired, NewSession must still work and leave
// the first model selected (the prior behavior).
func TestNewSessionNoHandlerKeepsIndexZero(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{
		{Name: "main", DisplayName: "Main", Model: "m1"},
		{Name: "alt", DisplayName: "Alt Model", Model: "m2"},
	})
	sw := w.NewSession()
	if got := sw.modelSelect.Value(); got != "Main" {
		t.Fatalf("new session with no handler = %q, want first model %q", got, "Main")
	}
}
