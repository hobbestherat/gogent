package gogent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gogent/internal/model"
)

func TestIssue389SessionStorePersistsFrozenModelSnapshot(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	defer store.Close()

	label, providerID := "Original Label", "provider-original"
	store.SetModelSnapshotProvider(func(name string) (string, string, bool) {
		if name != "primary" {
			return "", "", false
		}
		return label, providerID, true
	})
	us := buildSessionWithTranscript("issue389", []model.Message{{Role: model.RoleUser, Content: "hello"}})
	us.SetPrimaryModel("primary")

	if err := store.Save(us, "Issue 389"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	metas, err := store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("ListSessions returned %d sessions, want 1", len(metas))
	}
	if got := metas[0].Model; got != "primary" {
		t.Fatalf("Model = %q, want primary", got)
	}
	if got := metas[0].ModelLabel; got != "Original Label" {
		t.Fatalf("ModelLabel = %q, want Original Label", got)
	}
	if got := metas[0].ModelID; got != "provider-original" {
		t.Fatalf("ModelID = %q, want provider-original", got)
	}

	raw, err := os.ReadFile(metas[0].File)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", metas[0].File, err)
	}
	var idx sessionIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("unmarshal index: %v", err)
	}
	if idx.ModelLabel != "Original Label" || idx.ModelID != "provider-original" {
		t.Fatalf("index model snapshot = (%q,%q), want (Original Label,provider-original)", idx.ModelLabel, idx.ModelID)
	}

	label, providerID = "Renamed Later", "provider-renamed"
	metas, err = store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions after provider rename: %v", err)
	}
	if got := metas[0].ModelLabel; got != "Original Label" {
		t.Fatalf("unsaved provider rename changed persisted ModelLabel to %q, want Original Label", got)
	}

	loaded, err := store.LoadSession(metas[0].File)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if loaded.Model != "primary" || loaded.ModelLabel != "Original Label" || loaded.ModelID != "provider-original" {
		t.Fatalf("LoadedSession model fields = (%q,%q,%q), want (primary,Original Label,provider-original)",
			loaded.Model, loaded.ModelLabel, loaded.ModelID)
	}

	us.RootAgent.ThoughtTrain.AppendMessages(model.Message{Role: model.RoleAssistant, Content: "saved after rename"})
	if err := store.Save(us, "Issue 389 after rename"); err != nil {
		t.Fatalf("Save after rename: %v", err)
	}
	metas, err = store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions after saved rename: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("ListSessions after saved rename returned %d sessions, want 1", len(metas))
	}
	if got := metas[0].ModelLabel; got != "Renamed Later" {
		t.Fatalf("saved-after-rename ModelLabel = %q, want Renamed Later", got)
	}
	if got := metas[0].ModelID; got != "provider-renamed" {
		t.Fatalf("saved-after-rename ModelID = %q, want provider-renamed", got)
	}
}

func TestIssue389SessionStoreLeavesSnapshotEmptyWhenModelCannotResolve(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	defer store.Close()

	store.SetModelSnapshotProvider(func(string) (string, string, bool) { return "", "", false })
	us := buildSessionWithTranscript("issue389-missing", []model.Message{{Role: model.RoleUser, Content: "hello"}})
	us.SetPrimaryModel("missing")

	if err := store.Save(us, "Missing Model"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	metas, err := store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("ListSessions returned %d sessions, want 1", len(metas))
	}
	if metas[0].Model != "missing" || metas[0].ModelLabel != "" || metas[0].ModelID != "" {
		t.Fatalf("model fields = (%q,%q,%q), want (missing,\"\",\"\")", metas[0].Model, metas[0].ModelLabel, metas[0].ModelID)
	}
}

func TestIssue389OlderIndexWithoutSnapshotFallsBackToModelKey(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "2026-06-24T00-00-00_legacy_session")
	idx := sessionIndex{
		SessionID: "legacy",
		Title:     "Legacy",
		CreatedAt: "2026-06-24T00:00:00Z",
		Model:     "stable-key",
		Shards:    []shardMeta{},
	}
	raw, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(base+indexFileExt, raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	defer store.Close()
	metas, err := store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("ListSessions returned %d sessions, want 1", len(metas))
	}
	if metas[0].Model != "stable-key" || metas[0].ModelLabel != "" || metas[0].ModelID != "" {
		t.Fatalf("legacy model fields = (%q,%q,%q), want (stable-key,\"\",\"\")", metas[0].Model, metas[0].ModelLabel, metas[0].ModelID)
	}
}
