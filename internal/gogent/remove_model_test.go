package gogent

import (
	"errors"
	"strings"
	"testing"

	"gogent/internal/config"
)

// Issue #509: RemoveModel deletes a configured model and enforces the removal
// policy identically for the embedded and remote (HTTP DELETE) backends. These
// exercise every branch of the policy (unknown / in-use / default-while-others /
// last-allowed) plus persistence and the sentinel-error contract the server maps
// to HTTP status. They mirror the AddModel/SetDefaultModel test discipline
// (NewGogent(dir) + a hand-built g.config).

// removeTestGogent builds a core over a temp dir with the given models, the first
// of which is the default. It returns the core for direct probing.
func removeTestGogent(t *testing.T, models ...*config.ModelConfig) *Gogent {
	t.Helper()
	dir := t.TempDir()
	g := NewGogent(dir)
	def := ""
	if len(models) > 0 {
		def = models[0].Name
	}
	g.config = &config.Config{DefaultModel: def, ModelConfigs: models}
	return g
}

func TestRemoveModelRemovesNonDefaultAndPersists(t *testing.T) {
	g := removeTestGogent(t,
		&config.ModelConfig{Name: "main", Model: "m1"},
		&config.ModelConfig{Name: "alt", Model: "m2"},
	)

	if err := g.RemoveModel("alt"); err != nil {
		t.Fatalf("RemoveModel(alt): %v", err)
	}
	// Gone from the live list...
	if hasModel(g, "alt") {
		t.Fatal("alt still present after RemoveModel")
	}
	if !hasModel(g, "main") {
		t.Fatal("main was removed too (RemoveModel touched the wrong entry)")
	}
	// ...and the default is untouched.
	if got := g.DefaultModelName(); got != "main" {
		t.Fatalf("DefaultModelName = %q, want main (non-default removal must not change the default)", got)
	}
	// ...and it persisted to disk.
	loaded, err := config.LoadConfig(g.homeDir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(loaded.ModelConfigs) != 1 || loaded.ModelConfigs[0].Name != "main" {
		t.Fatalf("persisted models = %+v, want only [main]", loaded.ModelConfigs)
	}
}

func TestRemoveModelUnknownReturnsNotFound(t *testing.T) {
	g := removeTestGogent(t, &config.ModelConfig{Name: "main", Model: "m1"})

	err := g.RemoveModel("ghost")
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("RemoveModel(ghost) err = %v, want ErrModelNotFound", err)
	}
	if !hasModel(g, "main") {
		t.Fatal("a not-found removal must leave the model set unchanged")
	}
}

func TestRemoveModelBlocksDefaultWhileOthersRemain(t *testing.T) {
	g := removeTestGogent(t,
		&config.ModelConfig{Name: "main", Model: "m1"},
		&config.ModelConfig{Name: "alt", Model: "m2"},
	)

	err := g.RemoveModel("main")
	if !errors.Is(err, ErrModelIsDefault) {
		t.Fatalf("RemoveModel(default) err = %v, want ErrModelIsDefault", err)
	}
	// Blocked: nothing changed.
	if !hasModel(g, "main") || !hasModel(g, "alt") {
		t.Fatal("a blocked default removal must leave both models present")
	}
	if got := g.DefaultModelName(); got != "main" {
		t.Fatalf("default changed to %q on a blocked removal; want main", got)
	}
}

