package model

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"gogent/internal/config"
)

// decodeAnthropicBody unmarshals a vertex-anthropic request body into a generic
// map so tests can assert on the raw wire shape (which is the Anthropic Messages
// shape, not gogent's OpenAI-shaped CompletionRequest).
func decodeAnthropicBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal anthropic body: %v\nbody: %s", err, b)
	}
	return m
}

func buildVertexAnthropicBody(t *testing.T, req CompletionRequest) (map[string]any, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := (anthropicAdapter{vertex: true}).buildBody(req, &buf); err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	return decodeAnthropicBody(t, buf.Bytes()), buf.Bytes()
}

func intp(i int) *int { return &i }

func TestVertexAnthropicAPITypeAndSpec(t *testing.T) {
	for _, in := range []string{"vertex-anthropic", "Vertex-Anthropic", " VERTEX-ANTHROPIC ", "claude-vertex", "CLAUDE-VERTEX"} {
		if got := StringToAPIType(in); got != APITypeVertexAnthropic {
			t.Errorf("StringToAPIType(%q) = %q, want vertex-anthropic", in, got)
		}
	}

	found := false
	for _, id := range APITypeIDs() {
		if id == string(APITypeVertexAnthropic) {
			found = true
		}
	}
	if !found {
		t.Fatalf("APITypeIDs() = %v, missing vertex-anthropic", APITypeIDs())
	}

	p := providerFor(APITypeVertexAnthropic)
	if _, ok := p.auth.(adcAuth); !ok {
		t.Errorf("auth = %T, want adcAuth", p.auth)
	}
	if !p.caps.SupportsThinking {
		t.Error("SupportsThinking = false, want true (Claude on Vertex supports extended thinking)")
	}
	if p.caps.SupportsResponseFormat {
		t.Error("SupportsResponseFormat = true, want false (Anthropic has no response_format field)")
	}
	if p.caps.SupportsReasoningEffort {
		t.Error("SupportsReasoningEffort = true, want false (reasoning_effort is not an Anthropic body param)")
	}
	// Claude on Vertex lists models via the Model Garden anthropic publisher.
	if vl, ok := p.lister.(vertexPublisherLister); !ok || vl.publisher != "anthropic" {
		t.Errorf("lister = %#v, want vertexPublisherLister{publisher: anthropic}", p.lister)
	}
	a, ok := adapterFor(APITypeVertexAnthropic).(anthropicAdapter)
	if !ok {
		t.Fatalf("adapterFor(vertex-anthropic) = %T, want anthropicAdapter", adapterFor(APITypeVertexAnthropic))
	}
	if !a.vertex {
		t.Error("adapterFor(vertex-anthropic).vertex = false, want true")
	}
}

func TestVertexAnthropicURLsFromProjectLocation(t *testing.T) {
	cases := []struct {
		name       string
		project    string
		location   string
		model      string
		wantChat   string
		wantStream string
	}{
		{
			name:       "regional",
			project:    "gogent-prod",
			location:   "us-east5",
			model:      "claude-opus-4-8",
			wantChat:   "https://us-east5-aiplatform.googleapis.com/v1/projects/gogent-prod/locations/us-east5/publishers/anthropic/models/claude-opus-4-8:rawPredict",
			wantStream: "https://us-east5-aiplatform.googleapis.com/v1/projects/gogent-prod/locations/us-east5/publishers/anthropic/models/claude-opus-4-8:streamRawPredict",
		},
		{
			name:       "global drops host prefix only",
			project:    "root-smile-452719-d4",
			location:   "global",
			model:      "claude-opus-4-8",
			wantChat:   "https://aiplatform.googleapis.com/v1/projects/root-smile-452719-d4/locations/global/publishers/anthropic/models/claude-opus-4-8:rawPredict",
			wantStream: "https://aiplatform.googleapis.com/v1/projects/root-smile-452719-d4/locations/global/publishers/anthropic/models/claude-opus-4-8:streamRawPredict",
		},
		{
			name:       "dated snapshot id with @ separator",
			project:    "p",
			location:   "us-east5",
			model:      "claude-opus-4-5@20251101",
			wantChat:   "https://us-east5-aiplatform.googleapis.com/v1/projects/p/locations/us-east5/publishers/anthropic/models/claude-opus-4-5@20251101:rawPredict",
			wantStream: "https://us-east5-aiplatform.googleapis.com/v1/projects/p/locations/us-east5/publishers/anthropic/models/claude-opus-4-5@20251101:streamRawPredict",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := NewModelConnectionFromConfig(&config.ModelConfig{
				APIType:  "vertex-anthropic",
				Project:  tc.project,
				Location: tc.location,
				Model:    tc.model,
			})
			if conn.URL != tc.wantChat {
				t.Errorf("URL = %q, want %q", conn.URL, tc.wantChat)
			}
			if conn.StreamURL != tc.wantStream {
				t.Errorf("StreamURL = %q, want %q", conn.StreamURL, tc.wantStream)
			}
		})
	}
}

