package gogent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"gogent/internal/config"
	"gogent/internal/model"
)

// Issue #532 — GOAL 4 (the direct symptom). New/restored sessions must never
// inherit an unroutable default: defaultConnection skips an unroutable default to a
// routable survivor (or the clear-error fail-safe), the SendMessage default fallback
// redirects only a MATCHED-but-unroutable default (leaving an unmatched default to
// keep the session's existing connection), and a restored session whose recorded
// model name was dropped cannot resolve into a live unroutable connection.

// TestDefaultConnection_UnroutableDefault_FallsToRoutable: a new session whose
// configured default is unroutable must never inherit it — defaultConnection skips
// to a routable entry (issue #532, goal 4).
func TestDefaultConnection_UnroutableDefault_FallsToRoutable(t *testing.T) {
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	g.config = &config.Config{
		DefaultModel: "bad",
		ModelConfigs: []*config.ModelConfig{
			badEntry("bad"),
			{Name: "good", APIType: "openai", Model: "good-model", Endpoint: "https://good.example.com/v1"},
		},
	}
	conn := g.defaultConnection()
	if conn.ModelName != "good-model" {
		t.Errorf("defaultConnection resolved to %q, want the routable good-model (not the unroutable default)", conn.ModelName)
	}
	// And it is a real connection (endpoint-derived URL), not the fail-safe placeholder.
	if conn.URL == "" || conn.URL == model.DefaultModelURL {
		t.Errorf("defaultConnection returned a placeholder URL %q for a routable model", conn.URL)
	}
}

// TestDefaultConnection_AllRoutable_UsesNamedDefault: when the configured default is
// routable, defaultConnection must keep the existing default-by-name precedence (no
// regression from adding the routability guard).
func TestDefaultConnection_AllRoutable_UsesNamedDefault(t *testing.T) {
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	g.config = &config.Config{
		DefaultModel: "second",
		ModelConfigs: []*config.ModelConfig{
			{Name: "first", APIType: "openai", Model: "first-model", Endpoint: "https://first.example.com/v1"},
			{Name: "second", APIType: "openai", Model: "second-model", Endpoint: "https://second.example.com/v1"},
		},
	}
	conn := g.defaultConnection()
	if conn.ModelName != "second-model" {
		t.Errorf("defaultConnection resolved to %q, want the named default second-model", conn.ModelName)
	}
}

// TestSendMessage_NarrowedGuard_MatchedUnroutableDefault_Redirects is defense-in-depth
// (issue #532). With an unroutable DefaultModel injected directly into memory
// (bypassing the load sweep), a send with an EMPTY model name must redirect the turn
// to the first routable entry rather than routing through the bad default. The turn
// must reach the routable model's endpoint, not localhost.
func TestSendMessage_NarrowedGuard_MatchedUnroutableDefault_Redirects(t *testing.T) {
	srv := &captureServer{}
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	g.config = &config.Config{
		DefaultModel: "bad",
		ModelConfigs: []*config.ModelConfig{
			badEntry("bad"), // the configured default, but unroutable
			{Name: "good", APIType: "openai", Model: "good-model", Endpoint: server.URL + "/v1/chat/completions"},
		},
	}
	g.NewSession("s1")

	if _, err := g.SendMessageToSessionWithModelAndEffort(
		context.Background(), "s1", "root", "hi", "", ""); err != nil {
		t.Fatalf("send with an unroutable default should redirect to the routable entry, got: %v", err)
	}
	// The turn reached the routable model's endpoint (not localhost / the bad default).
	if srv.body == nil {
		t.Fatal("the turn was not routed to the routable model; the capture server saw no request")
	}
	// The turn's primary model reflects the routable fallback the session actually used.
	if us := g.GetUserSession("s1"); us != nil && us.PrimaryModel() != "good" {
		t.Errorf("PrimaryModel = %q, want good (the routable fallback)", us.PrimaryModel())
	}
}

// TestSendMessage_NarrowedGuard_UnmatchedDefault_KeepsExisting is the regression guard
// for the narrowing (Defect 3.1): an empty/unmatched DefaultModel must leave the
// session on its existing connection — the guard must not fire and re-resolve to a
// different model. The turn must still succeed over the session's existing connection.
func TestSendMessage_NarrowedGuard_UnmatchedDefault_KeepsExisting(t *testing.T) {
	srv := &captureServer{}
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	g.config = &config.Config{
		DefaultModel: "", // unmatched — the narrowed guard must NOT fire
		ModelConfigs: []*config.ModelConfig{
			{Name: "good", APIType: "openai", Model: "good-model", Endpoint: server.URL + "/v1/chat/completions"},
		},
	}
	g.NewSession("s1") // connection resolves to the only (routable) model

	if _, err := g.SendMessageToSessionWithModelAndEffort(
		context.Background(), "s1", "root", "hi", "", ""); err != nil {
		t.Fatalf("send with an unmatched default should keep the existing connection, got: %v", err)
	}
	if srv.body == nil {
		t.Fatal("the turn should have used the session's existing routable connection")
	}
}

// TestRestore_DroppedModelName_NotResolvable: combined with the load sweep, a
// restored session whose recorded model name was dropped as unroutable must not
// resolve into a live unroutable connection — it falls back to the (routable)
// default. (issue #532, goal 4 restore path.)
func TestRestore_DroppedModelName_NotResolvable(t *testing.T) {
	home := t.TempDir()
	seedConfigOnDisk(t, home, &config.Config{
		DefaultModel: "good",
		ModelConfigs: []*config.ModelConfig{badEntry("dropped"), goodEntry("good")},
	})
	g := NewGogent(home) // sweep drops "dropped"

	// The dropped name is gone from memory, so it cannot be resolved into a connection.
	if g.config.GetModelConfig("dropped") != nil {
		t.Fatal("the dropped unroutable entry must not be resolvable by name after the sweep")
	}
	// Restoring a session that last ran on the dropped model falls back safely (no panic,
	// no live unroutable connection): adoptLoaded cannot resolve "dropped", so it uses the
	// default connection.
	ls := LoadedSession{
		ID:    "s-restore-532",
		Title: "R",
		Model: "dropped",
		Transcripts: map[string][]model.Message{"root": {
			{Role: model.RoleUser, Content: "hi"},
		}},
	}
	if _, ok := g.adoptLoaded(ls); !ok {
		t.Fatalf("adoptLoaded returned ok=false for a session whose model was dropped")
	}
	if us := g.GetUserSession("s-restore-532"); us == nil {
		t.Fatal("the restored session was not created")
	}
}
