package agent

import (
	"strings"
	"testing"

	"gogent/internal/model"
)

func respToolCallWithFinish(name, arguments, finishReason string) string {
	return marshalCompletion(map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":       "call_1",
					"type":     "function",
					"function": map[string]any{"name": name, "arguments": arguments},
				}},
			},
			"finish_reason": finishReason,
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
}

func TestCollectToolCallsSalvagesTruncatedStructuredOutputFinalIssue390(t *testing.T) {
	s := &UserSession{}
	resp := &model.CompletionResponse{
		FinishReason: "length",
		ToolCalls: []model.ToolCall{{
			ID: "call_final",
			Function: model.FunctionCall{
				Name:      "structured_output",
				Arguments: `{"final": true, "response":"partial answer`,
			},
			Truncated: true,
		}},
	}

	calls, explicitFinal := s.collectToolCalls(resp)

	if !explicitFinal {
		t.Fatal("explicitFinal = false, want true for truncated final structured_output")
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %d, want 0 so invalid args cannot be executed: %+v", len(calls), calls)
	}
	if resp.Content != "partial answer" {
		t.Fatalf("recovered content = %q, want partial answer", resp.Content)
	}
}

func TestCollectToolCallsSalvagesStructuredOutputMissingResponseIssue390(t *testing.T) {
	s := &UserSession{}
	resp := &model.CompletionResponse{
		Content:      "assistant text fallback",
		FinishReason: "length",
		ToolCalls: []model.ToolCall{{
			ID:       "call_final",
			Function: model.FunctionCall{Name: "structured_output", Arguments: `{"final": true}`},
		}},
	}

	calls, explicitFinal := s.collectToolCalls(resp)

	if !explicitFinal {
		t.Fatal("explicitFinal = false, want true when length-cut final lacks response")
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %d, want 0: %+v", len(calls), calls)
	}
	if resp.Content != "assistant text fallback" {
		t.Fatalf("content = %q, want existing assistant text fallback preserved", resp.Content)
	}
}

func TestCollectToolCallsDoesNotSalvageWithoutFinalMarkerIssue390(t *testing.T) {
	s := &UserSession{}
	resp := &model.CompletionResponse{
		FinishReason: "length",
		ToolCalls: []model.ToolCall{{
			ID: "call_partial",
			Function: model.FunctionCall{
				Name:      "structured_output",
				Arguments: `{"response":"partial answer`,
			},
			Truncated: true,
		}},
	}

	calls, explicitFinal := s.collectToolCalls(resp)

	if explicitFinal {
		t.Fatal("explicitFinal = true without a final:true marker")
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1 unsalvaged call", len(calls))
	}
	if calls[0].Tool != "structured_output" {
		t.Fatalf("call tool = %q, want structured_output", calls[0].Tool)
	}
}

func TestRunLoopTruncatedStructuredOutputFinalDoesNotExecuteInvalidArgsIssue390(t *testing.T) {
	b := &scriptedBackend{seq: []string{
		respToolCallWithFinish("structured_output", `{"final": true, "response":"partial final`, "length"),
	}}
	us, id, getEvents := newLoopHarness(t, b)
	ctx, cancel := runLoopCtx(t)
	defer cancel()

	if _, err := us.ExecuteTaskLoop(ctx, id, "produce final"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	if got := b.requestCount(); got != 1 {
		t.Fatalf("round-trips = %d, want 1 salvaged terminal final", got)
	}
	events := getEvents()
	if n := countEvents(events, SessionEventToolCall); n != 0 {
		t.Fatalf("tool-call events = %d, want 0 for folded structured_output final", n)
	}
	if n := countEvents(events, SessionEventToolResult); n != 0 {
		t.Fatalf("tool-result events = %d, want 0 so invalid args are never surfaced", n)
	}
	final, ok := finalText(events)
	if !ok {
		t.Fatal("no SessionEventFinal emitted")
	}
	if final != "partial final" {
		t.Fatalf("final text = %q, want recovered partial final", final)
	}
}

func TestRunLoopTruncatedToolCallGetsOneContinuationIssue390(t *testing.T) {
	b := &scriptedBackend{seq: []string{
		respToolCallWithFinish("calc", `{"expression":"1+`, "length"),
		respToolCall("calc", `{"expression":"1+1"}`),
		respText("The answer is 2."),
	}}
	us, id, getEvents := newLoopHarness(t, b)
	ctx, cancel := runLoopCtx(t)
	defer cancel()

	if _, err := us.ExecuteTaskLoop(ctx, id, "add one and one"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	if got := b.requestCount(); got != 3 {
		t.Fatalf("round-trips = %d, want 3 (truncated call, one continuation, final)", got)
	}
	bodies := b.requestBodies()
	if len(bodies) < 2 || !strings.Contains(bodies[1], truncatedToolCallNote) {
		t.Fatalf("second request did not include truncated-tool continuation note; bodies=%q", bodies)
	}
	events := getEvents()
	if n := countEvents(events, SessionEventToolCall); n != 1 {
		t.Fatalf("tool-call events = %d, want exactly 1 complete calc call", n)
	}
	if n := countEvents(events, SessionEventToolResult); n != 1 {
		t.Fatalf("tool-result events = %d, want exactly 1 complete calc result", n)
	}
	for _, ev := range events {
		if ev.Type == SessionEventToolResult && strings.Contains(ev.Result, "invalid args") {
			t.Fatalf("truncated args were executed instead of continued: %q", ev.Result)
		}
	}
	final, ok := finalText(events)
	if !ok {
		t.Fatal("no SessionEventFinal emitted")
	}
	if final != "The answer is 2." {
		t.Fatalf("final text = %q, want completed answer", final)
	}
}