func TestVertexAnthropicBodyShape(t *testing.T) {
	temp := float32(0.7)
	body, _ := buildVertexAnthropicBody(t, CompletionRequest{
		Model:       "claude-opus-4-8",
		Stream:      true,
		MaxTokens:   intp(200),
		Temperature: &temp,
		TopP:        &temp,
		Messages: []Message{
			{Role: RoleSystem, Content: "You are helpful."},
			{Role: RoleUser, Content: "hi"},
		},
	})

	if _, ok := body["model"]; ok {
		t.Error("body has model; Vertex carries the model in the URL path, not the body")
	}
	if got := body["anthropic_version"]; got != vertexAnthropicVersion {
		t.Errorf("anthropic_version = %v, want %q", got, vertexAnthropicVersion)
	}
	if body["stream"] != true {
		t.Errorf("stream = %v, want true (streamRawPredict still reads stream from the body)", body["stream"])
	}
	// buildBody is now a PURE FORWARDER on both paths (issue #543): whether a model
	// accepts sampling params is decided UPSTREAM in buildRequest via the
	// (provider,model) capability layer (resolveModelQuirks), not here. This call
	// hands the adapter non-nil pointers, so they are forwarded verbatim — pinning
	// the new contract. The "current-gen Claude drops sampling" assertion now lives
	// at the buildRequest level (see caps_test.go) where the decision actually
	// resides.
	if body["temperature"] == nil {
		t.Error("body missing temperature; the adapter must forward whatever buildRequest hands it")
	}
	if body["top_p"] == nil {
		t.Error("body missing top_p; the adapter must forward whatever buildRequest hands it")
	}
	if got, ok := body["max_tokens"].(float64); !ok || got != 200 {
		t.Errorf("max_tokens = %v, want 200", body["max_tokens"])
	}

	// System is a block array carrying a cache_control breakpoint.
	sys, ok := body["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("system = %v, want a one-element block array", body["system"])
	}
	sysBlock := sys[0].(map[string]any)
	if sysBlock["type"] != "text" || sysBlock["text"] != "You are helpful." {
		t.Errorf("system block = %v, want text block with the prompt", sysBlock)
	}
	if cc, ok := sysBlock["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Errorf("system cache_control = %v, want {type: ephemeral}", sysBlock["cache_control"])
	}

	// The last turn's last content block carries a cache breakpoint.
	msgs := body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	lastBlocks := last["content"].([]any)
	lastBlock := lastBlocks[len(lastBlocks)-1].(map[string]any)
	if cc, ok := lastBlock["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Errorf("last-turn cache_control = %v, want {type: ephemeral}", lastBlock["cache_control"])
	}
}

func TestVertexAnthropicNoSystemOmitsField(t *testing.T) {
	body, _ := buildVertexAnthropicBody(t, CompletionRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: intp(50),
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
	})
	if _, ok := body["system"]; ok {
		t.Error("system present with no system message; want omitted")
	}
}

func TestVertexAnthropicAdaptiveThinking(t *testing.T) {
	// Thinking enabled -> adaptive thinking with summarized display.
	on, _ := buildVertexAnthropicBody(t, CompletionRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: intp(50),
		Thinking:  &ThinkingParam{Type: "enabled"},
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
	})
	think, ok := on["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking = %v, want an object when enabled", on["thinking"])
	}
	if think["type"] != "adaptive" {
		t.Errorf("thinking.type = %v, want adaptive", think["type"])
	}
	if think["display"] != "summarized" {
		t.Errorf("thinking.display = %v, want summarized", think["display"])
	}

	// Thinking disabled -> no thinking field at all.
	off, _ := buildVertexAnthropicBody(t, CompletionRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: intp(50),
		Thinking:  &ThinkingParam{Type: "disabled"},
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
	})
	if _, ok := off["thinking"]; ok {
		t.Error("thinking present when disabled; want omitted")
	}
}

