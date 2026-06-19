package gogent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/model"
)

// buildSessionWithTranscript makes a UserSession whose root agent has a known
// transcript, for store round-trip tests.
func buildSessionWithTranscript(id string, msgs []model.Message) *agent.UserSession {
	sess := model.NewModelSession("main", model.NewModelConnection())
	sess.ReplaceTranscript(msgs)
	root := agent.NewAgent("root", sess)
	us := agent.NewUserSession(id, root)
	return us
}

// buildLargeTranscript makes n distinct user/assistant messages.
func buildLargeTranscript(n int) []model.Message {
	msgs := make([]model.Message, n)
	for i := range msgs {
		role := model.RoleUser
		if i%2 == 1 {
			role = model.RoleAssistant
		}
		msgs[i] = model.Message{Role: role, Content: "msg-" + strconv.Itoa(i)}
	}
	return msgs
}

// activeShardPath returns the current (latest) shard file path for a session.
// Test helper; assumes the store is quiescent (no concurrent saves).
func (s *SessionStore) activeShardPath(id string) string {
	sms := s.shards[id]
	if len(sms) == 0 {
		return ""
	}
	return shardFilePath(s.base[id], sms[len(sms)-1].Index)
}

func TestSessionStoreSaveListArchive(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	defer store.Close()

	msgs := []model.Message{
		{Role: model.RoleUser, Content: "hello"},
		{Role: model.RoleAssistant, Content: "hi there"},
	}
	us := buildSessionWithTranscript("session-7", msgs)

	if err := store.Save(us, "Session 7"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 active session, got %d", len(loaded))
	}
	ls := loaded[0]
	if ls.ID != "session-7" || ls.Title != "Session 7" {
		t.Fatalf("unexpected meta: id=%q title=%q", ls.ID, ls.Title)
	}
	root := ls.Transcripts["root"]
	if len(root) != 2 || root[0].Content != "hello" || root[1].Content != "hi there" {
		t.Fatalf("unexpected root transcript: %+v", root)
	}

	// Archiving removes it from the active listing and renames every file.
	if err := store.Archive("session-7"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	loaded, err = store.ListActive()
	if err != nil {
		t.Fatalf("ListActive after archive: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected 0 active sessions after archive, got %d", len(loaded))
	}
	idx, _ := filepath.Glob(filepath.Join(dir, "*"+archivedTag+indexFileExt))
	if len(idx) != 1 {
		t.Fatalf("expected 1 archived index, got %d", len(idx))
	}
}

func TestSessionStoreAdoptContinues(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()
	us := buildSessionWithTranscript("session-1", []model.Message{{Role: model.RoleUser, Content: "a"}})
	if err := store.Save(us, "Session 1"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, _ := store.ListActive()
	if len(loaded) != 1 {
		t.Fatalf("expected 1 session")
	}
	// Adopt the existing file in a fresh store, append a message, and re-save:
	// the active shard is reused (still a single shard) with the new message.
	store2, _ := NewSessionStore(dir)
	defer store2.Close()
	store2.Adopt("session-1", loaded[0].File, us.RootAgent.ListAllAgents())
	us.RootAgent.ThoughtTrain.AppendMessages(model.Message{Role: model.RoleAssistant, Content: "b"})
	if err := store2.Save(us, "Session 1"); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if len(store2.shards["session-1"]) != 1 {
		t.Fatalf("expected delta save to reuse the single shard, got %d shards", len(store2.shards["session-1"]))
	}
	loaded2, _ := store2.ListActive()
	if len(loaded2[0].Transcripts["root"]) != 2 {
		t.Fatalf("expected 2 messages after continuation, got %d", len(loaded2[0].Transcripts["root"]))
	}
}

// failingWriter is an io.Writer that always errors, so json.Encoder.Encode can't
// flush a record — used to prove encode errors are surfaced, not swallowed.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("disk full")
}

// TestEncodeMessagesSurfacesErrors guards issue #17: a failed encode is
// aggregated and returned, not dropped with "_ =" while Save reports success.
func TestEncodeMessagesSurfacesErrors(t *testing.T) {
	us := buildSessionWithTranscript("session-x", []model.Message{
		{Role: model.RoleUser, Content: "hello"},
	})
	_, err := encodeMessages(json.NewEncoder(failingWriter{}), us.RootAgent.ListAllAgents(), func(string) int { return 0 })
	if err == nil {
		t.Fatal("expected an encode error, got nil (error was swallowed)")
	}
	if !strings.Contains(err.Error(), "encode message for agent") {
		t.Errorf("expected the error to identify the failing record, got: %v", err)
	}
}

// TestEncodeMessagesWritesRecords is the positive counterpart: a working sink
// receives exactly one line per transcript message.
func TestEncodeMessagesWritesRecords(t *testing.T) {
	us := buildSessionWithTranscript("session-x", []model.Message{
		{Role: model.RoleUser, Content: "hello"},
		{Role: model.RoleAssistant, Content: "hi"},
	})
	var buf bytes.Buffer
	n, err := encodeMessages(json.NewEncoder(&buf), us.RootAgent.ListAllAgents(), func(string) int { return 0 })
	if err != nil {
		t.Fatalf("encodeMessages: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 records, got %d", n)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines, got %d (%q)", len(lines), buf.String())
	}
}

// countRecords parses a JSONL shard file and returns the number of "meta" and
// "message" records on disk.
func countRecords(t *testing.T, path string) (messages, meta int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open session file: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec jsonlRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		switch rec.Kind {
		case "meta":
			meta++
		case "message":
			messages++
		}
	}
	return messages, meta
}

