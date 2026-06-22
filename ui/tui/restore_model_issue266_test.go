package ui

import (
	"testing"

	"gogent/internal/config"
)

// Issue #266 (UI half): a re-opened session must preselect the model it was last
// using in the dropdown — otherwise the next send goes to the default model
// (selectedModelName reads the dropdown). AdoptSession seeds the selection from
// RestoredSession.Model.

func newRestore266Workbench() *Workbench {
	return NewWorkbench([]*config.ModelConfig{
		{Name: "main", DisplayName: "Main", Model: "m1"},
		{Name: "alt", DisplayName: "Alt Model", Model: "m2"},
	})
}

func TestModelIndexByName(t *testing.T) {
	w := newRestore266Workbench()
	if got := w.modelIndexByName("alt"); got != 1 {
		t.Fatalf("modelIndexByName(alt) = %d, want 1", got)
	}
	if got := w.modelIndexByName("main"); got != 0 {
		t.Fatalf("modelIndexByName(main) = %d, want 0", got)
	}
	if got := w.modelIndexByName("ghost"); got != -1 {
		t.Fatalf("modelIndexByName(ghost) = %d, want -1", got)
	}
	if got := w.modelIndexByName(""); got != -1 {
		t.Fatalf("modelIndexByName(\"\") = %d, want -1", got)
	}
}

func TestAdoptSessionPreselectsRecordedModel(t *testing.T) {
	w := newRestore266Workbench()
	sw := w.AdoptSession(RestoredSession{ID: "s1", Title: "S1", Model: "alt"})
	if sw == nil || sw.modelSelect == nil {
		t.Fatalf("no session window / model select")
	}
	if got := sw.modelSelect.Value(); got != "Alt Model" {
		t.Fatalf("restored window model = %q, want %q (the recorded model, not the default)", got, "Alt Model")
	}
}

func TestAdoptSessionEmptyModelKeepsDefault(t *testing.T) {
	w := newRestore266Workbench()
	sw := w.AdoptSession(RestoredSession{ID: "s2", Title: "S2"}) // no Model
	if got := sw.modelSelect.Value(); got != "Main" {
		t.Fatalf("restored window with no recorded model = %q, want default %q", got, "Main")
	}
}
