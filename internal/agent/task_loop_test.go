package agent

import (
	"context"
	"encoding/json"
	"fmt"
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

	responses, err := us.ExecuteTaskLoop(context.Background(), "root", "what is 2+2?")
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

// TestFinalRecoversAnswerWhenTerminalTurnEmpty reproduces #171: the model states
// its answer as assistant content alongside a tool call, then returns an empty
// terminal turn. The final event must still carry the answer rather than an empty
// string (which the TUI renders as nothing, presenting as tool->idle with the
// last turn missing).
func TestFinalRecoversAnswerWhenTerminalTurnEmpty(t *testing.T) {
	contentAndCall := map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": "Here is the result: 4.",
				"tool_calls": []map[string]interface{}{{
					"id":   "c1",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "calc",
						"arguments": `{"expression":"2+2"}`,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
	fs := &fakeServer{responses: []map[string]interface{}{
		contentAndCall,
		finalResponse(""), // empty terminal turn — no content, no calls
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	var finalText string
	var sawFinal bool
	us.SetObserver(func(ev SessionEvent) {
		if ev.Type == SessionEventFinal {
			sawFinal = true
			finalText = ev.Text
		}
	})

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "what is 2+2?"); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	if !sawFinal {
		t.Fatal("no SessionEventFinal emitted")
	}
	if strings.TrimSpace(finalText) == "" {
		t.Fatal("final event text is empty; the last answer was dropped (#171)")
	}
	if !strings.Contains(finalText, "4") {
		t.Errorf("final text should recover the answer, got %q", finalText)
	}
}

// TestInjectUserNoteSplicesAtTurnBoundary verifies issue #170 phase 2: with
// mid-turn injection enabled, a note handed to the running session via
// InjectUserNote is spliced into the conversation at the next turn boundary
// (between the tool round and the following model call) framed as a clarification,
// so it reaches the model's next request rather than waiting for full idle.
func TestInjectUserNoteSplicesAtTurnBoundary(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("call_1", "calc", `{"expression":"2+2"}`),
		finalResponse("The answer is 4."),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL)
	us.SetInjectQueuedInput(true)
	// Queue the note before the loop starts; it is drained at the first turn
	// boundary (after the calc tool round), so it must appear in the second request.
	us.InjectUserNote("actually use base 16")

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "what is 2+2?"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	if len(fs.requests) != 2 {
		t.Fatalf("expected 2 requests to the model, got %d", len(fs.requests))
	}

	want := fmt.Sprintf(injectedNoteTemplate, "actually use base 16")
	var sawInjected bool
	for _, m := range fs.requests[1] {
		if m["role"] != "user" {
			continue
		}
		if content, _ := m["content"].(string); strings.Contains(content, want) {
			sawInjected = true
		}
	}
	if !sawInjected {
		t.Errorf("second request did not carry the injected clarification %q; messages: %v", want, fs.requests[1])
	}

	// The slot is single-use: a second turn with nothing queued must not re-inject.
	if note := us.takePendingNote(); note != "" {
		t.Errorf("pending note should be cleared after injection, got %q", note)
	}
}

// TestInjectUserNoteDisabledIsNotInjected verifies the experimental flag gates
// the splice: with injection off (the default), a queued note is not spliced into
// the running turn — the drain-on-idle path (phase 1) owns it instead.
func TestInjectUserNoteDisabledIsNotInjected(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("call_1", "calc", `{"expression":"2+2"}`),
		finalResponse("The answer is 4."),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, _ := newLoopSession(t, server.URL) // injection left off (default)
	us.InjectUserNote("should not be injected")

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "what is 2+2?"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	for _, req := range fs.requests {
		for _, m := range req {
			if content, _ := m["content"].(string); strings.Contains(content, "should not be injected") {
				t.Fatal("note was injected even though the flag is off")
			}
		}
	}
	// With injection off the note is left pending for the UI's idle drain.
	if note := us.takePendingNote(); note != "should not be injected" {
		t.Errorf("pending note = %q, want it left intact for drain-on-idle", note)
	}
}

// TestInjectUserNoteSafeWhenIdle verifies InjectUserNote is safe to call when no
// loop is running: it does not panic, ignores empty/whitespace text, and queues a
// real note for the next turn (issue #170).
func TestInjectUserNoteSafeWhenIdle(t *testing.T) {
	conn := model.NewModelConnection()
	sess := model.NewModelSession("test", conn)
	ag := NewAgent("root", sess)
	us := NewUserSession("idle", ag)

	// No loop running: these must not panic.
	us.InjectUserNote("")
	us.InjectUserNote("   ")
	if note := us.takePendingNote(); note != "" {
		t.Errorf("blank notes should be ignored, got %q", note)
	}
	us.InjectUserNote("queued while idle")
	if note := us.takePendingNote(); note != "queued while idle" {
		t.Errorf("idle InjectUserNote should queue the note, got %q", note)
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

	responses, err := us.ExecuteTaskLoop(context.Background(), "root", "compute 3*7")
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
