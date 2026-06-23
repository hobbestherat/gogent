package gogent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/tool"
)

type asyncSpawnResult struct {
	Success bool `json:"success"`
	Mode    struct {
		OneShot     bool `json:"one_shot"`
		Interactive bool `json:"interactive"`
		Async       bool `json:"async"`
	} `json:"mode"`
	Tasks []struct {
		Name   string `json:"name"`
		Task   string `json:"task"`
		Handle string `json:"handle"`
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"tasks"`
}

func decodeAsyncSpawnResult(t *testing.T, resp *tool.ToolCallResponse) asyncSpawnResult {
	t.Helper()
	if resp == nil || !resp.Success {
		t.Fatalf("expected a successful async spawn response, got %+v", resp)
	}
	raw, _ := json.Marshal(resp.Result)
	var got asyncSpawnResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode async spawn result: %v\n%s", err, raw)
	}
	return got
}

func executeAsyncSpawn(t *testing.T, reg *tool.ToolRegistry, args map[string]interface{}) (*tool.ToolCallResponse, error) {
	t.Helper()
	return reg.ExecuteToolCall(&tool.ToolCall{Tool: "spawn_subagent", Args: args}, tool.ToolContext{
		SessionID: "session_test",
		AgentID:   "agent_test",
		Context:   context.Background(),
	})
}

func waitBackgroundEvent(t *testing.T, events <-chan agent.SessionEvent, want bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == agent.SessionEventBackground && ev.Background == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for background=%v event", want)
		}
	}
}

func TestSpawnSubagentAsyncReturnsPendingHandleWhileWorkerRuns(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	handler := func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(arrived) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
		immediateSuccess(w, r)
	}
	g, reg, cleanup := newBatchSession(t, handler, 1)
	defer cleanup()
	us := g.GetUserSession("session_test")
	events := make(chan agent.SessionEvent, 8)
	us.SetObserver(func(ev agent.SessionEvent) { events <- ev })

	done := make(chan struct {
		resp *tool.ToolCallResponse
		err  error
	}, 1)
	go func() {
		resp, err := executeAsyncSpawn(t, reg, map[string]interface{}{
			"async": true,
			"name":  "slow",
			"task":  "wait until released",
		})
		done <- struct {
			resp *tool.ToolCallResponse
			err  error
		}{resp: resp, err: err}
	}()

	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("background sub-agent never started its model request")
	}

	var result struct {
		resp *tool.ToolCallResponse
		err  error
	}
	select {
	case result = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("spawn_subagent async blocked while the background worker was still running")
	}
	if result.err != nil {
		t.Fatalf("async spawn returned error: %v", result.err)
	}
	got := decodeAsyncSpawnResult(t, result.resp)
	if !got.Mode.OneShot || got.Mode.Interactive || !got.Mode.Async {
		t.Fatalf("mode = %+v, want one_shot async non-interactive", got.Mode)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("tasks len = %d, want 1", len(got.Tasks))
	}
	if got.Tasks[0].Status != "running" || got.Tasks[0].Handle == "" {
		t.Fatalf("task = %+v, want running with a handle", got.Tasks[0])
	}
	if !us.HasBackgroundWork() {
		t.Fatal("session should report background work while the worker is blocked")
	}
	waitBackgroundEvent(t, events, true)

	close(release)
	waitBackgroundEvent(t, events, false)
	if us.HasBackgroundWork() {
		t.Fatal("session still reports background work after worker completion")
	}
}

func TestSpawnSubagentAsyncReinjectsCompletedResultOnNextRootTurn(t *testing.T) {
	childDone := make(chan struct{})
	var childDoneOnce sync.Once
	var mu sync.Mutex
	var requests [][]map[string]interface{}
	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []map[string]interface{} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		requests = append(requests, req.Messages)
		mu.Unlock()

		if requestContains(req.Messages, "background child task") {
			childDoneOnce.Do(func() { close(childDone) })
			writeModelFinal(w, "SUCCESS: child result ready")
			return
		}
		writeModelFinal(w, "root saw the background result")
	}
	g, reg, cleanup := newBatchSession(t, handler, 2)
	defer cleanup()
	us := g.GetUserSession("session_test")
	events := make(chan agent.SessionEvent, 8)
	us.SetObserver(func(ev agent.SessionEvent) { events <- ev })

	resp, err := executeAsyncSpawn(t, reg, map[string]interface{}{
		"async": true,
		"name":  "research",
		"task":  "background child task",
	})
	if err != nil {
		t.Fatalf("async spawn returned error: %v", err)
	}
	got := decodeAsyncSpawnResult(t, resp)
	if len(got.Tasks) != 1 || got.Tasks[0].Status != "running" {
		t.Fatalf("async spawn tasks = %+v", got.Tasks)
	}
	select {
	case <-childDone:
	case <-time.After(3 * time.Second):
		t.Fatal("child request did not complete")
	}
	waitBackgroundEvent(t, events, false)

	if _, err := us.ExecuteTaskLoop(context.Background(), "agent_test", "continue root work"); err != nil {
		t.Fatalf("root turn after background completion failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawReinjected bool
	for _, req := range requests {
		if requestContains(req, "[Background sub-agent \"research\" finished]") &&
			requestContains(req, "SUCCESS: child result ready") {
			sawReinjected = true
		}
	}
	if !sawReinjected {
		t.Fatalf("next root turn did not include re-injected background result; requests=%v", requests)
	}
}

func TestSpawnSubagentAsyncLimiterSlotReleasedAfterSessionStop(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	handler := func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
			immediateSuccess(w, r)
		case <-r.Context().Done():
		}
	}
	g, reg, cleanup := newBatchSession(t, handler, 1)
	defer cleanup()
	us := g.GetUserSession("session_test")
	events := make(chan agent.SessionEvent, 8)
	us.SetObserver(func(ev agent.SessionEvent) { events <- ev })

	resp, err := executeAsyncSpawn(t, reg, map[string]interface{}{"async": true, "name": "blocked", "task": "hold slot"})
	if err != nil {
		t.Fatalf("first async spawn returned error: %v", err)
	}
	first := decodeAsyncSpawnResult(t, resp)
	if len(first.Tasks) != 1 || first.Tasks[0].Status != "running" {
		t.Fatalf("first async spawn tasks = %+v", first.Tasks)
	}
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("first background worker never reached the model request")
	}

	resp, err = executeAsyncSpawn(t, reg, map[string]interface{}{"async": true, "name": "rejected", "task": "no slot"})
	if err != nil {
		t.Fatalf("second async spawn should return a per-task error, got top-level error: %v", err)
	}
	rejected := decodeAsyncSpawnResult(t, resp)
	if len(rejected.Tasks) != 1 || rejected.Tasks[0].Status != "error" || !strings.Contains(rejected.Tasks[0].Error, "concurrency limit") {
		t.Fatalf("second async spawn task = %+v, want concurrency-limit per-task error", rejected.Tasks)
	}

	us.Stop()
	waitBackgroundEvent(t, events, false)

	resp, err = executeAsyncSpawn(t, reg, map[string]interface{}{"async": true, "name": "after-stop", "task": "slot reused"})
	if err != nil {
		t.Fatalf("third async spawn returned error after Stop released the slot: %v", err)
	}
	third := decodeAsyncSpawnResult(t, resp)
	if len(third.Tasks) != 1 || third.Tasks[0].Status != "running" {
		t.Fatalf("third async spawn tasks = %+v, want running after limiter release", third.Tasks)
	}
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("third background worker never reached the model request")
	}
	close(release)
	waitBackgroundEvent(t, events, false)
}

func requestContains(messages []map[string]interface{}, needle string) bool {
	for _, m := range messages {
		if content, ok := m["content"].(string); ok && strings.Contains(content, needle) {
			return true
		}
	}
	return false
}

func writeModelFinal(w http.ResponseWriter, content string) {
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

func TestSpawnSubagentAsyncInvalidItemsReturnPerTaskErrors(t *testing.T) {
	_, reg, cleanup := newBatchSession(t, immediateSuccess, 2)
	defer cleanup()

	resp, err := executeAsyncSpawn(t, reg, map[string]interface{}{
		"async": true,
		"subtasks": []interface{}{
			float64(7),
			map[string]interface{}{"name": "empty", "task": "   "},
		},
	})
	if err != nil {
		t.Fatalf("async invalid-item batch should not return a top-level error: %v", err)
	}
	got := decodeAsyncSpawnResult(t, resp)
	if len(got.Tasks) != 2 {
		t.Fatalf("tasks len = %d, want 2", len(got.Tasks))
	}
	if got.Tasks[0].Status != "error" || got.Tasks[0].Error != "invalid subtask item" {
		t.Fatalf("task[0] = %+v, want invalid item error", got.Tasks[0])
	}
	if got.Tasks[1].Status != "error" || got.Tasks[1].Error != "missing subtask.task" {
		t.Fatalf("task[1] = %+v, want missing task error", got.Tasks[1])
	}
}
