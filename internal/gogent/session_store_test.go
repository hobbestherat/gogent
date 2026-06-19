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

func TestSessionStoreSaveListArchive(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}

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

	// Archiving removes it from the active listing and renames the file.
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
	matches, _ := filepath.Glob(filepath.Join(dir, "*"+archivedFileSuffix))
	if len(matches) != 1 {
		t.Fatalf("expected 1 archived file, got %d", len(matches))
	}
}

func TestSessionStoreAdoptContinues(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	us := buildSessionWithTranscript("session-1", []model.Message{{Role: model.RoleUser, Content: "a"}})
	if err := store.Save(us, "Session 1"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, _ := store.ListActive()
	if len(loaded) != 1 {
		t.Fatalf("expected 1 session")
	}
	// Adopt the existing file in a fresh store, append a message, and re-save:
	// the same file should be reused (still exactly one active session).
	store2, _ := NewSessionStore(dir)
	store2.Adopt("session-1", loaded[0].File)
	us.RootAgent.ThoughtTrain.AppendMessages(model.Message{Role: model.RoleAssistant, Content: "b"})
	if err := store2.Save(us, "Session 1"); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 file after adopt+save, got %d", len(files))
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

// TestEncodeTranscriptSurfacesErrors guards issue #17: previously each
// enc.Encode result was dropped with "_ =", so a failed encode vanished while
// Save reported success. Now the failure is aggregated and returned.
func TestEncodeTranscriptSurfacesErrors(t *testing.T) {
	us := buildSessionWithTranscript("session-x", []model.Message{
		{Role: model.RoleUser, Content: "hello"},
	})

	err := encodeTranscript(json.NewEncoder(failingWriter{}), us, "Session X", "2026-01-01T00:00:00Z")
	if err == nil {
		t.Fatal("expected an encode error, got nil (error was swallowed)")
	}
	if !strings.Contains(err.Error(), "encode session meta") {
		t.Errorf("expected the error to identify the failing record, got: %v", err)
	}
}

// TestEncodeTranscriptWritesRecords is the positive counterpart: a working sink
// receives the meta line plus one line per transcript message.
func TestEncodeTranscriptWritesRecords(t *testing.T) {
	us := buildSessionWithTranscript("session-x", []model.Message{
		{Role: model.RoleUser, Content: "hello"},
		{Role: model.RoleAssistant, Content: "hi"},
	})

	var buf bytes.Buffer
	if err := encodeTranscript(json.NewEncoder(&buf), us, "Session X", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("encodeTranscript: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 { // 1 meta + 2 messages
		t.Fatalf("expected 3 JSONL records, got %d (%q)", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], `"kind":"meta"`) || !strings.Contains(lines[0], "Session X") {
		t.Errorf("unexpected meta line: %s", lines[0])
	}
}

// countRecords parses the JSONL session file and returns the number of "meta"
// and "message" records on disk.
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
// save after new messages are appended extends the file with only the new lines
// (an append to the same inode) instead of rewriting the whole transcript. The
// file must contain exactly the expected messages with no duplication.
func TestSessionStoreSaveAppendsDelta(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	us := buildSessionWithTranscript("s-delta", []model.Message{
		{Role: model.RoleUser, Content: "one"},
		{Role: model.RoleAssistant, Content: "two"},
	})
	if err := store.Save(us, "Delta"); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	path := store.files["s-delta"]
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

	// The delta save appended to the existing file, so the inode is unchanged.
	// A full rewrite would have replaced it via temp + rename (a new inode).
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Errorf("expected delta save to append in place (same inode), got a replaced file")
	}

	msgs, meta := countRecords(t, path)
	if meta != 1 {
		t.Errorf("expected exactly 1 meta line, got %d", meta)
	}
	if msgs != 4 {
		t.Errorf("expected 4 message records (no duplication), got %d", msgs)
	}

	// The file must round-trip back to the full transcript.
	loaded, _ := store.ListActive()
	if len(loaded) != 1 || len(loaded[0].Transcripts["root"]) != 4 {
		t.Fatalf("expected 4 root messages after reload, got %+v", loaded)
	}
	if loaded[0].Transcripts["root"][3].Content != "four" {
		t.Errorf("last message not preserved: %+v", loaded[0].Transcripts["root"])
	}
}

// TestSessionStoreSaveIdempotent proves that re-saving with no transcript
// changes is a true no-op: the file is byte-identical and not even reopened for
// writing (the delta path appends nothing).
func TestSessionStoreSaveIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	us := buildSessionWithTranscript("s-noop", []model.Message{
		{Role: model.RoleUser, Content: "hello"},
	})
	if err := store.Save(us, "Noop"); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	path := store.files["s-noop"]
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
		t.Errorf("no-op save changed the file:\nbefore: %q\nafter:  %q", first, second)
	}
	stat2, _ := os.Stat(path)
	if !os.SameFile(stat1, stat2) {
		t.Errorf("no-op save replaced the file (expected no change at all)")
	}
}

