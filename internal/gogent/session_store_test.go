package gogent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