// TestSessionStoreSaveAppendsDelta verifies the core fix for issue #21: a second
// save after new messages are appended extends the active shard with only the new
// lines (an append to the same inode) instead of rewriting the whole transcript.
func TestSessionStoreSaveAppendsDelta(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()
	us := buildSessionWithTranscript("s-delta", []model.Message{
		{Role: model.RoleUser, Content: "one"},
		{Role: model.RoleAssistant, Content: "two"},
	})
	if err := store.Save(us, "Delta"); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	path := store.activeShardPath("s-delta")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Grow the transcript and save again.
	us.RootAgent.ThoughtTrain.AppendMessages(
		model.Message{Role: model.RoleUser, Content: "three"},
		model.Message{Role: model.RoleAssistant, Content: "four"},
	)
	if err := store.Save(us, "Delta"); err != nil {
		t.Fatalf("Save 2: %v", err)
	}

	// The delta save appended to the existing shard, so the inode is unchanged.
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Errorf("expected delta save to append in place (same inode), got a replaced file")
	}

	msgs, _ := countRecords(t, path)
	if msgs != 4 {
		t.Errorf("expected 4 message records (no duplication), got %d", msgs)
	}

	loaded, _ := store.ListActive()
	if len(loaded) != 1 || len(loaded[0].Transcripts["root"]) != 4 {
		t.Fatalf("expected 4 root messages after reload, got %+v", loaded)
	}
	if loaded[0].Transcripts["root"][3].Content != "four" {
		t.Errorf("last message not preserved: %+v", loaded[0].Transcripts["root"])
	}
}

// TestSessionStoreSaveIdempotent proves that re-saving with no transcript
// changes is a true no-op: the shard is byte-identical and not even reopened for
// writing (the delta path appends nothing).
func TestSessionStoreSaveIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()
	us := buildSessionWithTranscript("s-noop", []model.Message{
		{Role: model.RoleUser, Content: "hello"},
	})
	if err := store.Save(us, "Noop"); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	path := store.activeShardPath("s-noop")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	stat1, _ := os.Stat(path)

	if err := store.Save(us, "Noop"); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read 2: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("no-op save changed the shard:\nbefore: %q\nafter:  %q", first, second)
	}
	stat2, _ := os.Stat(path)
	if !os.SameFile(stat1, stat2) {
		t.Errorf("no-op save replaced the shard (expected no change at all)")
	}
}

// TestSessionStoreSaveCompactionRewrites verifies that when an agent's transcript
// is replaced in place (a compaction), Save falls back to a full atomic rewrite:
// the shard ends up with only the compacted transcript and the inode changes
// (temp + rename).
func TestSessionStoreSaveCompactionRewrites(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()
	us := buildSessionWithTranscript("s-compact", []model.Message{
		{Role: model.RoleUser, Content: "old-1"},
		{Role: model.RoleAssistant, Content: "old-2"},
		{Role: model.RoleUser, Content: "old-3"},
	})
	if err := store.Save(us, "Compact"); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	path := store.activeShardPath("s-compact")
	before, _ := os.Stat(path)

	// Simulate a compaction: the transcript is replaced with a shorter digest,
	// which advances the transcript epoch.
	us.RootAgent.ThoughtTrain.ReplaceTranscript([]model.Message{
		{Role: model.RoleUser, Content: "[summary]"},
	})
	if err := store.Save(us, "Compact"); err != nil {
		t.Fatalf("Save 2: %v", err)
	}

	// A compaction triggers a full rewrite (temp + rename), replacing the shard.
	after, _ := os.Stat(path)
	if os.SameFile(before, after) {
		t.Errorf("expected compaction to rewrite the shard (new inode), got same inode")
	}

	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "old-1") {
		t.Errorf("compacted shard still contains stale old message: %s", b)
	}
	msgs, _ := countRecords(t, path)
	if msgs != 1 {
		t.Errorf("expected 1 message after compaction, got %d", msgs)
	}
	loaded, _ := store.ListActive()
	if len(loaded[0].Transcripts["root"]) != 1 || loaded[0].Transcripts["root"][0].Content != "[summary]" {
		t.Fatalf("expected reloaded transcript to be just the digest, got %+v", loaded[0].Transcripts["root"])
	}
}

