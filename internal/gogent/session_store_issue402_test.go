package gogent

import (
	"os"
	"strings"
	"testing"

	"gogent/internal/model"
)

func TestSessionStoreRoundTripsReasoningIssue402(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	defer store.Close()

	us := buildSessionWithTranscript("s-reasoning", []model.Message{
		{Role: model.RoleUser, Content: "solve"},
		{Role: model.RoleAssistant, Reasoning: "retained thought"},
	})
	if err := store.Save(us, "Reasoning"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path := store.activeShardPath("s-reasoning")
	raw := mustReadFileIssue402(t, path)
	if !strings.Contains(raw, `"reasoning":"retained thought"`) {
		t.Fatalf("session shard omitted reasoning field:\n%s", raw)
	}

	reopened, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("reopen NewSessionStore: %v", err)
	}
	defer reopened.Close()

	loaded, err := reopened.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded sessions = %d, want 1", len(loaded))
	}
	root := loaded[0].Transcripts["root"]
	if len(root) != 2 {
		t.Fatalf("root transcript len = %d, want 2: %+v", len(root), root)
	}
	if root[1].Content != "" {
		t.Fatalf("assistant content = %q, want empty visible answer", root[1].Content)
	}
	if root[1].Reasoning != "retained thought" {
		t.Fatalf("assistant reasoning = %q, want retained thought", root[1].Reasoning)
	}
}

func mustReadFileIssue402(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
