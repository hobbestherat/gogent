package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gogent/internal/model"
	"gogent/internal/tool"
)

// multiToolCallResponse builds an OpenAI-style assistant turn carrying several
// native tool_calls at once. Each call is {id, name, argsJSON}.
func multiToolCallResponse(calls ...[3]string) map[string]interface{} {
	tcs := make([]map[string]interface{}, 0, len(calls))
	for _, c := range calls {
		tcs = append(tcs, map[string]interface{}{
			"id":       c[0],
			"type":     "function",
			"function": map[string]interface{}{"name": c[1], "arguments": c[2]},
		})
	}
	return map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       map[string]interface{}{"role": "assistant", "tool_calls": tcs},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
}

// TestAllReadOnly covers the predicate that decides whether a turn's tool batch
// is eligible for the parallel read-only fast-path.
func TestAllReadOnly(t *testing.T) {
	reg := tool.NewToolRegistry()
	reg.Register(&tool.Tool{Name: "read", ReadOnly: true, Execute: func(map[string]interface{}, tool.ToolContext) (interface{}, error) { return "", nil }})
	reg.Register(&tool.Tool{Name: "grep", ReadOnly: true, Execute: func(map[string]interface{}, tool.ToolContext) (interface{}, error) { return "", nil }})
	reg.Register(&tool.Tool{Name: "write", Execute: func(map[string]interface{}, tool.ToolContext) (interface{}, error) { return "", nil }})

	call := func(name string) tool.ToolCall { return tool.ToolCall{Tool: name} }

	tests := []struct {
		name  string
		calls []tool.ToolCall
		want  bool
	}{
		{"two read-only", []tool.ToolCall{call("read"), call("grep")}, true},
		{"single call left to serial path", []tool.ToolCall{call("read")}, false},
		{"mixed read-only and write", []tool.ToolCall{call("read"), call("write")}, false},
		{"all writes", []tool.ToolCall{call("write"), call("write")}, false},
		{"unknown tool (e.g. MCP)", []tool.ToolCall{call("read"), call("mystery")}, false},
		{"empty", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := allReadOnly(reg, tc.calls); got != tc.want {
				t.Errorf("allReadOnly(%v) = %v, want %v", tc.calls, got, tc.want)
			}
		})
	}

	if allReadOnly(nil, []tool.ToolCall{call("read"), call("grep")}) {
		t.Error("allReadOnly(nil registry, ...) must be false")
	}
}

// TestRunBoundedToolsRunsAll verifies the fixed-semaphore runner executes every
// task exactly once and never exceeds the concurrency cap.
func TestRunBoundedToolsRunsAll(t *testing.T) {
	const n = maxParallelToolCalls * 3

	var current, peak, completed int64
	var peakMu sync.Mutex

	tasks := make([]func(), n)
	for i := 0; i < n; i++ {
		tasks[i] = func() {
			cur := atomic.AddInt64(&current, 1)
			peakMu.Lock()
			if cur > peak {
				peak = cur
			}
			peakMu.Unlock()
			time.Sleep(5 * time.Millisecond) // let concurrent tasks overlap
			atomic.AddInt64(&current, -1)
			atomic.AddInt64(&completed, 1)
		}
	}

	runBoundedTools(tasks)

	if completed != n {
		t.Fatalf("expected all %d tasks to run, got %d", n, completed)
	}
	if peak > maxParallelToolCalls {
		t.Fatalf("peak concurrency %d exceeded the cap of %d", peak, maxParallelToolCalls)
	}
	if peak < 2 {
		t.Fatalf("expected real parallelism (peak >= 2), got %d", peak)
	}
}

// TestExecuteTaskLoopParallelReadOnly drives the full loop with a turn that
// returns three independent read-only tool calls. It asserts they ran
// concurrently (peak overlap > 1) and that their result messages are fed back in
// call order regardless of which call finished first.
func TestExecuteTaskLoopParallelReadOnly(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		multiToolCallResponse(
			[3]string{"call_a", "slow_read", `{"id":"A","delay_ms":60}`},
			[3]string{"call_b", "slow_read", `{"id":"B","delay_ms":10}`},
			[3]string{"call_c", "slow_read", `{"id":"C","delay_ms":30}`},
		),
		finalResponse("done"),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	conn := model.NewModelConnection()
	conn.SetURL(server.URL)
	sess := model.NewModelSession("test", conn)

	var current, peak int64
	var peakMu sync.Mutex
	reg := tool.NewToolRegistry()
	reg.Register(&tool.Tool{
		Name:     "slow_read",
		ReadOnly: true,
		Execute: func(args map[string]interface{}, _ tool.ToolContext) (interface{}, error) {
			cur := atomic.AddInt64(&current, 1)
			peakMu.Lock()
			if cur > peak {
				peak = cur
			}
			peakMu.Unlock()
			delay, _ := args["delay_ms"].(float64)
			time.Sleep(time.Duration(delay) * time.Millisecond)
			atomic.AddInt64(&current, -1)
			return args["id"], nil
		},
	})

	ag := NewAgent("root", sess)
	ag.SetToolRegistry(reg)
	us := NewUserSession("s1", ag)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "read A, B and C"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	if peak < 2 {
		t.Fatalf("expected the read-only batch to run in parallel (peak >= 2), got %d", peak)
	}

	// The second request must carry the three tool results in call order
	// (A, B, C), proving results are reassembled by call index rather than by
	// whichever call finished first (B finishes well before A here).
	if len(fs.requests) < 2 {
		t.Fatalf("expected at least 2 model requests, got %d", len(fs.requests))
	}
	var got []string
	for _, m := range fs.requests[1] {
		if m["role"] == "tool" {
			content, _ := m["content"].(string)
			got = append(got, content)
		}
	}
	want := []string{"A", "B", "C"}
	if len(got) != len(want) {
		t.Fatalf("expected %d tool results, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tool results out of call order: got %v, want %v", got, want)
		}
	}
}

// TestExecuteTaskLoopMixedBatchSerial verifies a batch that is not all read-only
// (a read plus a write) falls back to the serial path: the calls still all run,
// in order, and never overlap.
func TestExecuteTaskLoopMixedBatchSerial(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		multiToolCallResponse(
			[3]string{"call_a", "slow_read", `{"id":"A","delay_ms":30}`},
			[3]string{"call_b", "slow_write", `{"id":"B","delay_ms":30}`},
		),
		finalResponse("done"),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	conn := model.NewModelConnection()
	conn.SetURL(server.URL)
	sess := model.NewModelSession("test", conn)

	var current, peak int64
	var peakMu sync.Mutex
	track := func(args map[string]interface{}, _ tool.ToolContext) (interface{}, error) {
		cur := atomic.AddInt64(&current, 1)
		peakMu.Lock()
		if cur > peak {
			peak = cur
		}
		peakMu.Unlock()
		delay, _ := args["delay_ms"].(float64)
		time.Sleep(time.Duration(delay) * time.Millisecond)
		atomic.AddInt64(&current, -1)
		return args["id"], nil
	}
	reg := tool.NewToolRegistry()
	reg.Register(&tool.Tool{Name: "slow_read", ReadOnly: true, Execute: track})
	reg.Register(&tool.Tool{Name: "slow_write", Execute: track}) // side-effecting

	ag := NewAgent("root", sess)
	ag.SetToolRegistry(reg)
	us := NewUserSession("s1", ag)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "read A then write B"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	if peak != 1 {
		t.Fatalf("expected a mixed (non read-only) batch to run serially (peak == 1), got %d", peak)
	}
}