// TestSessionStoreSaveTitleChangeRewrites verifies that a title change is
// persisted via the index (where the title lives) without rewriting transcript
// shards.
func TestSessionStoreSaveTitleChangeRewrites(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()
	us := buildSessionWithTranscript("s-title", []model.Message{
		{Role: model.RoleUser, Content: "hi"},
	})
	if err := store.Save(us, "Old Title"); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	if err := store.Save(us, "New Title"); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	loaded, _ := store.ListActive()
	if len(loaded) != 1 || loaded[0].Title != "New Title" {
		t.Fatalf("expected title 'New Title' after rename, got %+v", loaded)
	}
}

// TestSessionStoreSaveNewSubAgentAppends verifies that a sub-agent spawned after
// the first save has its transcript appended (not a full rewrite) and that the
// session round-trips with both agents' transcripts intact.
func TestSessionStoreSaveNewSubAgentAppends(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()
	us := buildSessionWithTranscript("s-sub", []model.Message{
		{Role: model.RoleUser, Content: "root-q"},
	})
	if err := store.Save(us, "Sub"); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	path := store.activeShardPath("s-sub")
	before, _ := os.Stat(path)

	// Spawn a sub-agent with its own transcript after the first save.
	subSess := model.NewModelSession("sub", model.NewModelConnection())
	subSess.AppendMessages(
		model.Message{Role: model.RoleUser, Content: "sub-task"},
		model.Message{Role: model.RoleAssistant, Content: "sub-done"},
	)
	sub := agent.NewAgent("worker-1", subSess)
	us.RootAgent.AddSubAgent(sub)

	if err := store.Save(us, "Sub"); err != nil {
		t.Fatalf("Save 2: %v", err)
	}

	// A new agent appends, so the active shard keeps its inode.
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Errorf("expected new-agent save to append in place, got replaced file")
	}

	loaded, _ := store.ListActive()
	ls := loaded[0]
	if len(ls.Transcripts["root"]) != 1 || ls.Transcripts["root"][0].Content != "root-q" {
		t.Errorf("root transcript wrong: %+v", ls.Transcripts["root"])
	}
	if len(ls.Transcripts["worker-1"]) != 2 {
		t.Errorf("sub-agent transcript wrong: %+v", ls.Transcripts["worker-1"])
	}
}

// TestSessionStoreSaveDeltaGrowsAcrossTurns is the end-to-end scenario from
// issue #21: many turns append one message at a time, and each save adds exactly
// one line to the active shard rather than rewriting the growing transcript.
func TestSessionStoreSaveDeltaGrowsAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()
	us := buildSessionWithTranscript("s-grow", []model.Message{
		{Role: model.RoleUser, Content: "turn-0"},
	})
	if err := store.Save(us, "Grow"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := store.activeShardPath("s-grow")

	const turns = 8
	for i := 1; i <= turns; i++ {
		us.RootAgent.ThoughtTrain.AppendMessages(
			model.Message{Role: model.RoleAssistant, Content: "resp-" + strconv.Itoa(i)},
		)
		if err := store.Save(us, "Grow"); err != nil {
			t.Fatalf("Save turn %d: %v", i, err)
		}
		msgs, _ := countRecords(t, path)
		if msgs != 1+i { // initial message + one per turn, no duplication
			t.Fatalf("after turn %d: expected %d message records, got %d", i, 1+i, msgs)
		}
	}
	loaded, _ := store.ListActive()
	if len(loaded[0].Transcripts["root"]) != 1+turns {
		t.Fatalf("expected %d messages after reload, got %d", 1+turns, len(loaded[0].Transcripts["root"]))
	}
}

