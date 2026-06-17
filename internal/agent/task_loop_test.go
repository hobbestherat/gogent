package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogent/internal/model"
	"gogent/internal/tool"
)

// fakeServer serves canned OpenAI-style responses in sequence and records the
// requests it received so the test can assert the conversation that was sent.
type fakeServer struct {
	responses []map[string]interface{}
	requests  [][]map[string]interface{} // captured "messages" arrays
	calls     int
}

func (f *fakeServer) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []map[string]interface{} `json:"messages"`
		Tools    []interface{}            `json:"tools"`
	}
	_ = json.Unmarshal(body, &req)
	f.requests = append(f.requests, req.Messages)

	idx := f.calls
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	f.calls++

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f.responses[idx])
}

func toolCallResponse(id, name, args string) map[string]interface{} {
	return map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role": "assistant",
				"tool_calls": []map[string]interface{}{{
					"id":   id,
					"type": "function",
					"function": map[string]interface{}{
						"name":      name,
						"arguments": args,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
}

func finalResponse(content string) map[string]interface{} {
	return map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       map[string]interface{}{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 20, "completion_tokens": 8, "total_tokens": 28},
	}
}

func newLoopSession(t *testing.T, url string) (*UserSession, *Agent) {
	t.Helper()
	conn := model.NewModelConnection()
	conn.SetURL(url)
	sess := model.NewModelSession("test", conn)

	reg := tool.NewToolRegistry()
	reg.RegisterCalcTool()

	ag := NewAgent("root", sess)
	ag.SetToolRegistry(reg)
	us := NewUserSession("s1", ag)
	return us, ag
}

// TestExecuteTaskLoopNativeToolCall verifies the full ReAct loop: the model asks
// for a tool, the loop executes it, feeds the result back, and the model answers.
func TestExecuteTaskLoopNativeToolCall(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("call_1", "calc", `{"expression":"2+2"}`),
		finalResponse("The answer is 4."),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, ag := newLoopSession(t, server.URL)

	responses, err := us.ExecuteTaskLoop("root", "what is 2+2?")
	if err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	if len(responses) != 2 {
		t.Fatalf("expected 2 model responses, got %d", len(responses))
	}
	if got := responses[len(responses)-1].Content; !strings.Contains(got, "4") {
		t.Errorf("final answer should mention 4, got %q", got)
	}

	// The second request must carry the assistant tool_call AND the tool result,
	// proving the transcript now includes prior turns (the original bug).
	if len(fs.requests) != 2 {
		t.Fatalf("expected 2 requests to the model, got %d", len(fs.requests))
	}
	second := fs.requests[1]
	var sawAssistantToolCall, sawToolResult bool
	for _, m := range second {
		switch m["role"] {
		case "assistant":
			if _, ok := m["tool_calls"]; ok {
				sawAssistantToolCall = true
			}
		case "tool":
			sawToolResult = true
			if content, _ := m["content"].(string); !strings.Contains(content, "4") {
				t.Errorf("tool result should contain calc output, got %q", content)
			}
		}
	}
	if !sawAssistantToolCall {
		t.Error("second request did not include the assistant's tool call")
	}
	if !sawToolResult {
		t.Error("second request did not include the tool result message")
	}

	// Token accounting should reflect the latest usage, not a fake estimate.
	if tc := ag.ThoughtTrain.GetTokenCount(); tc != 28 {
		t.Errorf("expected token count 28 from usage, got %d", tc)
	}
}

// TestExecuteTaskLoopJSONFallback verifies the fallback path for models that
// emit a JSON tool call as text instead of native tool_calls.
func TestExecuteTaskLoopJSONFallback(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		finalResponse(`{"tool":"calc","args":{"expression":"3*7"}}`),
		finalResponse("It is 21."),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)

	responses, err := us.ExecuteTaskLoop("root", "compute 3*7")
	if err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	if got := responses[len(responses)-1].Content; !strings.Contains(got, "21") {
		t.Errorf("final answer should mention 21, got %q", got)
	}
	// Fallback tool results are delivered as user messages prefixed TOOL_RESULT.
	second := fs.requests[1]
	found := false
	for _, m := range second {
		if c, _ := m["content"].(string); strings.Contains(c, "TOOL_RESULT[calc]") {
			found = true
		}
	}
	if !found {
		t.Error("expected a TOOL_RESULT fallback message in the second request")
	}
}
