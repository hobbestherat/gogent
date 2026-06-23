package gogent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogent/internal/model"
)

// Issue #325: the Saved Sessions browser must list ALL persisted sessions
// (active + archived), startup restore must STILL exclude archived sessions, and
// continuing an archived session must unarchive its on-disk base so later saves
// append to the active base and the session re-enters the active/restore set.

// saveAndArchive seeds one session, captures its (pre-archive) active base, then
// archives it. It returns the store, the active base prefix and the archived
// index path. The store is registered for cleanup by the caller's t.TempDir.
func saveAndArchive(t *testing.T, store *SessionStore, id string, msgs []model.Message) (activeBase, archivedIndex string) {
	t.Helper()
	us := buildSessionWithTranscript(id, msgs)
	if err := store.Save(us, "T-"+id); err != nil {
		t.Fatalf("Save(%s): %v", id, err)
	}
	activeBase = store.base[id]
	if activeBase == "" {
		t.Fatalf("no base recorded for %s after Save", id)
	}
	if err := store.Archive(id); err != nil {
		t.Fatalf("Archive(%s): %v", id, err)
	}
	return activeBase, indexFilePath(activeBase + archivedTag)
}

func metaByID(metas []SessionMeta, id string) (SessionMeta, bool) {
	for _, m := range metas {
		if m.ID == id {
			return m, true
		}
	}
	return SessionMeta{}, false
}

// TestListAllSessionsIncludesArchived is the core Step-1 behaviour: ListAllSessions
// returns BOTH an open (active) session and a closed (archived) one, each tagged
// with the right Archived bool, while the active-only ListSessions and ListActive
// keep excluding the archived one.
func TestListAllSessionsIncludesArchived(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	defer store.Close()

	// One session stays open (active)...
	open := buildSessionWithTranscript("open-1", []model.Message{{Role: model.RoleUser, Content: "open"}})
	if err := store.Save(open, "Open"); err != nil {
		t.Fatalf("Save open: %v", err)
	}
	// ...the other is closed (archived).
	saveAndArchive(t, store, "closed-1", []model.Message{{Role: model.RoleUser, Content: "closed"}})

	all, err := store.ListAllSessions()
	if err != nil {
		t.Fatalf("ListAllSessions: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListAllSessions returned %d sessions, want 2: %+v", len(all), all)
	}
	openMeta, ok := metaByID(all, "open-1")
	if !ok {
		t.Fatal("ListAllSessions missing the open session")
	}
	if openMeta.Archived {
		t.Errorf("open session tagged Archived=true, want false")
	}
	closedMeta, ok := metaByID(all, "closed-1")
	if !ok {
		t.Fatal("ListAllSessions missing the archived session")
	}
	if !closedMeta.Archived {
		t.Errorf("archived session tagged Archived=false, want true")
	}
	// The archived meta's File must point at the archived index so the browser can
	// re-open it; LoadSession must accept that path.
	if !strings.HasSuffix(strings.TrimSuffix(closedMeta.File, indexFileExt), archivedTag) {
		t.Errorf("archived File = %q, want a _session_archived index path", closedMeta.File)
	}

	// The active-only paths must NOT see the archived session (startup restore
	// stays active-only — the divergence the fix requires).
	active, err := store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(active) != 1 || active[0].ID != "open-1" {
		t.Fatalf("ListSessions (active-only) = %+v, want only open-1", active)
	}
	if active[0].Archived {
		t.Errorf("active-only ListSessions set Archived=true, want false always")
	}
	restored, err := store.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(restored) != 1 || restored[0].ID != "open-1" {
		t.Fatalf("ListActive = %+v, want only open-1 (archived excluded from restore)", restored)
	}
}

// TestListAllSessionsMetadataMatchesActive proves the archived listing carries the
// same index-derived metadata (id/title/messages) as the active listing would —
// the index read is identical for either base.
func TestListAllSessionsMetadataMatchesArchivedIndex(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	msgs := []model.Message{
		{Role: model.RoleUser, Content: "a"},
		{Role: model.RoleAssistant, Content: "b"},
		{Role: model.RoleUser, Content: "c"},
	}
	saveAndArchive(t, store, "meta-1", msgs)

	all, err := store.ListAllSessions()
	if err != nil {
		t.Fatalf("ListAllSessions: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 session, got %d", len(all))
	}
	m := all[0]
	if m.ID != "meta-1" {
		t.Errorf("ID = %q, want meta-1", m.ID)
	}
	if m.Title != "T-meta-1" {
		t.Errorf("Title = %q, want T-meta-1", m.Title)
	}
	if m.Messages != 3 {
		t.Errorf("Messages = %d, want 3 (summed from index across an archived base)", m.Messages)
	}
	if !m.Archived {
		t.Errorf("Archived = false, want true")
	}
}

