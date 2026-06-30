package gogent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gogent/internal/config"
)

// planModelServer serves canned final responses in sequence and records the user
// messages of each request, so the plan/approve flow can be asserted.
type planModelServer struct {
	mu        sync.Mutex
	responses []string // final answer contents, served in order
	calls     int
	messages  [][]map[string]interface{}
}

func (s *planModelServer) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)

	s.mu.Lock()
	idx := s.calls
	if idx >= len(s.responses) {
		idx = len(s.responses) - 1
	}
	content := s.responses[idx]
	s.calls++
	s.messages = append(s.messages, req.Messages)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       map[string]interface{}{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	})
}

// userMessages returns the most recent request's user-role message contents.
func (s *planModelServer) lastUserMessages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) == 0 {
		return nil
	}
	var out []string
	for _, m := range s.messages[len(s.messages)-1] {
		if role, _ := m["role"].(string); role == "user" {
			if c, _ := m["content"].(string); c != "" {
				out = append(out, c)
			}
		}
	}
	return out
}

// newPlanGogent builds a Gogent whose default model points at a fake server, with
// persistence disabled so the test touches no disk beyond the temp home.
func newPlanGogent(t *testing.T, url string) *Gogent {
	t.Helper()
	g := NewGogent(t.TempDir())
	g.store = nil
	g.config = &config.Config{
		DefaultModel: "m",
		Connections: []*config.ProviderConnection{
			{Name: "m-conn", APIType: "openai", Endpoint: url},
		},
		ModelConfigs: []*config.ModelConfig{
			{Name: "m", Model: "m", Connection: "m-conn", Caps: config.ModelCapabilities{ContextWindow: 8192}},
		},
		SubAgents: config.DefaultSubAgentConfig(),
	}
	return g
}

// TestExecuteApprovedPlan verifies the plan-mode gate end to end (issue #43): a
// plan-mode turn records a plan, and executing the approved plan re-runs the turn
// with the full tool set carrying the plan text, leaving plan mode.
func TestExecuteApprovedPlan(t *testing.T) {
	const plan = "PLAN:\n1. read foo\n2. edit foo"
	srv := &planModelServer{responses: []string{plan, "DONE: implemented"}}
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	g := newPlanGogent(t, server.URL)
	id := "plan-session"
	g.NewSession(id)

	// Plan turn: plan mode on, the model proposes a plan.
	g.SetPlanMode(id, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := g.SendMessageToSessionWithModel(ctx, id, "root", "plan a change to foo", "m"); err != nil {
		t.Fatalf("plan turn failed: %v", err)
	}
	if !g.HasPendingPlan(id) {
		t.Fatal("expected a pending plan after the plan turn")
	}
	if got := g.GetUserSession(id).PendingPlan(); !strings.Contains(got, "read foo") {
		t.Errorf("pending plan = %q, want it to contain the plan", got)
	}

	// Approve: execute the plan with full tools, carrying the plan text.
	if _, err := g.ExecuteApprovedPlan(ctx, id, "root"); err != nil {
		t.Fatalf("ExecuteApprovedPlan failed: %v", err)
	}
	if g.PlanMode(id) {
		t.Error("plan mode should be off after approving the plan")
	}
	if g.HasPendingPlan(id) {
		t.Error("pending plan should be cleared after approving the plan")
	}

	// The approve turn's request must carry the approved plan text to the model.
	joined := strings.Join(srv.lastUserMessages(), "\n")
	if !strings.Contains(joined, "read foo") || !strings.Contains(joined, "approved") {
		t.Errorf("approve turn user message = %q, want it to carry the approved plan", joined)
	}
}

// TestExecuteApprovedPlanNoPlan verifies approving without a pending plan fails
// cleanly rather than sending an empty turn (issue #43).
func TestExecuteApprovedPlanNoPlan(t *testing.T) {
	srv := &planModelServer{responses: []string{"x"}}
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	g := newPlanGogent(t, server.URL)
	id := "plan-session-2"
	g.NewSession(id)

	if _, err := g.ExecuteApprovedPlan(context.Background(), id, "root"); err == nil {
		t.Error("expected an error when approving with no pending plan")
	}
}
