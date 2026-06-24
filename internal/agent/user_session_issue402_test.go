package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"gogent/internal/model"
)

func respLengthIssue402(content, reasoningContent, reasoning string) string {
	msg := map[string]any{"role": "assistant", "content": content}
	if reasoningContent != "" {
		msg["reasoning_content"] = reasoningContent
	}
	if reasoning != "" {
		msg["reasoning"] = reasoning
	}
	return marshalCompletion(map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": "length",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
}

func requestMaxTokensIssue402(t *testing.T, body string) int {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal request body: %v\n%s", err, body)
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "maxOutputTokens"} {
		if v, ok := req[key]; ok {
			f, ok := v.(float64)
			if !ok {
				t.Fatalf("%s = %T(%v), want number", key, v, v)
			}
			return int(f)
		}
	}
	t.Fatalf("request body missing max token field: %s", body)
	return 0
}

func terminalAssistantIssue402(t *testing.T, us *UserSession) model.Message {
	t.Helper()
	transcript := us.RootAgent.ThoughtTrain.GetTranscript()
	for i := len(transcript) - 1; i >= 0; i-- {
		if transcript[i].Role == model.RoleAssistant {
			return transcript[i]
		}
	}
	t.Fatalf("no assistant message in transcript: %+v", transcript)
	return model.Message{}
}

func TestRunLoopEmptyLengthRaisesBudgetIssue402(t *testing.T) {
	b := &scriptedBackend{seq: []string{
		respLengthIssue402("", "", ""),
		respText("final after larger budget"),
	}}
	us, id, getEvents := newLoopHarness(t, b)
	ctx, cancel := runLoopCtx(t)
	defer cancel()

	if _, err := us.ExecuteTaskLoop(ctx, id, "finish"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}
	if got := b.requestCount(); got != 2 {
		t.Fatalf("round-trips = %d, want 2 (empty length retry plus final)", got)
	}
	bodies := b.requestBodies()
	if len(bodies) != 2 {
		t.Fatalf("request bodies = %d, want 2", len(bodies))
	}
	firstMax := requestMaxTokensIssue402(t, bodies[0])
	secondMax := requestMaxTokensIssue402(t, bodies[1])
	if secondMax <= firstMax {
		t.Fatalf("max tokens did not increase on empty length retry: first=%d second=%d\nsecond body=%s", firstMax, secondMax, bodies[1])
	}
	if !strings.Contains(bodies[1], truncationContinueNote) {
		t.Fatalf("retry request missing truncation continuation note: %s", bodies[1])
	}
	terminal := terminalAssistantIssue402(t, us)
	if strings.TrimSpace(terminal.Content) == "" && strings.TrimSpace(terminal.Reasoning) == "" {
		t.Fatalf("terminal assistant message is empty: %+v", terminal)
	}
	if terminal.Content != "final after larger budget" {
		t.Fatalf("terminal content = %q, want final after larger budget", terminal.Content)
	}
	if countEvents(getEvents(), SessionEventError) != 0 {
		t.Fatalf("unexpected error events: %+v", getEvents())
	}
}

func TestRunLoopPartialLengthContinuesIssue402(t *testing.T) {
	b := &scriptedBackend{seq: []string{
		respLengthIssue402("partial answer", "", ""),
		respText("completed answer"),
	}}
	us, id, getEvents := newLoopHarness(t, b)
	ctx, cancel := runLoopCtx(t)
	defer cancel()

	if _, err := us.ExecuteTaskLoop(ctx, id, "finish"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}
	if got := b.requestCount(); got != 2 {
		t.Fatalf("round-trips = %d, want 2 (partial length continuation plus final)", got)
	}
	bodies := b.requestBodies()
	if !strings.Contains(bodies[1], truncationContinueNote) {
		t.Fatalf("continuation request missing truncation note: %s", bodies[1])
	}
	if secondMax, firstMax := requestMaxTokensIssue402(t, bodies[1]), requestMaxTokensIssue402(t, bodies[0]); secondMax != firstMax {
		t.Fatalf("partial-output continuation should not raise max tokens: first=%d second=%d", firstMax, secondMax)
	}
	terminal := terminalAssistantIssue402(t, us)
	if terminal.Content != "completed answer" {
		t.Fatalf("terminal content = %q, want completed answer", terminal.Content)
	}
	final, ok := finalText(getEvents())
	if !ok || final != "completed answer" {
		t.Fatalf("final event = (%q,%v), want completed answer", final, ok)
	}
}

