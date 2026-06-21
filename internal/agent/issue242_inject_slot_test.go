package agent

import "testing"

// Unit-level contract test for the pendingNote slot that the #242 rendering
// change makes more user-visible. The slot is documented as "latest-wins"
// (user_session.go: "a newer note replaces an undrained one"); an interjected
// "You (clarification):" record renders in the UI for EVERY Interject press,
// but only the note held in this single slot is spliced into the model's next
// request. These tests pin the slot semantics so the display/delivery
// asymmetry is a deliberate, visible contract rather than an accident.
//
// (The slot is single-use and latest-wins; rapid unsynchronised interjections
// race on its mutex, so "latest" is the last writer, not strictly the last
// Interject press. That race is out of #242 scope.)

// TestInjectUserNoteLatestWins verifies a newer note replaces an undrained one,
// the slot drains once, and whitespace-only notes are ignored (do not clobber a
// real pending note).
func TestInjectUserNoteLatestWins(t *testing.T) {
	us, _ := newLoopSession(t, "http://127.0.0.1:1") // URL never dialed; no loop runs here.

	us.InjectUserNote("first")
	us.InjectUserNote("second")
	if got := us.takePendingNote(); got != "second" {
		t.Errorf("takePendingNote = %q, want %q (newer note should replace undrained one)", got, "second")
	}
	// The slot is single-use: a second drain yields nothing.
	if got := us.takePendingNote(); got != "" {
		t.Errorf("takePendingNote after drain = %q, want empty (single-use slot)", got)
	}

	// A whitespace-only note is ignored and must NOT overwrite a real one.
	us.InjectUserNote("real note")
	us.InjectUserNote("   ")
	if got := us.takePendingNote(); got != "real note" {
		t.Errorf("takePendingNote = %q, want %q (whitespace note should be ignored, not clobber the slot)", got, "real note")
	}
}