// TestUnarchiveBaseRenamesBackToActive is the Step-3 round-trip: UnarchiveBase
// renames the base from _session_archived back to _session (index + every shard),
// returns the active index path, and the session re-enters the active/restore set
// (ListActive includes it) — exactly what a Continue must achieve so later saves
// append to the active base.
func TestUnarchiveBaseRenamesBackToActive(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	activeBase, archivedIndex := saveAndArchive(t, store, "session-1",
		[]model.Message{{Role: model.RoleUser, Content: "x"}})

	// Pre-condition: archived index exists on disk, active does not.
	if _, err := os.Stat(archivedIndex); err != nil {
		t.Fatalf("expected archived index on disk: %v", err)
	}
	if _, err := os.Stat(indexFilePath(activeBase)); !os.IsNotExist(err) {
		t.Fatalf("active index should not exist before unarchive (err=%v)", err)
	}
	// ...and ListActive excludes it.
	if got, _ := store.ListActive(); len(got) != 0 {
		t.Fatalf("ListActive before unarchive = %d, want 0", len(got))
	}

	newPath, err := store.UnarchiveBase(archivedIndex)
	if err != nil {
		t.Fatalf("UnarchiveBase: %v", err)
	}
	if newPath != indexFilePath(activeBase) {
		t.Errorf("UnarchiveBase returned %q, want the active index path %q", newPath, indexFilePath(activeBase))
	}
	if strings.HasSuffix(strings.TrimSuffix(newPath, indexFileExt), archivedTag) {
		t.Errorf("UnarchiveBase returned a still-archived path: %q", newPath)
	}

	// Post-condition: the rename actually happened on disk.
	if _, err := os.Stat(indexFilePath(activeBase)); err != nil {
		t.Errorf("active index missing after unarchive: %v", err)
	}
	if _, err := os.Stat(archivedIndex); !os.IsNotExist(err) {
		t.Errorf("archived index still present after unarchive (err=%v)", err)
	}

	// The session is back in the active/restore set.
	restored, err := store.ListActive()
	if err != nil {
		t.Fatalf("ListActive after unarchive: %v", err)
	}
	if len(restored) != 1 || restored[0].ID != "session-1" {
		t.Fatalf("ListActive after unarchive = %+v, want session-1", restored)
	}
	// And no longer reported as archived by the browser listing.
	all, _ := store.ListAllSessions()
	if m, ok := metaByID(all, "session-1"); !ok || m.Archived {
		t.Errorf("after unarchive ListAllSessions meta = %+v, want present & Archived=false", m)
	}
}

// TestUnarchiveBaseRenamesEveryShard proves the reverse-rename moves ALL shard
// files (not just the index) when a session spans multiple shards — a partial
// rename would leave transcript data stranded under the archived prefix.
func TestUnarchiveBaseRenamesEveryShard(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	const total = shardMaxEvents + 500 // forces a second shard
	us := buildSessionWithTranscript("s-big", buildLargeTranscript(total))
	if err := store.Save(us, "Big"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	nShards := len(store.shards["s-big"])
	if nShards < 2 {
		t.Fatalf("expected >=2 shards, got %d", nShards)
	}
	activeBase := store.base["s-big"]
	if err := store.Archive("s-big"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	archivedBase := activeBase + archivedTag

	// Every shard should currently live under the archived prefix.
	for i := 0; i < nShards; i++ {
		if _, err := os.Stat(shardFilePath(archivedBase, i)); err != nil {
			t.Fatalf("archived shard %d missing pre-unarchive: %v", i, err)
		}
	}

	if _, err := store.UnarchiveBase(indexFilePath(archivedBase)); err != nil {
		t.Fatalf("UnarchiveBase: %v", err)
	}

	for i := 0; i < nShards; i++ {
		if _, err := os.Stat(shardFilePath(activeBase, i)); err != nil {
			t.Errorf("active shard %d missing after unarchive: %v", i, err)
		}
		if _, err := os.Stat(shardFilePath(archivedBase, i)); !os.IsNotExist(err) {
			t.Errorf("archived shard %d still present after unarchive (err=%v)", i, err)
		}
	}

	// The transcript is still loadable from the now-active base.
	ls, err := store.LoadSession(indexFilePath(activeBase))
	if err != nil {
		t.Fatalf("LoadSession after unarchive: %v", err)
	}
	if ls.ID != "s-big" {
		t.Errorf("loaded ID = %q, want s-big", ls.ID)
	}
}

// TestUnarchiveBaseNoOpOnActiveBase proves passing an already-active base is a
// harmless no-op: same path back, no error, and nothing on disk is renamed.
func TestUnarchiveBaseNoOpOnActiveBase(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	us := buildSessionWithTranscript("active-1", []model.Message{{Role: model.RoleUser, Content: "x"}})
	if err := store.Save(us, "Active"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	activeIndex := indexFilePath(store.base["active-1"])

	got, err := store.UnarchiveBase(activeIndex)
	if err != nil {
		t.Fatalf("UnarchiveBase on active base errored: %v", err)
	}
	if got != activeIndex {
		t.Errorf("UnarchiveBase(active) = %q, want unchanged %q", got, activeIndex)
	}
	if _, err := os.Stat(activeIndex); err != nil {
		t.Errorf("active index disturbed by no-op unarchive: %v", err)
	}
}

// TestUnarchiveBaseAcceptsBareBase proves the bare base prefix (no .index suffix)
// is accepted just like the index path, mirroring LoadSession/Adopt.
func TestUnarchiveBaseAcceptsBareBase(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	activeBase, _ := saveAndArchive(t, store, "bare-1",
		[]model.Message{{Role: model.RoleUser, Content: "x"}})
	archivedBareBase := activeBase + archivedTag // no .index suffix

	got, err := store.UnarchiveBase(archivedBareBase)
	if err != nil {
		t.Fatalf("UnarchiveBase(bare): %v", err)
	}
	if got != indexFilePath(activeBase) {
		t.Errorf("UnarchiveBase(bare) = %q, want %q", got, indexFilePath(activeBase))
	}
	if _, err := os.Stat(indexFilePath(activeBase)); err != nil {
		t.Errorf("active index missing after bare-base unarchive: %v", err)
	}
}

// TestUnarchiveBaseMissingArchivedIndex proves a missing archived index yields an
// error rather than silently "succeeding" — the caller (ContinueSession) warns
// and falls back rather than loading a renamed-away file.
func TestUnarchiveBaseMissingArchivedIndex(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	missing := filepath.Join(dir, "2026-01-01T00-00-00_ghost_session_archived"+indexFileExt)
	_, err := store.UnarchiveBase(missing)
	if err == nil {
		t.Fatal("expected an error unarchiving a missing archived index")
	}
}
