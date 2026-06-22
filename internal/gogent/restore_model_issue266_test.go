package gogent

import (
	"testing"

	"gogent/internal/config"
	"gogent/internal/model"
)

// Issue #266: a restored session must resume on the model it was last using, not
// the configured default. The model name is persisted in the index; these tests
// cover the read-back (LoadedSession.Model) and the apply (adoptLoaded points the
// session at that model and reports it via PrimaryModel).

func TestLoadSessionReadsBackModel(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	defer store.Close()

	us := buildSessionWithTranscript("s-266-readback", []model.Message{{Role: model.RoleUser, Content: "hi"}})
	us.SetPrimaryModel("alt") // the non-default model the session was using
	if err := store.Save(us, "S"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	metas, err := store.ListSessions()
	if err != nil || len(metas) != 1 {
		t.Fatalf("ListSessions: %v (n=%d)", err, len(metas))
	}
	ls, err := store.LoadSession(metas[0].File)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if ls.Model != "alt" {
		t.Fatalf("LoadedSession.Model = %q, want %q", ls.Model, "alt")
	}
}

func newRestoreGogent(t *testing.T) *Gogent {
	t.Helper()
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	g.config = &config.Config{
		DefaultModel: "main",
		ModelConfigs: []*config.ModelConfig{
			{Name: "main", Model: "m1", APIType: "openai"},
			{Name: "alt", Model: "m2", APIType: "openai"},
		},
	}
	return g
}

func TestAdoptLoadedAppliesRecordedModel(t *testing.T) {
	g := newRestoreGogent(t)
	ls := LoadedSession{
		ID:          "s-266-apply",
		Title:       "S",
		Model:       "alt", // not the default
		Transcripts: map[string][]model.Message{"root": {{Role: model.RoleUser, Content: "hi"}}},
	}
	adopted, ok := g.adoptLoaded(ls)
	if !ok {
		t.Fatalf("adoptLoaded returned ok=false")
	}
	if adopted.Model != "alt" {
		t.Fatalf("adopted.Model = %q, want %q", adopted.Model, "alt")
	}
	us := g.GetUserSession("s-266-apply")
	if us == nil {
		t.Fatalf("no user session created")
	}
	if got := us.PrimaryModel(); got != "alt" {
		t.Fatalf("PrimaryModel() = %q, want %q (restored model, not default)", got, "alt")
	}
}

func TestAdoptLoadedUnknownOrEmptyModelKeepsDefault(t *testing.T) {
	for _, tc := range []struct{ name, recorded string }{
		{"unknown model removed from config", "ghost"},
		{"older session with no recorded model", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRestoreGogent(t)
			ls := LoadedSession{
				ID:          "s-266-fallback",
				Model:       tc.recorded,
				Transcripts: map[string][]model.Message{"root": {{Role: model.RoleUser, Content: "hi"}}},
			}
			if _, ok := g.adoptLoaded(ls); !ok {
				t.Fatalf("adoptLoaded returned ok=false")
			}
			us := g.GetUserSession("s-266-fallback")
			if us == nil {
				t.Fatalf("no user session created")
			}
			// PrimaryModel is only stamped when the recorded model resolves; an
			// unknown/empty model falls back to the default connection and leaves
			// PrimaryModel unset (its prior behaviour), rather than mislabelling.
			if got := us.PrimaryModel(); got != "" {
				t.Fatalf("PrimaryModel() = %q, want empty for %s", got, tc.name)
			}
		})
	}
}