func TestRemoveModelAllowsLastClearsDefault(t *testing.T) {
	// The last/only model is ALLOWED to be removed even though it is the default;
	// DefaultModel is cleared, yielding the empty-list state.
	g := removeTestGogent(t, &config.ModelConfig{Name: "main", Model: "m1"})

	if err := g.RemoveModel("main"); err != nil {
		t.Fatalf("RemoveModel(last default) = %v, want nil (last model is allowed even if default)", err)
	}
	if len(g.Models()) != 0 {
		t.Fatalf("models after removing the last = %v, want empty", g.Models())
	}
	if got := g.DefaultModelName(); got != "" {
		t.Fatalf("DefaultModelName after removing the last = %q, want empty", got)
	}
	// And the cleared default persisted across a reload.
	loaded, err := config.LoadConfig(g.homeDir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.DefaultModel != "" || len(loaded.ModelConfigs) != 0 {
		t.Fatalf("persisted state = %+v, want empty models + empty default", loaded)
	}
}

// TestRemoveModelDefaultThenLastIsAllowed walks the full lifecycle: with two
// models the default is blocked, but once only the default remains it is allowed.
func TestRemoveModelDefaultThenLastIsAllowed(t *testing.T) {
	g := removeTestGogent(t,
		&config.ModelConfig{Name: "main", Model: "m1"},
		&config.ModelConfig{Name: "alt", Model: "m2"},
	)
	if err := g.RemoveModel("alt"); err != nil {
		t.Fatalf("RemoveModel(alt): %v", err)
	}
	// Now "main" is the last model AND the default — allowed.
	if err := g.RemoveModel("main"); err != nil {
		t.Fatalf("RemoveModel(last default) = %v, want nil", err)
	}
	if len(g.Models()) != 0 {
		t.Fatalf("models = %v, want empty", g.Models())
	}
}

func TestRemoveModelInUseBlocked(t *testing.T) {
	g := removeTestGogent(t,
		&config.ModelConfig{Name: "main", Model: "m1"},
		&config.ModelConfig{Name: "alt", Model: "m2"},
	)
	// A live session that has routed a turn through "alt" (not the default, so the
	// block is attributable to in-use, not to being the default).
	us := g.NewSession("sess-1")
	us.SetPrimaryModel("alt")

	err := g.RemoveModel("alt")
	if !errors.Is(err, ErrModelInUse) {
		t.Fatalf("RemoveModel(in-use) err = %v, want ErrModelInUse", err)
	}
	// The blocking session id is named in the message so the UI can tell the user
	// which session to close.
	if err == nil || !strings.Contains(err.Error(), "sess-1") {
		t.Fatalf("in-use error %v should name the blocking session id sess-1", err)
	}
	if !hasModel(g, "alt") {
		t.Fatal("an in-use model must not be deleted")
	}
}

func TestRemoveModelIdleSessionDoesNotBlock(t *testing.T) {
	// D2 boundary (documented in the design + RemoveModel doc comment): a session
	// that has NOT routed a turn reports PrimaryModel()=="" and is deliberately NOT
	// counted as in-use. So removing a non-default model that a never-sent session
	// merely defaulted to succeeds. This test locks that decision.
	g := removeTestGogent(t,
		&config.ModelConfig{Name: "main", Model: "m1"},
		&config.ModelConfig{Name: "alt", Model: "m2"},
	)
	idle := g.NewSession("idle-1")
	if pm := idle.PrimaryModel(); pm != "" {
		t.Fatalf("a fresh session PrimaryModel = %q, want empty (no turn routed)", pm)
	}

	if err := g.RemoveModel("alt"); err != nil {
		t.Fatalf("RemoveModel(alt) with only an idle session = %v, want nil", err)
	}
	if hasModel(g, "alt") {
		t.Fatal("an idle (never-sent) session must not block removing a non-default model")
	}
}

// TestRemoveModelInUsePrecedesLast confirms the policy ordering: in-use is checked
// before allow-last, so the ONLY model cannot be removed while a session is mid-use
// on it (the caller must let that session finish/close first).
func TestRemoveModelInUsePrecedesLast(t *testing.T) {
	g := removeTestGogent(t, &config.ModelConfig{Name: "main", Model: "m1"})
	us := g.NewSession("sess-1")
	us.SetPrimaryModel("main")

	err := g.RemoveModel("main")
	if !errors.Is(err, ErrModelInUse) {
		t.Fatalf("RemoveModel(last, in-use) err = %v, want ErrModelInUse (in-use precedes allow-last)", err)
	}
	if !hasModel(g, "main") {
		t.Fatal("the in-use last model must not be removed")
	}
}

func TestRemoveModelSentinelsAreWrapped(t *testing.T) {
	// The server maps status via errors.Is, so each sentinel must wrap (not just
	// string-match) and be distinguishable.
	g := removeTestGogent(t,
		&config.ModelConfig{Name: "main", Model: "m1"},
		&config.ModelConfig{Name: "alt", Model: "m2"},
	)
	if err := g.RemoveModel("ghost"); !errors.Is(err, ErrModelNotFound) || errors.Is(err, ErrModelIsDefault) {
		t.Errorf("unknown: err=%v must be ONLY ErrModelNotFound", err)
	}
	if err := g.RemoveModel("main"); !errors.Is(err, ErrModelIsDefault) || errors.Is(err, ErrModelNotFound) {
		t.Errorf("default: err=%v must be ONLY ErrModelIsDefault", err)
	}
}

// hasModel reports whether a model with the given Name is configured.
func hasModel(g *Gogent, name string) bool {
	for _, m := range g.Models() {
		if m.Name == name {
			return true
		}
	}
	return false
}

// TestRemoveModelEmptyNameReturnsNotFound locks the fixes-round-1 guard: an empty
// name matches no configured model and must be rejected up front as not-found.
// Without the guard, the in-use scan below would compare PrimaryModel()=="" ==
// name and spuriously block on every idle session.
func TestRemoveModelEmptyNameReturnsNotFound(t *testing.T) {
	g := removeTestGogent(t,
		&config.ModelConfig{Name: "main", Model: "m1"},
		&config.ModelConfig{Name: "alt", Model: "m2"},
	)
	err := g.RemoveModel("")
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("RemoveModel(\"\") err = %v, want ErrModelNotFound", err)
	}
	if errors.Is(err, ErrModelInUse) {
		t.Fatalf("RemoveModel(\"\") must not be reported as in-use (the empty-name/idle-session aliasing bug): %v", err)
	}
	// Nothing removed.
	if len(g.Models()) != 2 {
		t.Fatalf("models after RemoveModel(\"\") = %v, want both untouched", g.Models())
	}
}

