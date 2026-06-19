package gogent

import (
	"path/filepath"
	"strings"
	"testing"

	"gogent/internal/model"
)

// TestSessionStoreListSessionsMetadata verifies the issue #58 index-only listing
// returns the per-session summary (title/date/turns/tokens/model/messages/file)
// straight from the index without loading transcripts.
func TestSessionStoreListSessionsMetadata(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	defer store.Close()

	msgs := []model.Message{
		{Role: model.RoleUser, Content: "hello"},
		{Role: model.RoleAssistant, Content: "hi there"},
		{Role: model.RoleUser, Content: "again"},
	}
	us := buildSessionWithTranscript("session-9", msgs)
	us.AddTokenUsage(120, 80)
	us.SetPrimaryModel("gpt-test")
	if err := store.Save(us, "Session 9"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	metas, err := store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("expected 1 session, got %d", len(metas))
	}
	m := metas[0]
	for _, tc := range []struct{ name, got, want string }{
		{"id", m.ID, "session-9"},
		{"title", m.Title, "Session 9"},
		{"model", m.Model, "gpt-test"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if m.CreatedAt == "" {
		t.Error("expected a non-empty CreatedAt")
	}
	if m.File == "" || !strings.HasSuffix(m.File, indexFileExt) {
		t.Errorf("expected an index file path, got %q", m.File)
	}
	if m.Messages != 3 {
		t.Errorf("Messages = %d, want 3", m.Messages)
	}
	if m.TokensIn != 120 || m.TokensOut != 80 {
		t.Errorf("tokens = %d/%d, want 120/80", m.TokensIn, m.TokensOut)
	}
	// Turns come from the live snapshot; no public turn setter exists, so it is
	// persisted as the snapshot's current value (here 0) — the point under test
	// is that the field round-trips through the index, verified below.
	if m.Turns != us.Snapshot().Turns {
		t.Errorf("Turns = %d, want snapshot %d", m.Turns, us.Snapshot().Turns)
	}
}

// TestSessionStoreListSessionsExcludesArchived verifies the browser listing
// skips archived sessions (shared activeBases filter).
func TestSessionStoreListSessionsExcludesArchived(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()
	us := buildSessionWithTranscript("session-1", []model.Message{{Role: model.RoleUser, Content: "a"}})
	if err := store.Save(us, "S1"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Archive("session-1"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	metas, _ := store.ListSessions()
	if len(metas) != 0 {
		t.Fatalf("expected 0 sessions after archive, got %d", len(metas))
	}
}

// TestSessionStoreListSessionsMessageCountAcrossShards proves the message count
// in the listing is summed from the index's shard table (every shard) rather
// than read from the active shard alone — the O(sessions) listing does not
// replay transcripts, unlike ListActive's current-shard restore.
func TestSessionStoreListSessionsMessageCountAcrossShards(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	const total = shardMaxEvents + 500 // forces a second shard
	us := buildSessionWithTranscript("s-big", buildLargeTranscript(total))
	if err := store.Save(us, "Big"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(store.shards["s-big"]) < 2 {
		t.Fatalf("expected >=2 shards, got %d", len(store.shards["s-big"]))
	}

	metas, _ := store.ListSessions()
	if len(metas) != 1 {
		t.Fatalf("expected 1 session, got %d", len(metas))
	}
	if metas[0].Messages != total {
		t.Errorf("Messages = %d, want %d (summed across shards)", metas[0].Messages, total)
	}

	// ListActive, by contrast, restores only the active shard's messages.
	active, _ := store.ListActive()
	if len(active[0].Transcripts["root"]) != total-shardMaxEvents {
		t.Errorf("ListActive root = %d, want %d (active shard only)",
			len(active[0].Transcripts["root"]), total-shardMaxEvents)
	}
}

// TestSessionStoreLoadSession verifies on-demand loading of one session's
// transcript by index file path and by bare base prefix.
func TestSessionStoreLoadSession(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()
	us := buildSessionWithTranscript("session-9", []model.Message{
		{Role: model.RoleUser, Content: "hello"},
		{Role: model.RoleAssistant, Content: "hi"},
	})
	if err := store.Save(us, "Session 9"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	metas, _ := store.ListSessions()
	if len(metas) != 1 {
		t.Fatalf("expected 1 session, got %d", len(metas))
	}

	ls, err := store.LoadSession(metas[0].File)
	if err != nil {
		t.Fatalf("LoadSession by file: %v", err)
	}
	if ls.ID != "session-9" || ls.Title != "Session 9" {
		t.Errorf("LoadSession meta = id=%q title=%q", ls.ID, ls.Title)
	}
	if root := ls.Transcripts["root"]; len(root) != 2 || root[0].Content != "hello" {
		t.Errorf("LoadSession transcript = %+v", root)
	}

	// A bare base prefix (without the .index suffix) is also accepted.
	base := strings.TrimSuffix(metas[0].File, indexFileExt)
	if _, err := store.LoadSession(base); err != nil {
		t.Fatalf("LoadSession by base: %v", err)
	}

	if _, err := store.LoadSession(filepath.Join(dir, "nope")); err == nil {
		t.Error("expected an error loading a missing session")
	}
}

// TestSessionStoreLoadSessionMissing proves a missing index yields an error
// rather than a silent empty session.
func TestSessionStoreLoadSessionMissing(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()
	if _, err := store.LoadSession("does-not-exist"); err == nil {
		t.Fatal("expected an error for a missing session index")
	}
}

// TestGogentSavedSessions exercises the issue #58 bridge: the index-only listing,
// on-demand continuation (which adopts a live backend session) and read-only
// loading. It drives the store directly to seed a saved session.
func TestGogentSavedSessions(t *testing.T) {
	g := NewGogent(t.TempDir())
	if g.store == nil {
		t.Skip("session persistence unavailable")
	}
	us := buildSessionWithTranscript("session-7", []model.Message{
		{Role: model.RoleUser, Content: "hello"},
		{Role: model.RoleAssistant, Content: "world"},
	})
	us.SetPrimaryModel("gpt-test")
	if err := g.store.Save(us, "Session 7"); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	metas := g.ListSessions()
	if len(metas) != 1 {
		t.Fatalf("expected 1 saved session, got %d", len(metas))
	}
	if metas[0].ID != "session-7" || metas[0].Model != "gpt-test" {
		t.Errorf("unexpected meta: %+v", metas[0])
	}

	// Continue adopts it as a live session seeded with the saved transcript.
	ls, ok := g.ContinueSession(metas[0].File)
	if !ok {
		t.Fatal("expected ContinueSession to adopt the session")
	}
	if g.GetUserSession("session-7") == nil {
		t.Error("expected the continued session to be live")
	}
	if len(ls.Transcripts["root"]) != 2 {
		t.Errorf("expected 2 restored messages, got %d", len(ls.Transcripts["root"]))
	}

	// Continuing a session that is already live is a no-op.
	if _, ok := g.ContinueSession(metas[0].File); ok {
		t.Error("expected continuing an already-live session to be a no-op")
	}

	// Read-only load returns the transcript without creating a live session.
	ro, err := g.LoadSavedSession(metas[0].File)
	if err != nil {
		t.Fatalf("LoadSavedSession: %v", err)
	}
	if len(ro.Transcripts["root"]) != 2 || ro.Transcripts["root"][1].Content != "world" {
		t.Errorf("read-only transcript wrong: %+v", ro.Transcripts["root"])
	}
}

// TestGogentListSessionsNoStore verifies the listing degrades cleanly when
// persistence is unavailable rather than panicking.
func TestGogentListSessionsNoStore(t *testing.T) {
	g := NewGogent(t.TempDir())
	g.store = nil
	if metas := g.ListSessions(); metas != nil {
		t.Errorf("expected nil listing with no store, got %v", metas)
	}
	if _, err := g.LoadSavedSession("anything"); err == nil {
		t.Error("expected an error from LoadSavedSession with no store")
	}
	if _, ok := g.ContinueSession("anything"); ok {
		t.Error("expected ok=false from ContinueSession with no store")
	}
}
