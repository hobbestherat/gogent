package gogent

import (
	"os"
	"testing"

	"gogent/internal/model"
)

// Issue #325 (round 2): UnarchiveBase must NOT clobber a live active session when
// an active and an archived base share the same prefix (same created-second +
// same id — reachable because TUI ids like "session-N" are reused across process
// restarts). os.Rename would silently overwrite the open session's data, so the
// store guards against it. These tests lock in that guard and the data-safe
// ContinueSession fallback.

// seedActiveAndArchivedCollision creates an archived base AND a distinct active
// base sharing the same prefix (same id + CreatedAt), with different titles so a
// clobber is detectable. It returns the store, the active base prefix, and the
// archived index path.
func seedActiveAndArchivedCollision(t *testing.T, store *SessionStore, id string) (activeBase, archivedIndex string) {
	t.Helper()
	const created = int64(1_700_000_000) // fixed second -> deterministic base prefix

	// First session: saved then archived.
	first := buildSessionWithTranscript(id, []model.Message{{Role: model.RoleUser, Content: "old"}})
	first.CreatedAt = created
	if err := store.Save(first, "ARCHIVED-ONE"); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	activeBase = store.base[id]
	if err := store.Archive(id); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	archivedIndex = indexFilePath(activeBase + archivedTag)

	// Second session: same id + CreatedAt -> recomputes the SAME active base and
	// writes a fresh active index alongside the archived one.
	second := buildSessionWithTranscript(id, []model.Message{
		{Role: model.RoleUser, Content: "new-a"},
		{Role: model.RoleAssistant, Content: "new-b"},
	})
	second.CreatedAt = created
	if err := store.Save(second, "ACTIVE-LIVE"); err != nil {
		t.Fatalf("Save second: %v", err)
	}
	if store.base[id] != activeBase {
		t.Fatalf("expected the second save to reuse base %q, got %q", activeBase, store.base[id])
	}
	// Both must now exist on disk for the collision to be real.
	if _, err := os.Stat(indexFilePath(activeBase)); err != nil {
		t.Fatalf("active index missing, no collision set up: %v", err)
	}
	if _, err := os.Stat(archivedIndex); err != nil {
		t.Fatalf("archived index missing, no collision set up: %v", err)
	}
	return activeBase, archivedIndex
}

// TestUnarchiveBaseRefusesToClobberActive proves the guard: unarchiving an
// archived base whose active sibling already exists returns an error and the
// ACTIVE index path, and leaves BOTH on-disk indexes untouched (the live active
// session's data is preserved, not overwritten by the rename).
func TestUnarchiveBaseRefusesToClobberActive(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	activeBase, archivedIndex := seedActiveAndArchivedCollision(t, store, "session-1")

	got, err := store.UnarchiveBase(archivedIndex)
	if err == nil {
		t.Fatal("expected an error unarchiving onto an existing active base, got nil")
	}
	if got != indexFilePath(activeBase) {
		t.Errorf("returned path = %q, want the active index %q", got, indexFilePath(activeBase))
	}

	// The active (live) index must be intact — still the SECOND session's content.
	live, err := store.LoadSession(indexFilePath(activeBase))
	if err != nil {
		t.Fatalf("LoadSession(active) after guarded unarchive: %v", err)
	}
	if live.Title != "ACTIVE-LIVE" {
		t.Errorf("active session clobbered: Title = %q, want ACTIVE-LIVE", live.Title)
	}
	if n := len(live.Transcripts["root"]); n != 2 {
		t.Errorf("active transcript = %d msgs, want 2 (not overwritten by the 1-msg archived one)", n)
	}

	// The archived index must also still be present (not consumed by a half-rename).
	if _, err := os.Stat(archivedIndex); err != nil {
		t.Errorf("archived index disturbed by a refused unarchive: %v", err)
	}
}

// TestContinueSessionCollisionIsDataSafe drives the same collision through the
// full Gogent.ContinueSession path: even though UnarchiveBase errors, the caller
// adopts the returned active path, so Continue succeeds on the live session and
// neither on-disk base is destroyed.
func TestContinueSessionCollisionIsDataSafe(t *testing.T) {
	g := NewGogent(t.TempDir())
	if g.store == nil {
		t.Skip("session persistence unavailable")
	}
	activeBase, archivedIndex := seedActiveAndArchivedCollision(t, g.store, "session-1")

	ls, ok := g.ContinueSession(archivedIndex)
	if !ok {
		t.Fatal("ContinueSession returned ok=false on a collision, want a data-safe fallback to the active base")
	}
	// It must have adopted the ACTIVE base (the live session), not the archived one.
	if ls.Title != "ACTIVE-LIVE" {
		t.Errorf("continued Title = %q, want ACTIVE-LIVE (loaded the active base)", ls.Title)
	}
	if g.GetUserSession("session-1") == nil {
		t.Error("expected a live session after a data-safe continue")
	}
	// Neither index was destroyed by the rename guard.
	if _, err := os.Stat(indexFilePath(activeBase)); err != nil {
		t.Errorf("active index missing after collision continue: %v", err)
	}
	if _, err := os.Stat(archivedIndex); err != nil {
		t.Errorf("archived index missing after collision continue: %v", err)
	}
}