func TestVertexAnthropicThinkingBlockReplayedBeforeToolUse(t *testing.T) {
	body, _ := buildVertexAnthropicBody(t, CompletionRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: intp(50),
		Messages: []Message{
			{Role: RoleUser, Content: "weather?"},
			{
				Role:              RoleAssistant,
				Content:           "Let me check.",
				Thinking:          "The user wants weather; I'll call the tool.",
				ThinkingSignature: "sig-abc123",
				ToolCalls: []ToolCall{{
					ID:       "toolu_1",
					Type:     "function",
					Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`},
				}},
			},
			{Role: RoleTool, ToolCallID: "toolu_1", Content: "sunny"},
		},
	})

	msgs := body["messages"].([]any)
	// messages: [user, assistant(thinking,text,tool_use), user(tool_result)]
	var assistant map[string]any
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" {
			assistant = mm
			break
		}
	}
	if assistant == nil {
		t.Fatalf("no assistant turn in %v", msgs)
	}
	blocks := assistant["content"].([]any)
	if len(blocks) < 2 {
		t.Fatalf("assistant blocks = %v, want at least thinking + tool_use", blocks)
	}
	first := blocks[0].(map[string]any)
	if first["type"] != "thinking" {
		t.Errorf("first assistant block type = %v, want thinking (must precede tool_use)", first["type"])
	}
	if first["thinking"] != "The user wants weather; I'll call the tool." {
		t.Errorf("thinking text = %v, want the captured reasoning", first["thinking"])
	}
	if first["signature"] != "sig-abc123" {
		t.Errorf("thinking signature = %v, want sig-abc123 (must be replayed verbatim)", first["signature"])
	}
	// A tool_use block must appear after the thinking block.
	sawToolUse := false
	for _, b := range blocks[1:] {
		if b.(map[string]any)["type"] == "tool_use" {
			sawToolUse = true
		}
	}
	if !sawToolUse {
		t.Errorf("assistant turn has no tool_use after thinking: %v", blocks)
	}
}

func TestDirectAnthropicBodyEmitsPromptCacheBreakpointsIssue404(t *testing.T) {
	// Direct Anthropic keeps the direct-only fields (model present, no
	// anthropic_version body field, sampling params present) while also emitting
	// the same prompt-cache breakpoints as vertex-anthropic (issue #404).
	temp := float32(0.3)
	var buf bytes.Buffer
	if err := (anthropicAdapter{}).buildBody(CompletionRequest{
		Model:       "claude-opus-4-8",
		MaxTokens:   intp(100),
		Temperature: &temp,
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "hi"},
		},
	}, &buf); err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	body := decodeAnthropicBody(t, buf.Bytes())
	if body["model"] != "claude-opus-4-8" {
		t.Errorf("direct body model = %v, want claude-opus-4-8", body["model"])
	}
	if _, ok := body["anthropic_version"]; ok {
		t.Error("direct body has anthropic_version; it should ride the header, not the body")
	}
	sys, ok := body["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("direct body system = %v, want one-element block array", body["system"])
	}
	sysBlock := sys[0].(map[string]any)
	if sysBlock["type"] != "text" || sysBlock["text"] != "sys" {
		t.Errorf("direct system block = %v, want text block with sys", sysBlock)
	}
	if cc, ok := sysBlock["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Errorf("direct system cache_control = %v, want ephemeral", sysBlock["cache_control"])
	}
	if body["temperature"] == nil {
		t.Error("direct body missing temperature; non-vertex path must keep sampling params")
	}

	msgs := body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	blocks := last["content"].([]any)
	lastBlock := blocks[len(blocks)-1].(map[string]any)
	if cc, ok := lastBlock["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Errorf("direct last-turn cache_control = %v, want ephemeral", lastBlock["cache_control"])
	}
}

func TestVertexAnthropicVolatileTailMergesAfterToolResultButBreakpointStaysOnTranscriptIssue404(t *testing.T) {
	body, _ := buildVertexAnthropicBody(t, CompletionRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: intp(100),
		Messages: []Message{
			{Role: RoleSystem, Content: "stable system"},
			{Role: RoleUser, Content: "question"},
			{
				Role: RoleAssistant,
				ToolCalls: []ToolCall{{
					ID:       "toolu_1",
					Type:     "function",
					Function: FunctionCall{Name: "lookup", Arguments: `{"q":"x"}`},
				}},
			},
			{Role: RoleTool, ToolCallID: "toolu_1", Content: `{"answer":"42"}`},
			{Role: RoleUser, Content: "## Git status\nM file.go", Volatile: true},
		},
	})

	msgs := body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("last message role = %v, want merged user turn", last["role"])
	}
	blocks := last["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("last user blocks = %v, want tool_result + volatile text", blocks)
	}
	toolResult := blocks[0].(map[string]any)
	if toolResult["type"] != "tool_result" || toolResult["tool_use_id"] != "toolu_1" {
		t.Fatalf("first merged block = %v, want tool_result", toolResult)
	}
	if cc, ok := toolResult["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Errorf("tool_result cache_control = %v, want breakpoint on last transcript block", toolResult["cache_control"])
	}
	volatile := blocks[1].(map[string]any)
	if volatile["type"] != "text" || volatile["text"] != "## Git status\nM file.go" {
		t.Fatalf("second merged block = %v, want volatile text", volatile)
	}
	if _, ok := volatile["cache_control"]; ok {
		t.Errorf("volatile tail has cache_control; breakpoint must stay on transcript block: %v", volatile)
	}
}

func TestVertexAnthropicParseResponseCapturesThinking(t *testing.T) {
	resp, err := (anthropicAdapter{vertex: true}).parseResponse([]byte(`{
		"content": [
			{"type":"thinking","thinking":"reasoning here","signature":"sig-xyz"},
			{"type":"text","text":"answer"},
			{"type":"tool_use","id":"toolu_9","name":"f","input":{"a":1}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":10,"output_tokens":5}
	}`))
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if resp.Thinking != "reasoning here" {
		t.Errorf("Thinking = %q, want %q", resp.Thinking, "reasoning here")
	}
	if resp.ThinkingSignature != "sig-xyz" {
		t.Errorf("ThinkingSignature = %q, want %q", resp.ThinkingSignature, "sig-xyz")
	}
	if resp.Content != "answer" {
		t.Errorf("Content = %q, want %q", resp.Content, "answer")
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "toolu_9" {
		t.Errorf("ToolCalls = %+v, want one call toolu_9", resp.ToolCalls)
	}
}

func TestVertexAnthropicParseStreamCapturesThinkingAndSignature(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think "}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"more"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-stream"}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hello"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	ch := make(chan StreamResponse, 64)
	content, _, err := (anthropicAdapter{vertex: true}).parseStream(strings.NewReader(stream), ch)
	close(ch)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if content != "hello" {
		t.Errorf("content = %q, want %q", content, "hello")
	}
	var reasoning strings.Builder
	var done StreamResponse
	for ev := range ch {
		reasoning.WriteString(ev.Reasoning)
		if ev.Done {
			done = ev
		}
	}
	if reasoning.String() != "think more" {
		t.Errorf("live reasoning = %q, want %q", reasoning.String(), "think more")
	}
	if done.Thinking != "think more" {
		t.Errorf("Done.Thinking = %q, want %q", done.Thinking, "think more")
	}
	if done.ThinkingSignature != "sig-stream" {
		t.Errorf("Done.ThinkingSignature = %q, want %q", done.ThinkingSignature, "sig-stream")
	}
}

// TestVertexAnthropicEndToEndADCAndWire exercises the full connection: ADC bearer
// auth, the :rawPredict path, and an Anthropic-shaped request/response round-trip.
func TestVertexAnthropicEndToEndADCAndWire(t *testing.T) {
	tokenSource := &staticTokenSource{token: "vtx-anthropic-token"}
	var scopesSeen []string
	withFakeADCTokenSource(t, func(ctx context.Context, scopes ...string) (oauth2.TokenSource, error) {
		scopesSeen = append([]string(nil), scopes...)
		return tokenSource, nil
	})

	var gotAuth, gotPath string
	var rawBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		rawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hi from claude on vertex"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":6}}`))
	}))
	defer server.Close()

	// An explicit endpoint overrides the derived base but the spec's chatURLFunc
	// still appends the :rawPredict model path, so we get the Anthropic wire here.
	conn := NewModelConnectionFromConfig(&config.ModelConfig{
		APIType:     "vertex-anthropic",
		Endpoint:    server.URL,
		Model:       "claude-opus-4-8",
		Temperature: 0.9, // must NOT be forwarded
		MaxTokens:   321,
	})
	resp, err := conn.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if !strings.HasSuffix(gotPath, "/publishers/anthropic/models/claude-opus-4-8:rawPredict") {
		t.Errorf("path = %q, want .../publishers/anthropic/models/claude-opus-4-8:rawPredict", gotPath)
	}
	if gotAuth != "Bearer vtx-anthropic-token" {
		t.Errorf("Authorization = %q, want ADC bearer token", gotAuth)
	}
	if len(scopesSeen) != 1 || scopesSeen[0] != adcScope {
		t.Errorf("ADC scopes = %v, want [%q]", scopesSeen, adcScope)
	}
	body := decodeAnthropicBody(t, rawBody)
	if _, ok := body["model"]; ok {
		t.Error("forwarded body has model; want it only in the URL path")
	}
	if body["anthropic_version"] != vertexAnthropicVersion {
		t.Errorf("anthropic_version = %v, want %q", body["anthropic_version"], vertexAnthropicVersion)
	}
	if _, ok := body["temperature"]; ok {
		t.Error("forwarded body has temperature; want sampling params dropped on Vertex")
	}
	if got, ok := body["max_tokens"].(float64); !ok || got != 321 {
		t.Errorf("max_tokens = %v, want 321", body["max_tokens"])
	}
	if resp.Content != "hi from claude on vertex" || resp.FinishReason != "stop" {
		t.Errorf("response = %+v, want Anthropic-shaped reply mapped to stop", resp)
	}
}
