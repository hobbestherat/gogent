package gogent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/model"
	"gogent/internal/tool"
)

// Issue #282, E2: the spawn_subagent batch path must tolerate the weak-model
// "subtasks":["do x","do y"] bare-string shape and fan it out concurrently, and
// must report per-subtask errors by index without sinking the whole batch.

// newBatchSession spins up a fake model backend and a user session wired to it,
// returning the Gogent, the registry and a cleanup func. The handler is supplied
// by the caller so each test can choose immediate vs barrier responses.
func newBatchSession(t *testing.T, handler http.HandlerFunc, limit int) (*Gogent, *tool.ToolRegistry, func()) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "gogent-282-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	workspace := filepath.Join(tempDir, ".gogent", "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	server := httptest.NewServer(handler)

	g := NewGogentWithWorkspace(tempDir, workspace)
	conn := model.NewModelConnection()
	conn.SetURL(server.URL)
	sess := model.NewModelSession("session_test", conn)
	root := agent.NewAgent("agent_test", sess)
	us := g.CreateUserSession("session_test", root)
	us.SetSubAgentLimiter(agent.NewSubAgentLimiter(limit))

	cleanup := func() {
		server.Close()
		os.RemoveAll(tempDir)
	}
	return g, g.GetToolRegistry(), cleanup
}

// immediateSuccess answers every model request with a one-shot SUCCESS so a
// spawned sub-agent finishes in a single round trip.
func immediateSuccess(w http.ResponseWriter, r *http.Request) {
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

func runBatch(t *testing.T, reg *tool.ToolRegistry, subtasks []interface{}) *tool.ToolCallResponse {
	t.Helper()
	call := &tool.ToolCall{Tool: "spawn_subagent", Args: map[string]interface{}{"subtasks": subtasks}}
	resp, err := reg.ExecuteToolCall(call, tool.ToolContext{
		SessionID: "session_test",
		AgentID:   "agent_test",
		Context:   context.Background(),
	})
	if err != nil {
		// Per-subtask failures live inside resp; a top-level err is unexpected here.
		t.Fatalf("spawn_subagent batch returned top-level error: %v", err)
	}
	return resp
}

type subtaskResult struct {
	Name   string `json:"name"`
	Task   string `json:"task"`
	Result string `json:"result"`
	Error  string `json:"error"`
}

func decodeResults(t *testing.T, resp *tool.ToolCallResponse) []subtaskResult {
	t.Helper()
	if resp == nil || !resp.Success {
		t.Fatalf("expected a successful batch response, got %+v", resp)
	}
	raw, _ := json.Marshal(resp.Result)
	var decoded struct {
		Results []subtaskResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode batch result: %v", err)
	}
	return decoded.Results
}

// The weak-model bare-string batch must fan out and complete: every entry runs
// as a sub-agent and returns a SUCCESS result, with an empty name.
func TestSpawnSubtasksBareStringTolerated(t *testing.T) {
	_, reg, cleanup := newBatchSession(t, immediateSuccess, 4)
	defer cleanup()

	results := decodeResults(t, runBatch(t, reg, []interface{}{"summarise README", "list tests"}))
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for i, r := range results {
		if r.Error != "" {
			t.Errorf("subtask %d (bare string) errored: %s", i, r.Error)
		}
		if !strings.Contains(r.Result, "SUCCESS") {
			t.Errorf("subtask %d missing SUCCESS result, got %q", i, r.Result)
		}
	}
	// Bare strings become the task with no name.
	if results[0].Task != "summarise README" {
		t.Errorf("subtask 0 task = %q, want it to carry the bare string", results[0].Task)
	}
	if results[0].Name != "" {
		t.Errorf("subtask 0 name = %q, want empty for a bare-string subtask", results[0].Name)
	}
}

// Mixed/invalid items are reported per-index and never sink the valid siblings.
func TestSpawnSubtasksMixedItems(t *testing.T) {
	_, reg, cleanup := newBatchSession(t, immediateSuccess, 4)
	defer cleanup()

	subtasks := []interface{}{
		map[string]interface{}{"name": "ok", "task": "do work"}, // 0: valid map
		float64(123),                             // 1: invalid item type
		map[string]interface{}{"name": "noTask"}, // 2: missing task
		"bare task ok",                           // 3: valid bare string
		"   ",                                    // 4: whitespace-only -> missing task
	}
	results := decodeResults(t, runBatch(t, reg, subtasks))
	if len(results) != len(subtasks) {
		t.Fatalf("expected %d results (index-aligned), got %d", len(subtasks), len(results))
	}

	if results[0].Error != "" || !strings.Contains(results[0].Result, "SUCCESS") {
		t.Errorf("result[0] valid map: got error=%q result=%q", results[0].Error, results[0].Result)
	}
	if results[1].Error != "invalid subtask item" {
		t.Errorf("result[1] = %q, want %q", results[1].Error, "invalid subtask item")
	}
	if results[2].Error != "missing subtask.task" {
		t.Errorf("result[2] = %q, want %q", results[2].Error, "missing subtask.task")
	}
	if results[3].Error != "" || !strings.Contains(results[3].Result, "SUCCESS") {
		t.Errorf("result[3] valid bare string: got error=%q result=%q", results[3].Error, results[3].Result)
	}
	if results[4].Error != "missing subtask.task" {
		t.Errorf("result[4] (whitespace) = %q, want %q", results[4].Error, "missing subtask.task")
	}
}

// An empty subtasks array is a successful no-op batch (no sub-agents spawned).
func TestSpawnSubtasksEmptyArray(t *testing.T) {
	_, reg, cleanup := newBatchSession(t, immediateSuccess, 4)
	defer cleanup()
	results := decodeResults(t, runBatch(t, reg, []interface{}{}))
	if len(results) != 0 {
		t.Errorf("empty batch should yield no results, got %d", len(results))
	}
}

// A non-array subtasks value is rejected by schema validation before reaching
// the fan-out, surfaced as an unsuccessful response (not a panic).
func TestSpawnSubtasksNotArray(t *testing.T) {
	_, reg, cleanup := newBatchSession(t, immediateSuccess, 4)
	defer cleanup()
	call := &tool.ToolCall{Tool: "spawn_subagent", Args: map[string]interface{}{"subtasks": "not an array"}}
	resp, err := reg.ExecuteToolCall(call, tool.ToolContext{
		SessionID: "session_test", AgentID: "agent_test", Context: context.Background(),
	})
	if err == nil && (resp == nil || resp.Success) {
		t.Fatalf("non-array subtasks should not succeed, got resp=%+v err=%v", resp, err)
	}
	msg := ""
	if resp != nil {
		msg = resp.Error
	}
	if err != nil {
		msg = err.Error()
	}
	if !strings.Contains(msg, "array") {
		t.Errorf("error %q should mention the array type problem", msg)
	}
}

// Single-task mode (no subtasks) still requires a non-empty task.
func TestSpawnSingleTaskRequiresTask(t *testing.T) {
	_, reg, cleanup := newBatchSession(t, immediateSuccess, 4)
	defer cleanup()
	call := &tool.ToolCall{Tool: "spawn_subagent", Args: map[string]interface{}{"name": "x"}}
	resp, err := reg.ExecuteToolCall(call, tool.ToolContext{
		SessionID: "session_test", AgentID: "agent_test", Context: context.Background(),
	})
	if err == nil && (resp == nil || resp.Success) {
		t.Fatalf("single-task spawn with no task should fail, got resp=%+v err=%v", resp, err)
	}
}

// Single-task mode succeeds for a well-formed lone task.
func TestSpawnSingleTaskSucceeds(t *testing.T) {
	_, reg, cleanup := newBatchSession(t, immediateSuccess, 4)
	defer cleanup()
	call := &tool.ToolCall{Tool: "spawn_subagent", Args: map[string]interface{}{"name": "solo", "task": "do the thing"}}
	resp, err := reg.ExecuteToolCall(call, tool.ToolContext{
		SessionID: "session_test", AgentID: "agent_test", Context: context.Background(),
	})
	if err != nil {
		t.Fatalf("single-task spawn errored: %v", err)
	}
	if resp == nil || !resp.Success {
		t.Fatalf("expected success, got %+v", resp)
	}
	raw, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(raw), "SUCCESS") {
		t.Errorf("single-task result missing SUCCESS, got %s", raw)
	}
}

