package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIParseResponseReasoningContentIssue402(t *testing.T) {
	body := []byte(`{
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "",
				"reasoning_content": "glm thinking"
			},
			"finish_reason": "length"
		}],
		"usage": {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}
	}`)

	resp, err := (openAIAdapter{}).parseResponse(body)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if resp.Content != "" {
		t.Fatalf("Content = %q, want empty visible answer", resp.Content)
	}
	if resp.Reasoning != "glm thinking" {
		t.Fatalf("Reasoning = %q, want GLM reasoning_content", resp.Reasoning)
	}
	if resp.FinishReason != "length" {
		t.Fatalf("FinishReason = %q, want length", resp.FinishReason)
	}
}

func TestOpenAIParseResponseReasoningFieldIssue402(t *testing.T) {
	body := []byte(`{
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "visible",
				"reasoning": "openrouter thinking"
			},
			"finish_reason": "stop"
		}]
	}`)

	resp, err := (openAIAdapter{}).parseResponse(body)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if resp.Content != "visible" {
		t.Fatalf("Content = %q, want visible", resp.Content)
	}
	if resp.Reasoning != "openrouter thinking" {
		t.Fatalf("Reasoning = %q, want OpenRouter reasoning", resp.Reasoning)
	}
}

func TestAnthropicParseResponseThinkingReasoningIssue402(t *testing.T) {
	body := []byte(`{
		"role": "assistant",
		"content": [
			{"type": "thinking", "thinking": "think one", "signature": "sig-1"},
			{"type": "text", "text": "answer"},
			{"type": "thinking", "thinking": " think two", "signature": "sig-2"}
		],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 3, "output_tokens": 5}
	}`)

	resp, err := (anthropicAdapter{}).parseResponse(body)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if resp.Content != "answer" {
		t.Fatalf("Content = %q, want answer", resp.Content)
	}
	if resp.Reasoning != "think one think two" {
		t.Fatalf("Reasoning = %q, want concatenated thinking blocks", resp.Reasoning)
	}
	if resp.Thinking != "think one" || resp.ThinkingSignature != "sig-1" {
		t.Fatalf("Thinking cargo = (%q,%q), want first thinking block", resp.Thinking, resp.ThinkingSignature)
	}
}

func TestGeminiParseResponseThoughtReasoningIssue402(t *testing.T) {
	body := []byte(`{
		"candidates": [{
			"content": {"parts": [
				{"text": "plan", "thought": true},
				{"text": "answer"},
				{"text": " more", "thought": true}
			]},
			"finishReason": "STOP"
		}],
		"usageMetadata": {"promptTokenCount": 2, "candidatesTokenCount": 4, "totalTokenCount": 6, "thoughtsTokenCount": 3}
	}`)

	resp, err := (geminiAdapter{}).parseResponse(body)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if resp.Content != "answer" {
		t.Fatalf("Content = %q, want answer", resp.Content)
	}
	if resp.Reasoning != "plan more" {
		t.Fatalf("Reasoning = %q, want concatenated thought parts", resp.Reasoning)
	}
	if resp.Usage == nil || resp.Usage.ReasoningTokens != 3 {
		t.Fatalf("Usage = %+v, want reasoning tokens", resp.Usage)
	}
}

func TestCompleteWithToolsStreamCtxPersistsReasoningOnResponseIssue402(t *testing.T) {
	server := reasoningServer(t, reasoningContentSSE, nil)
	c := NewModelConnection()
	c.SetURL(server.URL)

	resp, err := c.CompleteWithToolsStreamCtx(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("CompleteWithToolsStreamCtx: %v", err)
	}
	want := "Let me think about this.\nDone thinking."
	if resp.Reasoning != want {
		t.Fatalf("resp.Reasoning = %q, want %q", resp.Reasoning, want)
	}
	if strings.Contains(resp.Content, "think") {
		t.Fatalf("reasoning leaked into visible content: %q", resp.Content)
	}
}

func TestMessageJSONRoundTripsReasoningIssue402(t *testing.T) {
	orig := Message{Role: RoleAssistant, Content: "", Reasoning: "saved thought"}
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"reasoning":"saved thought"`) {
		t.Fatalf("marshaled message omitted reasoning: %s", b)
	}
	var got Message
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Reasoning != orig.Reasoning {
		t.Fatalf("Reasoning = %q, want %q", got.Reasoning, orig.Reasoning)
	}
}

func TestOpenAIRequestDoesNotReplayPersistedReasoningIssue402(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	conn := NewModelConnection()
	conn.SetURL(server.URL)
	sess := NewModelSession("reasoning-replay", conn)
	sess.ReplaceTranscript([]Message{
		{Role: RoleUser, Content: "previous"},
		{Role: RoleAssistant, Content: "", Reasoning: "persisted side channel"},
	})

	if _, err := sess.SendWithToolsCtx(context.Background(), []Message{{Role: RoleUser, Content: "next"}}, nil); err != nil {
		t.Fatalf("SendWithToolsCtx: %v", err)
	}
	if strings.Contains(gotBody, `"reasoning"`) || strings.Contains(gotBody, "persisted side channel") {
		t.Fatalf("retained reasoning was replayed to OpenAI-compatible request body:\n%s", gotBody)
	}
}