// TestRemoveModelEmptyNameDoesNotAliasIdleSession is the precise regression for the
// bug: an idle session reports PrimaryModel()=="", so before the guard
// RemoveModel("") returned ErrModelInUse whenever any idle session existed.
func TestRemoveModelEmptyNameDoesNotAliasIdleSession(t *testing.T) {
	g := removeTestGogent(t,
		&config.ModelConfig{Name: "main", Model: "m1"},
		&config.ModelConfig{Name: "alt", Model: "m2"},
	)
	g.NewSession("idle") // never sends -> PrimaryModel()==""

	err := g.RemoveModel("")
	if !errors.Is(err, ErrModelNotFound) || errors.Is(err, ErrModelInUse) {
		t.Fatalf("RemoveModel(\"\") with an idle session = %v; want ErrModelNotFound only "+
			"(the idle session's empty PrimaryModel must not be treated as in-use)", err)
	}
}

// TestRemoveModelInUseReportsASession: with several sessions mid-use on the model,
// the block names one of them (map iteration order is non-deterministic, so assert
// it names SOME live session id, not which one).
func TestRemoveModelInUseReportsASession(t *testing.T) {
	g := removeTestGogent(t,
		&config.ModelConfig{Name: "main", Model: "m1"},
		&config.ModelConfig{Name: "alt", Model: "m2"},
	)
	for _, id := range []string{"sess-a", "sess-b", "sess-c"} {
		g.NewSession(id).SetPrimaryModel("alt")
	}

	err := g.RemoveModel("alt")
	if !errors.Is(err, ErrModelInUse) {
		t.Fatalf("RemoveModel(in-use by 3) err = %v, want ErrModelInUse", err)
	}
	named := false
	for _, id := range []string{"sess-a", "sess-b", "sess-c"} {
		if strings.Contains(err.Error(), id) {
			named = true
		}
	}
	if !named {
		t.Errorf("in-use error %v should name one of the blocking sessions", err)
	}
	if !hasModel(g, "alt") {
		t.Error("the in-use model must not have been removed")
	}
}

// TestRemoveModelStaleDefaultDoesNotBlockNonDefault: a DefaultModel that points at
// a nonexistent model (stale) must not interfere with removing a real, non-default
// model. The block only applies when the removed model IS the default.
func TestRemoveModelStaleDefaultDoesNotBlockNonDefault(t *testing.T) {
	g := removeTestGogent(t,
		&config.ModelConfig{Name: "main", Model: "m1"},
		&config.ModelConfig{Name: "alt", Model: "m2"},
	)
	g.config.DefaultModel = "ghost" // stale: references no configured model

	if err := g.RemoveModel("alt"); err != nil {
		t.Fatalf("RemoveModel(alt) with a stale default = %v, want nil", err)
	}
	if hasModel(g, "alt") {
		t.Fatal("alt should have been removed")
	}
	if got := g.DefaultModelName(); got != "ghost" {
		t.Fatalf("DefaultModel changed to %q; a non-default removal must not touch it", got)
	}
}