// TestSessionStoreSaveCompactionRewrites verifies that when an agent's
// transcript is replaced in place (a compaction), Save falls back to a full
// atomic rewrite: the file ends up with only the compacted transcript, not a
// concatenation of the old and new ones, and the inode changes (temp + rename).
func TestSessionStoreSaveCompactionRewrites(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	us := buildSessionWithTranscript("s-compact", []model.Message{
		{Role: model.RoleUser, Content: "old-1"},
		{Role: model.RoleAssistant, Content: "old-2"},
		{Role: model.RoleUser, Content: "old-3"},
	})
	if err := store.Save(us, "Compact"); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	path := store.files["s-compact"]
	before, _ := os.Stat(path)

	// Simulate a compaction: the transcript is replaced with a shorter digest,
	// which advances the transcript epoch.
	us.RootAgent.ThoughtTrain.ReplaceTranscript([]model.Message{
		{Role: model.RoleUser, Content: "[summary]"},
	})
	if err := store.Save(us, "Compact"); err != nil {
		t.Fatalf("Save 2: %v", err)
	}

	// A compaction triggers a full rewrite (temp + rename), replacing the file.
	after, _ := os.Stat(path)
	if os.SameFile(before, after) {
		t.Errorf("expected compaction to rewrite the file (new inode), got same inode")
	}

	// The stale "old-*" messages must be gone; only the digest remains.
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "old-1") {
		t.Errorf("compacted file still contains stale old message: %s", b)
	}
	msgs, meta := countRecords(t, path)
	if meta != 1 || msgs != 1 {
		t.Errorf("expected 1 meta + 1 message after compaction, got meta=%d messages=%d", meta, msgs)
	}
	loaded, _ := store.ListActive()
	if len(loaded[0].Transcripts["root"]) != 1 || loaded[0].Transcripts["root"][0].Content != "[summary]" {
		t.Fatalf("expected reloaded transcript to be just the digest, got %+v", loaded[0].Transcripts["root"])
	}
}

// TestSessionStoreSaveTitleChangeRewrites verifies that a title change (held in
// the meta header) forces a full rewrite so the new title is persisted, even
// though no transcript messages changed.
func TestSessionStoreSaveTitleChangeRewrites(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
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
// file round-trips with both agents' transcripts intact.
func TestSessionStoreSaveNewSubAgentAppends(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	us := buildSessionWithTranscript("s-sub", []model.Message{
		{Role: model.RoleUser, Content: "root-q"},
	})
	if err := store.Save(us, "Sub"); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	path := store.files["s-sub"]
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

	// A new agent appends, so the root file keeps its inode.
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
// one line rather than rewriting the growing transcript. After N turns the file
// holds N message records total (O(n) bytes written across the session, not
// O(n^2)).
func TestSessionStoreSaveDeltaGrowsAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewSessionStore(dir)
	us := buildSessionWithTranscript("s-grow", []model.Message{
		{Role: model.RoleUser, Content: "turn-0"},
	})
	if err := store.Save(us, "Grow"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := store.files["s-grow"]

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
