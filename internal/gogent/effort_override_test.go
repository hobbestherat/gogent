package gogent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gogent/internal/config"
)

// captureServer records the JSON body of the last chat-completions request and
// always returns a terminal assistant message so the task loop ends in one round.
type captureServer struct {
	mu   sync.Mutex
	body map[string]interface{}
}

func (s *captureServer) handler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var parsed map[string]interface{}
	_ = json.Unmarshal(raw, &parsed)
	s.mu.Lock()
	s.body = parsed
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(finalChatResponse("done"))
}

func (s *captureServer) reasoningEffort() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.body["reasoning_effort"]
	if !ok {
		return "", false
	}
	str, _ := v.(string)
	return str, true
}

// newEffortGogent wires a Gogent whose single model points at endpoint, with the
// given api_type and a config-default reasoning_effort, so the override can be
// tested against it.
func newEffortGogent(t *testing.T, endpoint, apiType, defaultEffort string) *Gogent {
	t.Helper()
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	g.config = &config.Config{
		DefaultModel: "test",
		ModelConfigs: []*config.ModelConfig{{
			Name:            "test",
			Model:           "glm-5.2",
			APIType:         apiType,
			Endpoint:        endpoint,
			ReasoningEffort: defaultEffort,
		}},
	}
	return g
}

// TestEffortOverrideAppliedToRequest checks a chosen per-session effort overrides
// the model config's reasoning_effort in the request for an effort-capable
// provider (issue #177).
func TestEffortOverrideAppliedToRequest(t *testing.T) {
	srv := &captureServer{}
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	// zai supports reasoning_effort; config default is "high".
	g := newEffortGogent(t, server.URL+"/v1/chat/completions", "zai", "high")
	g.NewSession("s1")

	if _, err := g.SendMessageToSessionWithModelAndEffort(
		context.Background(), "s1", "root", "hello", "test", "max"); err != nil {
		t.Fatalf("send: %v", err)
	}

	got, ok := srv.reasoningEffort()
	if !ok {
		t.Fatal("request omitted reasoning_effort, want \"max\"")
	}
	if got != "max" {
		t.Errorf("reasoning_effort = %q, want \"max\" (override of config default)", got)
	}
}

// TestEffortOverrideFallsBackToConfig checks an empty override leaves the model
// config's configured reasoning_effort in place (issue #177).
func TestEffortOverrideFallsBackToConfig(t *testing.T) {
	srv := &captureServer{}
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	g := newEffortGogent(t, server.URL+"/v1/chat/completions", "zai", "high")
	g.NewSession("s1")

	if _, err := g.SendMessageToSessionWithModelAndEffort(
		context.Background(), "s1", "root", "hello", "test", ""); err != nil {
		t.Fatalf("send: %v", err)
	}

	got, ok := srv.reasoningEffort()
	if !ok || got != "high" {
		t.Errorf("reasoning_effort = %q (present=%v), want \"high\" from config default", got, ok)
	}
}

// TestEffortOverrideDroppedForUnsupportedProvider checks the request never carries
// reasoning_effort for a provider without supportsReasoningEffort, even when an
// override is chosen — the provider gate drops it (issue #177).
func TestEffortOverrideDroppedForUnsupportedProvider(t *testing.T) {
	srv := &captureServer{}
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	// openrouter has supportsReasoningEffort == false.
	g := newEffortGogent(t, server.URL+"/api/v1", "openrouter", "")
	g.NewSession("s1")

	if _, err := g.SendMessageToSessionWithModelAndEffort(
		context.Background(), "s1", "root", "hello", "test", "high"); err != nil {
		t.Fatalf("send: %v", err)
	}

	if got, ok := srv.reasoningEffort(); ok {
		t.Errorf("reasoning_effort = %q present, want absent for a provider without effort support", got)
	}
}