// TestSessionStoreShardsLargeTranscript is the core of issue #26: a transcript
// larger than the shard cap is split across multiple bounded shard files rather
// than one unbounded file. Each shard stays under both the event and byte caps.
func TestSessionStoreShardsLargeTranscript(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	const total = shardMaxEvents + 500 // forces a second shard
	us := buildSessionWithTranscript("s-big", buildLargeTranscript(total))
	if err := store.Save(us, "Big"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	sms := store.shards["s-big"]
	if len(sms) != 2 {
		t.Fatalf("expected 2 shards for %d events (cap %d), got %d", total, shardMaxEvents, len(sms))
	}
	if sms[0].Events != shardMaxEvents || sms[1].Events != total-shardMaxEvents {
		t.Fatalf("unexpected shard split: %+v", sms)
	}
	for i, sm := range sms {
		fi, err := os.Stat(shardFilePath(store.base["s-big"], sm.Index))
		if err != nil {
			t.Fatalf("shard %d missing on disk: %v", i, err)
		}
		if fi.Size() > shardMaxBytes {
			t.Errorf("shard %d exceeds byte cap: %d bytes", i, fi.Size())
		}
	}
}

// TestSessionStoreCurrentShardRestore verifies the issue #26 restore fix: a long
// session is restored from its current (latest) shard only, so restore cost is
// bounded even though the full history remains on disk.
func TestSessionStoreCurrentShardRestore(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	const total = shardMaxEvents + 500
	us := buildSessionWithTranscript("s-big", buildLargeTranscript(total))
	if err := store.Save(us, "Big"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A fresh store reads only the index + the active shard.
	store2, _ := NewSessionStore(dir)
	defer store2.Close()
	loaded, err := store2.ListActive()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("ListActive: %v len=%d", err, len(loaded))
	}
	root := loaded[0].Transcripts["root"]
	if len(root) != total-shardMaxEvents {
		t.Fatalf("expected restore to load only the active shard (%d msgs), got %d", total-shardMaxEvents, len(root))
	}
	// The older shard is still on disk, just not loaded into the transcript.
	base := strings.TrimSuffix(loaded[0].File, indexFileExt)
	if _, err := os.Stat(shardFilePath(base, 0)); err != nil {
		t.Errorf("older shard 0 should still exist on disk: %v", err)
	}
}

// TestSessionStoreAdoptPreservesOlderShards verifies that after a current-shard
// restore, a continued save is a delta that appends to the active shard and
// leaves the older (archived-on-disk) shards intact — no data loss.
func TestSessionStoreAdoptPreservesOlderShards(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	const total = shardMaxEvents + 500
	us := buildSessionWithTranscript("s-big", buildLargeTranscript(total))
	if err := store.Save(us, "Big"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	store2, _ := NewSessionStore(dir)
	defer store2.Close()
	loaded, _ := store2.ListActive()

	// Continue the session from the restored (active-shard) transcript.
	base := strings.TrimSuffix(loaded[0].File, indexFileExt)
	us2 := buildSessionWithTranscript("s-big", loaded[0].Transcripts["root"])
	store2.Adopt("s-big", loaded[0].File, us2.RootAgent.ListAllAgents())
	us2.RootAgent.ThoughtTrain.AppendMessages(model.Message{Role: model.RoleAssistant, Content: "more"})
	if err := store2.Save(us2, "Big"); err != nil {
		t.Fatalf("Save after adopt: %v", err)
	}

	// Older shard 0 is untouched; the active shard gained exactly one message.
	if _, err := os.Stat(shardFilePath(base, 0)); err != nil {
		t.Errorf("older shard 0 should still exist: %v", err)
	}
	msgs, _ := countRecords(t, shardFilePath(base, 1))
	if msgs != (total-shardMaxEvents)+1 {
		t.Errorf("expected active shard to grow by one message, got %d", msgs)
	}
}

// TestSessionStoreCompactionAfterRestoreRewrites locks in the fix for a restore
// edge case: Adopt captures each restored agent's transcript epoch as a baseline
// so that a compaction on the first restored turn (epoch advance) is detected
// and rewritten, rather than silently dropped by a delta that appends nothing.
func TestSessionStoreCompactionAfterRestoreRewrites(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	us := buildSessionWithTranscript("s-c", []model.Message{
		{Role: model.RoleUser, Content: "one"},
		{Role: model.RoleAssistant, Content: "two"},
	})
	if err := store.Save(us, "C"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Restore into a fresh store, then compact before the next save.
	store2, _ := NewSessionStore(dir)
	defer store2.Close()
	loaded, _ := store2.ListActive()
	us2 := buildSessionWithTranscript("s-c", loaded[0].Transcripts["root"])
	store2.Adopt("s-c", loaded[0].File, us2.RootAgent.ListAllAgents())
	us2.RootAgent.ThoughtTrain.ReplaceTranscript([]model.Message{
		{Role: model.RoleUser, Content: "[summary]"},
	})
	if err := store2.Save(us2, "C"); err != nil {
		t.Fatalf("Save after compaction: %v", err)
	}

	// The compaction must be persisted (not silently dropped): the reloaded
	// transcript is the summary alone, with no stale "one"/"two" tail.
	reloaded, _ := store2.ListActive()
	root := reloaded[0].Transcripts["root"]
	if len(root) != 1 || root[0].Content != "[summary]" {
		t.Fatalf("expected compaction persisted as the summary, got %+v", root)
	}
}

// TestSessionStoreIndexIsSourceOfTruth verifies the index carries the session
// meta and shard table, so listing does not have to parse transcript lines.
func TestSessionStoreIndexIsSourceOfTruth(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	us := buildSessionWithTranscript("s-idx", []model.Message{{Role: model.RoleUser, Content: "x"}})
	if err := store.Save(us, "Index Title"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, _ := store.ListActive()
	idx, err := loadIndexFile(strings.TrimSuffix(loaded[0].File, indexFileExt))
	if err != nil {
		t.Fatalf("loadIndexFile: %v", err)
	}
	if idx.SessionID != "s-idx" || idx.Title != "Index Title" {
		t.Errorf("index meta wrong: %+v", idx)
	}
	if len(idx.Shards) != 1 || idx.Shards[0].Events != 1 {
		t.Errorf("index shard table wrong: %+v", idx.Shards)
	}
}

// TestSessionStoreArchiveRenamesAllFiles verifies that archiving renames the
// index and every shard, not just one file.
func TestSessionStoreArchiveRenamesAllFiles(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	defer store.Close()

	const total = shardMaxEvents + 10
	us := buildSessionWithTranscript("s-arc", buildLargeTranscript(total))
	if err := store.Save(us, "Arc"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(store.shards["s-arc"]) < 2 {
		t.Fatalf("expected >=2 shards before archive, got %d", len(store.shards["s-arc"]))
	}

	if err := store.Archive("s-arc"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	for _, pat := range []string{"*" + archivedTag + indexFileExt, "*" + archivedTag + ".0000.jsonl", "*" + archivedTag + ".0001.jsonl"} {
		m, _ := filepath.Glob(filepath.Join(dir, pat))
		if len(m) != 1 {
			t.Errorf("expected 1 archived file matching %q, got %d", pat, len(m))
		}
	}
	loaded, _ := store.ListActive()
	if len(loaded) != 0 {
		t.Errorf("expected archived session to be unlisted, got %d", len(loaded))
	}
}

// TestSessionStoreShardCache verifies the parsed-shard LRU evicts least-recently
// used entries and supports prefix invalidation.
func TestSessionStoreShardCache(t *testing.T) {
	c := newShardCache(2)
	recs := []shardRecord{{agentID: "root", msg: model.Message{Content: "x"}}}

	c.put("/d/a.0000.jsonl", recs)
	c.put("/d/a.0001.jsonl", recs)
	if _, ok := c.get("/d/a.0000.jsonl"); !ok { // touches a, making b the LRU
		t.Fatal("expected cache hit for a.0000")
	}
	c.put("/d/a.0002.jsonl", recs) // evicts b (LRU)
	if _, ok := c.get("/d/a.0001.jsonl"); ok {
		t.Error("expected b.0001 evicted as LRU")
	}
	if _, ok := c.get("/d/a.0000.jsonl"); !ok {
		t.Error("expected a.0000 still resident")
	}

	c.evictPrefix("/d/a")
	for _, k := range []string{"/d/a.0000.jsonl", "/d/a.0002.jsonl"} {
		if _, ok := c.get(k); ok {
			t.Errorf("expected %q evicted by prefix", k)
		}
	}
}

// TestSessionStoreSyncAndClose verifies the durability flusher can be driven
// synchronously and shut down without panicking, leaving data readable.
func TestSessionStoreSyncAndClose(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)

	us := buildSessionWithTranscript("s-sync", []model.Message{{Role: model.RoleUser, Content: "persist me"}})
	if err := store.Save(us, "Sync"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	store.Sync()
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After close, a fresh store still reads the persisted session.
	store2, _ := NewSessionStore(dir)
	defer store2.Close()
	loaded, _ := store2.ListActive()
	if len(loaded) != 1 || len(loaded[0].Transcripts["root"]) != 1 {
		t.Fatalf("data not readable after Sync+Close: %+v", loaded)
	}
}
