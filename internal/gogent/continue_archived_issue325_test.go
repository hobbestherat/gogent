package gogent

import (
	"os"
	"strings"
	"testing"

	"gogent/internal/model"
)

// archivedFileFor seeds & archives a session through the given Gogent's store and
// returns the archived index path the browser would hand to Open/Continue (via
// g.ListSessions, which lists ALL sessions incl. archived after issue #325).
func archivedFileFor(t *testing.T, g *Gogent, id string) string {
	t.Helper()
	us := buildSessionWithTranscript(id, []model.Message{
		{Role: model.RoleUser, Content: "hello"},
		{Role: model.RoleAssistant, Content: "world"},
	})
	us.SetPrimaryModel("gpt-test")
	if err := g.store.Save(us, "T-"+id); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if err := g.store.Archive(id); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	for _, m := range g.ListSessions() {
		if m.ID == id {
			if !m.Archived {
				t.Fatalf("browser listing should tag %s Archived=true, got %+v", id, m)
			}
			return m.File
		}
	}
	t.Fatalf("archived session %s not surfaced by g.ListSessions()", id)
	return ""
}

// TestGogentListSessionsSurfacesArchived proves the browser-facing g.ListSessions
// now lists archived (closed) sessions — the user-visible bug in issue #325.
func TestGogentListSessionsSurfacesArchived(t *testing.T) {
	g := NewGogent(t.TempDir())
	if g.store == nil {
		t.Skip("session persistence unavailable")
	}
	archivedFileFor(t, g, "closed-7")

	metas := g.ListSessions()
	if len(metas) != 1 {
		t.Fatalf("g.ListSessions() = %d sessions, want 1 (the archived one)", len(metas))
	}
	if metas[0].ID != "closed-7" || !metas[0].Archived {
		t.Errorf("expected archived closed-7 in browser listing, got %+v", metas[0])
	}
}

// TestGogentContinueUnarchivesSession is the Step-3 acceptance: continuing an
// archived session unarchives its base (so a later save appends to the active
// base and the session re-enters the restore set) AND makes it live.
func TestGogentContinueUnarchivesSession(t *testing.T) {
	g := NewGogent(t.TempDir())
	if g.store == nil {
		t.Skip("session persistence unavailable")
	}
	file := archivedFileFor(t, g, "closed-1")

	// Sanity: archived index exists, active does not, before continuing.
	activeIndex := strings.Replace(file, archivedTag+indexFileExt, indexFileExt, 1)
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("archived index should exist pre-continue: %v", err)
	}
	if _, err := os.Stat(activeIndex); !os.IsNotExist(err) {
		t.Fatalf("active index should not exist pre-continue (err=%v)", err)
	}

	ls, ok := g.ContinueSession(file)
	if !ok {
		t.Fatal("ContinueSession on an archived session returned ok=false")
	}
	if ls.ID != "closed-1" {
		t.Errorf("continued session ID = %q, want closed-1", ls.ID)
	}
	if g.GetUserSession("closed-1") == nil {
		t.Error("continued session is not live")
	}
	if len(ls.Transcripts["root"]) != 2 {
		t.Errorf("continued transcript = %d msgs, want 2", len(ls.Transcripts["root"]))
	}

	// The base was unarchived on disk (rename _session_archived -> _session).
	if _, err := os.Stat(activeIndex); err != nil {
		t.Errorf("active index missing after continue (unarchive failed): %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("archived index still present after continue (err=%v)", err)
	}

	// The unarchived session re-enters the restore set.
	restored, err := g.store.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(restored) != 1 || restored[0].ID != "closed-1" {
		t.Errorf("ListActive after continue = %+v, want closed-1 (back in restore set)", restored)
	}
	// And the browser no longer marks it archived.
	if metas := g.ListSessions(); len(metas) != 1 || metas[0].Archived {
		t.Errorf("after continue browser listing = %+v, want closed-1 not archived", metas)
	}
}

// TestGogentOpenAnalysisLeavesArchived proves the READ-ONLY Open action does NOT
// unarchive: a closed session stays closed on disk, excluded from restore, after
// LoadSavedSession. (The deliberate asymmetry with Continue in issue #325.)
func TestGogentOpenAnalysisLeavesArchived(t *testing.T) {
	g := NewGogent(t.TempDir())
	if g.store == nil {
		t.Skip("session persistence unavailable")
	}
	file := archivedFileFor(t, g, "closed-2")

	ro, err := g.LoadSavedSession(file)
	if err != nil {
		t.Fatalf("LoadSavedSession: %v", err)
	}
	if ro.ID != "closed-2" || len(ro.Transcripts["root"]) != 2 {
		t.Errorf("read-only load wrong: id=%q msgs=%d", ro.ID, len(ro.Transcripts["root"]))
	}
	// No live session created.
	if g.GetUserSession("closed-2") != nil {
		t.Error("read-only Open created a live session, want none")
	}
	// Still archived on disk: the file the browser handed us is unchanged.
	if _, err := os.Stat(file); err != nil {
		t.Errorf("archived index disturbed by read-only Open: %v", err)
	}
	// Still excluded from restore, still marked archived in the browser.
	if restored, _ := g.store.ListActive(); len(restored) != 0 {
		t.Errorf("ListActive after read-only Open = %d, want 0 (stays closed)", len(restored))
	}
	if metas := g.ListSessions(); len(metas) != 1 || !metas[0].Archived {
		t.Errorf("after read-only Open browser listing = %+v, want still archived", metas)
	}
}