// barrierServer only responds once `target` model requests are simultaneously in
// flight. If the batch ran serially, the target is never reached and every
// request times out — which the test asserts must NOT happen. This proves the
// bare-string (weak-model) batch genuinely fans out concurrently (issue #282).
type barrierServer struct {
	target   int32
	arrived  int32
	release  chan struct{}
	once     sync.Once
	timedOut int32
}

func (b *barrierServer) handler(w http.ResponseWriter, r *http.Request) {
	if atomic.AddInt32(&b.arrived, 1) >= b.target {
		b.once.Do(func() { close(b.release) })
	}
	select {
	case <-b.release:
	case <-time.After(5 * time.Second):
		atomic.StoreInt32(&b.timedOut, 1)
	}
	immediateSuccess(w, r)
}

func TestSpawnSubtasksConcurrentFanout(t *testing.T) {
	bs := &barrierServer{target: 3, release: make(chan struct{})}
	_, reg, cleanup := newBatchSession(t, bs.handler, 8)
	defer cleanup()

	// Weak-model bare-string batch of 3 independent tasks.
	results := decodeResults(t, runBatch(t, reg, []interface{}{"task a", "task b", "task c"}))
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if atomic.LoadInt32(&bs.timedOut) != 0 {
		t.Fatal("sub-agents did not overlap: the 3-way concurrency barrier was never satisfied, so the batch ran serially")
	}
	for i, r := range results {
		if r.Error != "" {
			t.Errorf("subtask %d errored: %s", i, r.Error)
		}
	}
}
