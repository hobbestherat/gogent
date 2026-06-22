package gogent

import "testing"

// Issue #272: renaming a session must persist the new title to the index right
// away, so the Sessions browser (which searches the index by title) finds it by
// the new name without waiting for another message turn.

func findMetaTitle(t *testing.T, store *SessionStore, id string) string {
	t.Helper()
	metas, err := store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, m := range metas {
		if m.ID == id {
			return m.Title
		}
	}
	t.Fatalf("session %q not found in index listing", id)
	return ""
}

func TestRenameSessionPersistsTitleToIndex(t *testing.T) {
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	defer store.Close()
	g.store = store

	g.NewSession("s-272")
	g.SetSessionTitle("s-272", "Old Name")
	g.persistSession("s-272") // first save: index records "Old Name"

	if got := findMetaTitle(t, store, "s-272"); got != "Old Name" {
		t.Fatalf("pre-rename index title = %q, want %q", got, "Old Name")
	}

	// The bug: before the fix, only SetSessionTitle (in-memory) ran on rename and
	// the index kept the stale title. RenameSession persists it immediately.
	g.RenameSession("s-272", "New Name")

	if got := findMetaTitle(t, store, "s-272"); got != "New Name" {
		t.Fatalf("post-rename index title = %q, want %q (rename not persisted)", got, "New Name")
	}
}
