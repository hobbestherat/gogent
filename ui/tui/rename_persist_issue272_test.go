package ui

import "testing"

// Issue #272 (UI half): renaming a session must notify the backend (OnRename) so
// the new title reaches the session index. Before the fix SetSessionTitle only
// updated in-memory window/sidebar state and layout.json, never the backend.

func TestSetSessionTitleNotifiesBackend(t *testing.T) {
	w := newRestore266Workbench() // builds a workbench + desktop with models
	var gotID, gotTitle string
	calls := 0
	w.SetHandlers(Handlers{
		OnRename: func(id, title string) {
			gotID, gotTitle = id, title
			calls++
		},
	})
	w.AdoptSession(RestoredSession{ID: "s1", Title: "Old"})

	w.SetSessionTitle("s1", "New Title")

	if calls != 1 {
		t.Fatalf("OnRename called %d times, want 1", calls)
	}
	if gotID != "s1" || gotTitle != "New Title" {
		t.Fatalf("OnRename got (%q, %q), want (s1, New Title)", gotID, gotTitle)
	}
}

func TestSetSessionTitleEmptyIsIgnored(t *testing.T) {
	w := newRestore266Workbench()
	calls := 0
	w.SetHandlers(Handlers{OnRename: func(string, string) { calls++ }})
	w.AdoptSession(RestoredSession{ID: "s1", Title: "Old"})

	w.SetSessionTitle("s1", "   ") // blank rename is rejected before any notify

	if calls != 0 {
		t.Fatalf("OnRename called %d times for a blank title, want 0", calls)
	}
}
