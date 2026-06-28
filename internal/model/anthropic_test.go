package model

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogent/internal/config"
)

func TestStringToAPITypeAnthropic(t *testing.T) {
	for _, in := range []string{"anthropic", "Anthropic", " claude ", "CLAUDE"} {
		if got := StringToAPIType(in); got != APITypeAnthropic {
			t.Errorf("StringToAPIType(%q) = %q, want anthropic", in, got)
		}
	}
}

func TestAnthropicEndpoints(t *testing.T) {
	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType: "anthropic",
		Model:   "claude-sonnet-4-6",
	})
	if want := "https://api.anthropic.com/v1/messages"; conn.URL != want {
		t.Errorf("chat URL = %q, want %q", conn.URL, want)
	}
	if _, ok := conn.wireAdapter().(anthropicAdapter); !ok {
		t.Errorf("adapter = %T, want anthropicAdapter", conn.wireAdapter())
	}
}

func TestAnthropicBuildBody(t *testing.T) {
	maxTokens := 1024
	temp := float32(0.5)
	req := CompletionRequest{
		Model:       "claude-sonnet-4-6",
		MaxTokens:   &maxTokens,
		Temperature: &temp,
		Messages: []Message{
			{Role: RoleSystem, Content: "You are helpful."},
			{Role: RoleSystem, Content: "Be terse."},
			{Role: RoleUser, Content: "What's the weather?"},
			{Role: RoleAssistant, Content: "Let me check.", ToolCalls: []ToolCall{
				{ID: "toolu_1", Type: "function", Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`}},
			}},
			{Role: RoleTool, ToolCallID: "toolu_1", Content: "sunny"},
			{Role: RoleTool, ToolCallID: "toolu_2", Content: "warm"},
		},
		Tools: []ToolDef{
			{Type: "function", Function: FunctionDef{
				Name:        "get_weather",
				Description: "Get weather",
				Parameters:  map[string]interface{}{"type": "object"},
			}},
			{Type: "function", Function: FunctionDef{Name: "noargs"}},
		},
	}

	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}

	var got anthropicRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q", got.Model)
	}
	if got.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want 1024", got.MaxTokens)
	}
	sys, ok := got.System.([]interface{})
	if !ok || len(sys) != 1 {
		t.Fatalf("system = %#v, want one text block with cache_control", got.System)
	}
	sysBlock := sys[0].(map[string]interface{})
	if sysBlock["text"] != "You are helpful.\n\nBe terse." {
		t.Errorf("system text = %q", sysBlock["text"])
	}
	if cc, ok := sysBlock["cache_control"].(map[string]interface{}); !ok || cc["type"] != "ephemeral" {
		t.Errorf("system cache_control = %v, want ephemeral", sysBlock["cache_control"])
	}
	if got.Temperature == nil || *got.Temperature != 0.5 {
		t.Errorf("temperature = %v, want 0.5", got.Temperature)
	}

	// user / assistant / user(tool results) — the two consecutive tool results
	// merge into a single user turn.
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: %+v", len(got.Messages), got.Messages)
	}
	if got.Messages[0].Role != "user" || got.Messages[0].Content[0].Type != "text" {
		t.Errorf("msg0 = %+v", got.Messages[0])
	}
	asst := got.Messages[1]
	if asst.Role != "assistant" || len(asst.Content) != 2 {
		t.Fatalf("assistant msg = %+v", asst)
	}
	if asst.Content[0].Type != "text" || asst.Content[0].Text != "Let me check." {
		t.Errorf("assistant text block = %+v", asst.Content[0])
	}
	if asst.Content[1].Type != "tool_use" || asst.Content[1].ID != "toolu_1" ||
		asst.Content[1].Name != "get_weather" || string(asst.Content[1].Input) != `{"city":"Paris"}` {
		t.Errorf("tool_use block = %+v", asst.Content[1])
	}
	results := got.Messages[2]
	if results.Role != "user" || len(results.Content) != 2 {
		t.Fatalf("tool result turn = %+v", results)
	}
	if results.Content[0].Type != "tool_result" || results.Content[0].ToolUseID != "toolu_1" ||
		results.Content[0].Content != "sunny" {
		t.Errorf("tool_result block 0 = %+v", results.Content[0])
	}
	if results.Content[1].ToolUseID != "toolu_2" || results.Content[1].Content != "warm" {
		t.Errorf("tool_result block 1 = %+v", results.Content[1])
	}
	if results.Content[1].CacheControl == nil || results.Content[1].CacheControl.Type != "ephemeral" {
		t.Errorf("last transcript block cache_control = %+v, want ephemeral", results.Content[1].CacheControl)
	}

	// Tools map to name + input_schema; a tool with no parameters still gets an
	// object schema (Anthropic rejects a missing input_schema).
	if len(got.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(got.Tools))
	}
	if got.Tools[0].Name != "get_weather" || got.Tools[0].InputSchema == nil {
		t.Errorf("tool0 = %+v", got.Tools[0])
	}
	if got.Tools[1].InputSchema == nil {
		t.Errorf("noargs tool must have a non-nil input_schema: %+v", got.Tools[1])
	}
}

func TestAnthropicBuildBodyDefaultsMaxTokens(t *testing.T) {
	raw, err := buildBodyBytes(anthropicAdapter{}, CompletionRequest{
		Model:    "claude-x",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got anthropicRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want default 4096", got.MaxTokens)
	}
}

func TestAnthropicParseResponse(t *testing.T) {
	body := `{
		"id": "msg_1",
		"type": "message",
		"role": "assistant",
		"content": [
			{"type": "text", "text": "I'll check the weather."},
			{"type": "tool_use", "id": "toolu_9", "name": "get_weather", "input": {"city": "Paris"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 12, "output_tokens": 7, "cache_read_input_tokens": 3}
	}`

	resp, err := anthropicAdapter{}.parseResponse([]byte(body))
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if resp.Content != "I'll check the weather." {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.Role != RoleAssistant {
		t.Errorf("role = %q", resp.Role)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "toolu_9" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.Function.Arguments != `{"city": "Paris"}` && tc.Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("tool call args = %q", tc.Function.Arguments)
	}
	if resp.Usage == nil {
		t.Fatal("usage nil")
	}
	// prompt = input_tokens + cache reads; cached subset preserved.
	if resp.Usage.PromptTokens != 15 || resp.Usage.CompletionTokens != 7 ||
		resp.Usage.CachedTokens() != 3 || resp.Usage.TotalTokens != 22 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestAnthropicUsageCacheReadInputTokensFlowToCachedTokensIssue404(t *testing.T) {
	got := (anthropicUsage{
		InputTokens:              11,
		CacheReadInputTokens:     7,
		CacheCreationInputTokens: 5,
		OutputTokens:             3,
	}).toTokenUsage(0)
	if got == nil {
		t.Fatal("toTokenUsage returned nil")
	}
	if got.PromptTokens != 23 {
		t.Errorf("PromptTokens = %d, want input + cache_read + cache_creation = 23", got.PromptTokens)
	}
	if got.CachedTokens() != 7 {
		t.Errorf("CachedTokens = %d, want cache_read_input_tokens = 7", got.CachedTokens())
	}
	// #544: the cache WRITE count (cache_creation_input_tokens) must be retained,
	// not silently discarded the way it was before the read/write split. This is
	// the headline fix — Anthropic is the only provider with a write count.
	if got.Cache.WriteTokens != 5 {
		t.Errorf("Cache.WriteTokens = %d, want cache_creation_input_tokens = 5 (no longer discarded)", got.Cache.WriteTokens)
	}
	if got.TotalTokens != 26 {
		t.Errorf("TotalTokens = %d, want prompt + completion = 26", got.TotalTokens)
	}
}

// TestAnthropicCompleteRoundTrip exercises the full blocking path through a fake
// Anthropic server: request shape, auth headers, and response mapping.
func TestAnthropicCompleteRoundTrip(t *testing.T) {
	var gotPath, gotKey, gotVersion, gotAuth string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"role":"assistant","content":[{"type":"text","text":"hello back"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`))
	}))
	defer server.Close()

	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType:  "anthropic",
		Endpoint: server.URL,
		Model:    "claude-sonnet-4-6",
		APIKey:   "secret-key",
	})

	resp, err := conn.CompleteWithTools(
		[]Message{{Role: RoleUser, Content: "hello"}},
		[]ToolDef{{Type: "function", Function: FunctionDef{Name: "ping", Parameters: map[string]interface{}{"type": "object"}}}},
	)
	if err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}

	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	if gotKey != "secret-key" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotVersion != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", gotVersion, anthropicVersion)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header must not be set for Anthropic, got %q", gotAuth)
	}
	if !strings.Contains(string(gotBody), `"input_schema"`) {
		t.Errorf("request body missing input_schema tool: %s", gotBody)
	}
	if strings.Contains(string(gotBody), `"type":"function"`) {
		t.Errorf("request body must not carry OpenAI tool shape: %s", gotBody)
	}
	if resp.Content != "hello back" || resp.FinishReason != "stop" {
		t.Errorf("resp = %+v", resp)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 5 || resp.Usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

const anthropicSSE = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":11,"cache_read_input_tokens":4}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_5","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicParseStream(t *testing.T) {
	server := sseServer(t, anthropicSSE)
	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType:  "anthropic",
		Endpoint: server.URL,
		Model:    "claude-sonnet-4-6",
		APIKey:   "k",
	})

	streamCh, errCh := conn.CompleteStream([]Message{{Role: RoleUser, Content: "hi"}})
	deltas, terminal, err := drain(t, streamCh, errCh)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if got := strings.Join(deltas, ""); got != "Hello" {
		t.Errorf("assembled content = %q, want Hello", got)
	}
	if terminal.FinishReason == nil || *terminal.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %v, want tool_calls", terminal.FinishReason)
	}
	if len(terminal.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(terminal.ToolCalls))
	}
	tc := terminal.ToolCalls[0]
	if tc.ID != "toolu_5" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("streamed tool call = %+v", tc)
	}
	if terminal.Usage == nil || terminal.Usage.PromptTokens != 15 ||
		terminal.Usage.CompletionTokens != 9 || terminal.Usage.CachedTokens() != 4 {
		t.Errorf("usage = %+v", terminal.Usage)
	}
}
