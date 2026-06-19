package gogent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/model"
	"gogent/internal/tool"
)

// fanoutServer answers every model request with the same one-shot final answer,
// so a spawned sub-agent finishes in a single round trip. It records how many
// requests were in flight at once so the test can prove the concurrency cap.
type fanoutServer struct {
	inFlight int64
	peak     int64
}

func (f *fanoutServer) handler(w http.ResponseWriter, r *http.Request) {
	cur := atomic.AddInt64(&f.inFlight, 1)
	for {
		p := atomic.LoadInt64(&f.peak)
		if cur <= p || atomic.CompareAndSwapInt64(&f.peak, p, cur) {
			break
		}
	}
	defer atomic.AddInt64(&f.inFlight, -1)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       map[string]interface{}{"role": "assistant", "content": "SUCCESS: done"},
			"finish_reason": "stop",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	})
}

// TestSpawnSubAgentBatchBounded drives the real spawn_subagent batch tool through
// the tool registry with a limiter of one slot and asserts that (a) every
// subtask in the batch produces a result and (b) the global concurrency limiter
// kept the fan-out from running more than one sub-agent at a time (issue #23).
func TestSpawnSubAgentBatchBounded(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "gogent-fanout-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)
	workspace := filepath.Join(tempDir, ".gogent", "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("workspace: %v", err)
	}

	fs := &fanoutServer{}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	g := NewGogentWithWorkspace(tempDir, workspace)

	// Root agent whose model backend points at the fake server; spawned children
	// share this backend.
	conn := model.NewModelConnection()
	conn.SetURL(server.URL)
	sess := model.NewModelSession("session_test", conn)
	root := agent.NewAgent("agent_test", sess)
	us := g.CreateUserSession("session_test", root)

	// Force a single global slot so the batch must serialize through the limiter,
	// exercising the inline-backpressure path end to end.
	us.SetSubAgentLimiter(agent.NewSubAgentLimiter(1))

	subtasks := []interface{}{
		map[string]interface{}{"name": "a", "task": "do a"},
		map[string]interface{}{"name": "b", "task": "do b"},
		map[string]interface{}{"name": "c", "task": "do c"},
	}
	call := &tool.ToolCall{
		Tool: "spawn_subagent",
		Args: map[string]interface{}{"subtasks": subtasks},
	}
	resp, err := g.GetToolRegistry().ExecuteToolCall(call, tool.ToolContext{
		SessionID: "session_test",
		AgentID:   "agent_test",
		Context:   context.Background(),
	})
	if err != nil {
		t.Fatalf("spawn_subagent batch errored: %v", err)
	}
	if resp == nil || !resp.Success {
		t.Fatalf("expected a successful batch response, got %+v", resp)
	}

	// Inspect the per-subtask outcomes via JSON (the result struct is unexported).
	raw, _ := json.Marshal(resp.Result)
	var decoded struct {
		Results []struct {
			Name   string `json:"name"`
			Result string `json:"result"`
			Error  string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode batch result: %v", err)
	}
	if len(decoded.Results) != len(subtasks) {
		t.Fatalf("expected %d results, got %d", len(subtasks), len(decoded.Results))
	}
	for _, r := range decoded.Results {
		if r.Error != "" {
			t.Errorf("subtask %q errored: %s", r.Name, r.Error)
		}
		if !strings.Contains(r.Result, "SUCCESS") {
			t.Errorf("subtask %q missing SUCCESS result, got %q", r.Name, r.Result)
		}
	}

	// With a single slot, at most one sub-agent runs in a spawned goroutine; the
	// overflow runs inline in the caller, so peak in-flight is bounded by
	// limit+1. Without the limiter all three would run concurrently (peak 3).
	if fs.peak > 2 {
		t.Fatalf("limiter of 1 should bound sub-agent fan-out to <=2 in flight, but peak was %d", fs.peak)
	}
}
