package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gogent/internal/model"
	"gogent/internal/tool"
)

// planFakeServer serves canned responses in sequence and records every request's
// advertised tools and messages, so plan-mode tests can assert which tools were
// offered and what system prompt was sent.
type planFakeServer struct {
	mu        sync.Mutex
	responses []map[string]interface{}
	calls     int
	tools     [][]map[string]interface{} // per request: advertised tool function names + args
	messages  [][]map[string]interface{}
}

func (f *planFakeServer) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []map[string]interface{} `json:"messages"`
		Tools    []map[string]interface{} `json:"tools"`
	}
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	idx := f.calls
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	f.calls++
	toolNames := make([]map[string]interface{}, 0, len(req.Tools))
	for _, t := range req.Tools {
		if fn, ok := t["function"].(map[string]interface{}); ok {
			toolNames = append(toolNames, map[string]interface{}{
				"name":      fn["name"],
				"arguments": fn["arguments"],
			})
		}
	}
	f.tools = append(f.tools, toolNames)
	f.messages = append(f.messages, req.Messages)
	resp := f.responses[idx]
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// toolNamesFromRequest pulls the advertised function names out of a recorded
// request's tool list.
func toolNamesFromRequest(t *testing.T, names []map[string]interface{}) []string {
	t.Helper()
	out := make([]string, 0, len(names))
	for _, n := range names {
		if name, ok := n["name"].(string); ok {
			out = append(out, name)
		}
	}
	return out
}

// newPlanSession builds a session whose agent owns a registry with a read-only
// "read" tool and a side-effecting "write" tool, pointed at a fake server.
func newPlanSession(t *testing.T, url string) (*UserSession, *Agent) {
	t.Helper()
	conn := newTestModelConnection()
	conn.SetURL(url)
	sess := model.NewModelSession("test", conn)

	reg := tool.NewToolRegistry()
	reg.Register(&tool.Tool{
		Name: "read", ReadOnly: true, Description: "d", InputSchema: nil,
		Execute: func(map[string]interface{}, tool.ToolContext) (interface{}, error) { return "ok", nil },
	})
	reg.Register(&tool.Tool{
		Name: "write", Description: "d", InputSchema: nil,
		Execute: func(map[string]interface{}, tool.ToolContext) (interface{}, error) { return "ok", nil },
	})

	ag := NewAgent("root", sess)
	ag.SetToolRegistry(reg)
	return NewUserSession("s1", ag), ag
}

func finalOnly(content string) map[string]interface{} {
	return map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       map[string]interface{}{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	}
}

// TestPlanModeRestrictsAdvertisedTools asserts that in plan mode the loop
// advertises only read-only tools to the model — the side-effecting "write" tool
// is stripped (issue #43).
func TestPlanModeRestrictsAdvertisedTools(t *testing.T) {
	fs := &planFakeServer{responses: []map[string]interface{}{finalOnly("a plan")}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newPlanSession(t, server.URL)
	us.SetPlanMode(true)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "plan a refactor"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.tools) == 0 {
		t.Fatal("no requests recorded")
	}
	advertised := toolNamesFromRequest(t, fs.tools[0])
	has := func(n string) bool {
		for _, got := range advertised {
			if got == n {
				return true
			}
		}
		return false
	}
	if has("write") {
		t.Errorf("plan mode advertised write tool; tools = %v (write must be stripped)", advertised)
	}
	if !has("read") {
		t.Errorf("plan mode did not advertise read tool; tools = %v", advertised)
	}
}

// TestPlanModeRecordsPlanAndEmits asserts a plan-mode turn records the final
// answer as the pending plan and emits SessionEventPlan (issue #43).
func TestPlanModeRecordsPlanAndEmits(t *testing.T) {
	fs := &planFakeServer{responses: []map[string]interface{}{finalOnly("Step 1: read\nStep 2: edit")}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newPlanSession(t, server.URL)
	us.SetPlanMode(true)

	var mu sync.Mutex
	var planEvent *SessionEvent
	us.SetObserver(func(ev SessionEvent) {
		mu.Lock()
		defer mu.Unlock()
		if ev.Type == SessionEventPlan {
			ec := ev
			planEvent = &ec
		}
	})

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "plan a refactor"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	if got := us.PendingPlan(); !strings.Contains(got, "Step 1") {
		t.Errorf("PendingPlan = %q, want the recorded plan", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if planEvent == nil || !strings.Contains(planEvent.Plan, "Step 1") {
		t.Errorf("expected a SessionEventPlan carrying the plan, got %+v", planEvent)
	}
}

// TestPlanModeSystemPrompt asserts the plan-mode system prompt is sent to the
// model (issue #43).
func TestPlanModeSystemPrompt(t *testing.T) {
	fs := &planFakeServer{responses: []map[string]interface{}{finalOnly("plan")}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newPlanSession(t, server.URL)
	us.SetPlanMode(true)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "plan it"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.messages) == 0 {
		t.Fatal("no requests recorded")
	}
	var systemText string
	for _, m := range fs.messages[0] {
		if role, _ := m["role"].(string); role == "system" {
			systemText, _ = m["content"].(string)
		}
	}
	if !strings.Contains(systemText, "PLAN MODE") {
		t.Errorf("system prompt in plan mode = %q, want it to mention PLAN MODE", systemText)
	}
}

// TestPlanModeOffKeepsAllTools asserts that with plan mode off the full tool set
// (including the side-effecting write) is advertised, so plan mode is opt-in.
func TestPlanModeOffKeepsAllTools(t *testing.T) {
	fs := &planFakeServer{responses: []map[string]interface{}{finalOnly("done")}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newPlanSession(t, server.URL)
	// plan mode stays off (default).

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "do it"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	advertised := toolNamesFromRequest(t, fs.tools[0])
	has := func(n string) bool {
		for _, got := range advertised {
			if got == n {
				return true
			}
		}
		return false
	}
	if !has("write") || !has("read") {
		t.Errorf("with plan mode off, expected read+write advertised; got %v", advertised)
	}
}

// TestSetPlanModeClearsPendingPlan asserts leaving plan mode drops the plan
// awaiting approval (it is no longer actionable), while ClearPendingPlan drops
// only the plan (issue #43).
func TestSetPlanModeClearsPendingPlan(t *testing.T) {
	us := newTestSession("s1")
	us.SetPlanMode(true)
	if !us.setPendingPlan("a plan") {
		t.Fatal("expected plan to be recorded")
	}
	if us.PendingPlan() == "" {
		t.Fatal("expected a pending plan")
	}
	us.SetPlanMode(false)
	if us.PendingPlan() != "" {
		t.Errorf("leaving plan mode should clear the pending plan, got %q", us.PendingPlan())
	}
}
