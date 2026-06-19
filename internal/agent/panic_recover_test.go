package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogent/internal/tool"
)

// panickingTool is a tool whose Execute always panics, used to prove the task
// loop contains tool/parser panics to a single call (issue #8) instead of
// crashing the process.
func panickingTool(name, msg string) *tool.Tool {
	return &tool.Tool{
		Name:        name,
		Description: "always panics",
		InputSchema: map[string]interface{}{"type": "object"},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			panic(msg)
		},
	}
}

// toolCallIDs builds one OpenAI-style assistant turn that requests several
// native tool calls, so a single model response carries multiple calls.
func toolCallIDs(calls ...[2]string) map[string]interface{} {
	toolCalls := make([]map[string]interface{}, 0, len(calls))
	for _, c := range calls {
		toolCalls = append(toolCalls, map[string]interface{}{
			"id":   c[0],
			"type": "function",
			"function": map[string]interface{}{
				"name":      c[1],
				"arguments": `{}`,
			},
		})
	}
	return map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role":       "assistant",
				"tool_calls": toolCalls,
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
}

// TestExecuteTaskLoopSurvivesPanickingTool verifies the sequential tool path: a
// tool that panics is contained (issue #8) and reported back to the model as an
// error, so the loop keeps going and produces a final answer.
func TestExecuteTaskLoopSurvivesPanickingTool(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("call_1", "bomb", `{}`),
		finalResponse("recovered from the tool error"),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, ag := newLoopSession(t, server.URL)
	ag.ToolRegistry.Register(panickingTool("bomb", "kaboom"))

	responses, err := us.ExecuteTaskLoop("root", "use the bomb tool")
	if err != nil {
		t.Fatalf("loop must survive a panicking tool, got error: %v", err)
	}
	if len(responses) != 2 {
		t.Fatalf("expected 2 responses (tool turn + final), got %d", len(responses))
	}

	// The contained panic must be fed back to the model as the tool result.
	second := fs.requests[1]
	found := false
	for _, m := range second {
		if c, _ := m["content"].(string); strings.Contains(c, "panicked") && strings.Contains(c, "kaboom") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the panic to surface as a tool result; second request=%+v", second)
	}
}

// TestExecuteTaskLoopSurvivesPanickingParallelSpawns verifies the parallel
// sub-agent tool path (allSpawnSubAgent): two spawn_subagent calls in one turn
// run concurrently, and when they panic each is contained to its own entry — no
// deadlock, each result reports the panic, and the loop continues to a final
// answer. This guards the WaitGroup accounting under panic (issue #8).
func TestExecuteTaskLoopSurvivesPanickingParallelSpawns(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallIDs([2]string{"c1", "spawn_subagent"}, [2]string{"c2", "spawn_subagent"}),
		finalResponse("both spawns were contained"),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, ag := newLoopSession(t, server.URL)
	ag.ToolRegistry.Register(panickingTool("spawn_subagent", "parallel kaboom"))

	responses, err := us.ExecuteTaskLoop("root", "spawn two sub-agents")
	if err != nil {
		t.Fatalf("parallel branch must survive panicking spawns, got error: %v", err)
	}
	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(responses))
	}

	// Both parallel entries must surface their contained panic as tool results.
	second := fs.requests[1]
	panicResults := 0
	for _, m := range second {
		if m["role"] != "tool" {
			continue
		}
		if c, _ := m["content"].(string); strings.Contains(c, "panicked") {
			panicResults++
		}
	}
	if panicResults != 2 {
		t.Errorf("expected 2 contained panic tool results, got %d; second request=%+v", panicResults, second)
	}
}