func TestRunLoopReasoningOnlyLengthContinuesIssue402(t *testing.T) {
	b := &scriptedBackend{seq: []string{
		respLengthIssue402("", "kept reasoning", ""),
		respText("answer after reasoning continuation"),
	}}
	us, id, getEvents := newLoopHarness(t, b)
	ctx, cancel := runLoopCtx(t)
	defer cancel()

	if _, err := us.ExecuteTaskLoop(ctx, id, "finish"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}
	if got := b.requestCount(); got != 2 {
		t.Fatalf("round-trips = %d, want 2 (reasoning-only continuation plus final)", got)
	}
	bodies := b.requestBodies()
	if !strings.Contains(bodies[1], truncationContinueNote) {
		t.Fatalf("reasoning-only continuation request missing note: %s", bodies[1])
	}
	if secondMax, firstMax := requestMaxTokensIssue402(t, bodies[1]), requestMaxTokensIssue402(t, bodies[0]); secondMax != firstMax {
		t.Fatalf("reasoning-only recoverable continuation should not raise max tokens: first=%d second=%d", firstMax, secondMax)
	}
	transcript := us.RootAgent.ThoughtTrain.GetTranscript()
	var sawReasoning bool
	for _, m := range transcript {
		if m.Role == model.RoleAssistant && m.Reasoning == "kept reasoning" {
			sawReasoning = true
		}
	}
	if !sawReasoning {
		t.Fatalf("reasoning-only assistant turn was not retained in transcript: %+v", transcript)
	}
	terminal := terminalAssistantIssue402(t, us)
	if strings.TrimSpace(terminal.Content) == "" && strings.TrimSpace(terminal.Reasoning) == "" {
		t.Fatalf("terminal assistant message is empty: %+v", terminal)
	}
	if final, ok := finalText(getEvents()); !ok || final != "answer after reasoning continuation" {
		t.Fatalf("final event = (%q,%v), want answer after reasoning continuation", final, ok)
	}
}

func TestRunLoopLengthAtTokenCeilingSurfacesErrorIssue402(t *testing.T) {
	b := &scriptedBackend{def: respLengthIssue402("", "", "")}
	us, id, getEvents := newLoopHarness(t, b)
	ctx, cancel := runLoopCtx(t)
	defer cancel()

	if _, err := us.ExecuteTaskLoop(ctx, id, "finish"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}
	if got := b.requestCount(); got != maxTruncationRetries+1 {
		t.Fatalf("round-trips = %d, want initial plus %d retries", got, maxTruncationRetries)
	}
	events := getEvents()
	if countEvents(events, SessionEventError) != 1 {
		t.Fatalf("error events = %d, want 1: %+v", countEvents(events, SessionEventError), events)
	}
	final, ok := finalText(events)
	if !ok {
		t.Fatal("no final event emitted on exhausted truncation retries")
	}
	if !strings.Contains(final, truncationNoticeMarker) || strings.TrimSpace(final) == truncationNoticeMarker {
		t.Fatalf("final truncation notice = %q, want actionable non-empty notice", final)
	}
	terminal := terminalAssistantIssue402(t, us)
	if strings.TrimSpace(terminal.Content) == "" {
		t.Fatalf("terminal assistant content empty after exhausted truncation: %+v", terminal)
	}
	if !strings.Contains(terminal.Content, truncationNoticeMarker) {
		t.Fatalf("terminal content missing truncation marker: %q", terminal.Content)
	}
}
