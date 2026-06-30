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

func TestVertexNativeAPITypeSpecAndURLs(t *testing.T) {
	for _, in := range []string{"vertex-native", "Vertex-Native", " gemini "} {
		if got := StringToAPIType(in); got != APITypeVertexNative {
			t.Errorf("StringToAPIType(%q) = %q, want vertex-native", in, got)
		}
	}

	found := false
	for _, id := range APITypeIDs() {
		if id == string(APITypeVertexNative) {
			found = true
		}
	}
	if !found {
		t.Fatalf("APITypeIDs() = %v, missing vertex-native", APITypeIDs())
	}

	p := providerFor(APITypeVertexNative)
	if _, ok := p.auth.(adcAuth); !ok {
		t.Errorf("auth = %T, want adcAuth", p.auth)
	}
	if !p.caps.SupportsThinking {
		t.Error("SupportsThinking = false, want true for native Gemini")
	}
	if !p.caps.SupportsResponseFormat {
		t.Error("SupportsResponseFormat = false, want true for native Gemini responseSchema")
	}
	if p.caps.SupportsReasoningEffort {
		t.Error("SupportsReasoningEffort = true, want false for native Gemini")
	}
	if p.caps.ReasoningRejectsTemperature {
		t.Error("ReasoningRejectsTemperature = true, want false because Gemini thinking accepts temperature/topP")
	}
	if vl, ok := p.lister.(vertexPublisherLister); !ok || vl.publisher != "google" {
		t.Errorf("lister = %#v, want vertexPublisherLister{publisher: google}", p.lister)
	}
	if _, ok := adapterFor(APITypeVertexNative).(geminiAdapter); !ok {
		t.Errorf("adapterFor(vertex-native) = %T, want geminiAdapter", adapterFor(APITypeVertexNative))
	}

	cases := []struct {
		name       string
		project    string
		location   string
		model      string
		wantURL    string
		wantStream string
	}{
		{
			name:       "regional",
			project:    "gogent-prod",
			location:   "us-central1",
			model:      "gemini-2.5-flash",
			wantURL:    "https://us-central1-aiplatform.googleapis.com/v1/projects/gogent-prod/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent",
			wantStream: "https://us-central1-aiplatform.googleapis.com/v1/projects/gogent-prod/locations/us-central1/publishers/google/models/gemini-2.5-flash:streamGenerateContent?alt=sse",
		},
		{
			name:       "global drops host prefix only",
			project:    "gogent-prod",
			location:   " Global ",
			model:      "gemini-2.5-pro",
			wantURL:    "https://aiplatform.googleapis.com/v1/projects/gogent-prod/locations/global/publishers/google/models/gemini-2.5-pro:generateContent",
			wantStream: "https://aiplatform.googleapis.com/v1/projects/gogent-prod/locations/global/publishers/google/models/gemini-2.5-pro:streamGenerateContent?alt=sse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := NewModelConnection(
				&config.ProviderConnection{
					APIType:  "gemini",
					Project:  tc.project,
					Location: tc.location,
				},
				&config.ModelConfig{
					Model: tc.model,
				},
			)
			if conn.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", conn.URL, tc.wantURL)
			}
			if conn.StreamURL != tc.wantStream {
				t.Errorf("StreamURL = %q, want %q", conn.StreamURL, tc.wantStream)
			}
		})
	}
}

func TestGeminiAdapterBuildBodyRepresentativeRequest(t *testing.T) {
	temp := float32(0.7)
	topP := float32(0.95)
	maxTokens := 128
	req := CompletionRequest{
		Model:       "must-not-be-in-native-body",
		Temperature: &temp,
		TopP:        &topP,
		MaxTokens:   &maxTokens,
		Thinking:    &ThinkingParam{Type: "enabled"},
		Messages: []Message{
			{Role: RoleSystem, Content: "Be precise."},
			UserImageMessage("What is the weather?", DataURL("image/png", []byte("png-bytes"))),
			{
				Role:    RoleAssistant,
				Content: "I will check.",
				ToolCalls: []ToolCall{{
					ID:       "call_weather",
					Type:     "function",
					Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`},
				}},
			},
			{Role: RoleTool, Name: "get_weather", ToolCallID: "call_weather", Content: "18"},
		},
		Tools: []ToolDef{{
			Type: "function",
			Function: FunctionDef{
				Name:        "get_weather",
				Description: "Get weather",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"city": map[string]interface{}{"type": "string"},
					},
					"required": []string{"city"},
				},
			},
		}},
		ToolChoice: &ToolChoice{Mode: ToolChoiceAuto},
		ResponseFormat: JSONSchemaResponseFormat("weather", map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"summary": map[string]interface{}{"type": "string"},
			},
			"required": []string{"summary"},
		}),
	}

	raw, err := buildBodyBytes(geminiAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal body: %v\n%s", err, raw)
	}

	if _, ok := got["model"]; ok {
		t.Fatalf("native Gemini body contains model field: %s", raw)
	}
	system := got["systemInstruction"].(map[string]interface{})
	if text := system["parts"].([]interface{})[0].(map[string]interface{})["text"]; text != "Be precise." {
		t.Errorf("systemInstruction text = %v, want Be precise.", text)
	}

	contents := got["contents"].([]interface{})
	if len(contents) != 3 {
		t.Fatalf("contents len = %d, want 3: %s", len(contents), raw)
	}
	user := contents[0].(map[string]interface{})
	if user["role"] != "user" {
		t.Errorf("first content role = %v, want user", user["role"])
	}
	userParts := user["parts"].([]interface{})
	if userParts[0].(map[string]interface{})["text"] != "What is the weather?" {
		t.Errorf("user text part = %v", userParts[0])
	}
	inline := userParts[1].(map[string]interface{})["inlineData"].(map[string]interface{})
	if inline["mimeType"] != "image/png" || inline["data"] != "cG5nLWJ5dGVz" {
		t.Errorf("inlineData = %+v, want image/png base64 payload", inline)
	}

	modelTurn := contents[1].(map[string]interface{})
	if modelTurn["role"] != "model" {
		t.Errorf("assistant role = %v, want model", modelTurn["role"])
	}
	modelParts := modelTurn["parts"].([]interface{})
	fc := modelParts[1].(map[string]interface{})["functionCall"].(map[string]interface{})
	if fc["name"] != "get_weather" || fc["id"] != "call_weather" {
		t.Errorf("functionCall = %+v, want get_weather/call_weather", fc)
	}
	if fc["args"].(map[string]interface{})["city"] != "Paris" {
		t.Errorf("functionCall args = %+v, want city Paris", fc["args"])
	}

	toolTurn := contents[2].(map[string]interface{})
	if toolTurn["role"] != "user" {
		t.Errorf("tool result role = %v, want user", toolTurn["role"])
	}
	fr := toolTurn["parts"].([]interface{})[0].(map[string]interface{})["functionResponse"].(map[string]interface{})
	if fr["name"] != "get_weather" || fr["id"] != "call_weather" {
		t.Errorf("functionResponse = %+v, want get_weather/call_weather", fr)
	}
	respObj := fr["response"].(map[string]interface{})
	if respObj["result"] != float64(18) {
		t.Errorf("scalar functionResponse response = %+v, want {result:18}", respObj)
	}

	decl := got["tools"].([]interface{})[0].(map[string]interface{})["functionDeclarations"].([]interface{})[0].(map[string]interface{})
	params := decl["parameters"].(map[string]interface{})
	if params["type"] != "OBJECT" {
		t.Errorf("tool schema type = %v, want OBJECT", params["type"])
	}
	city := params["properties"].(map[string]interface{})["city"].(map[string]interface{})
	if city["type"] != "STRING" {
		t.Errorf("nested tool schema type = %v, want STRING", city["type"])
	}

	functionCalling := got["toolConfig"].(map[string]interface{})["functionCallingConfig"].(map[string]interface{})
	if functionCalling["mode"] != "AUTO" {
		t.Errorf("functionCallingConfig.mode = %v, want AUTO", functionCalling["mode"])
	}
	// Vertex AI rejects a parallelFunctionCalls field in functionCallingConfig, so
	// gogent must not emit it (it defaults to parallel calls anyway).
	if _, ok := functionCalling["parallelFunctionCalls"]; ok {
		t.Errorf("functionCallingConfig.parallelFunctionCalls present (%v); Vertex rejects this field", functionCalling["parallelFunctionCalls"])
	}

	gen := got["generationConfig"].(map[string]interface{})
	if gen["temperature"] != float64(0.7) || gen["topP"] != float64(0.95) || gen["maxOutputTokens"] != float64(128) {
		t.Errorf("generationConfig sampling = %+v, want temperature/topP/maxOutputTokens", gen)
	}
	if gen["responseMimeType"] != "application/json" {
		t.Errorf("responseMimeType = %v, want application/json", gen["responseMimeType"])
	}
	responseSchema := gen["responseSchema"].(map[string]interface{})
	if responseSchema["type"] != "OBJECT" {
		t.Errorf("responseSchema type = %v, want OBJECT", responseSchema["type"])
	}
	summary := responseSchema["properties"].(map[string]interface{})["summary"].(map[string]interface{})
	if summary["type"] != "STRING" {
		t.Errorf("responseSchema nested type = %v, want STRING", summary["type"])
	}
	thinking := gen["thinkingConfig"].(map[string]interface{})
	if thinking["includeThoughts"] != true || thinking["thinkingBudget"] != float64(-1) {
		t.Errorf("thinkingConfig = %+v, want includeThoughts true and thinkingBudget -1", thinking)
	}
}

func TestGeminiAdapterBuildBodyFunctionResponseObjectHandling(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{name: "object unchanged", content: `{"temperature":18}`, want: `{"temperature":18}`},
		{name: "array wrapped", content: `[1,2]`, want: `{"result":[1,2]}`},
		{name: "plain text wrapped", content: `sunny`, want: `{"result":"sunny"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := buildBodyBytes(geminiAdapter{}, CompletionRequest{
				Messages: []Message{{Role: RoleTool, Name: "lookup", ToolCallID: "call_1", Content: tc.content}},
			})
			if err != nil {
				t.Fatalf("buildBody: %v", err)
			}
			var got struct {
				Contents []struct {
					Parts []struct {
						FunctionResponse struct {
							Response json.RawMessage `json:"response"`
						} `json:"functionResponse"`
					} `json:"parts"`
				} `json:"contents"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			if !jsonEqual(got.Contents[0].Parts[0].FunctionResponse.Response, []byte(tc.want)) {
				t.Errorf("response = %s, want %s", got.Contents[0].Parts[0].FunctionResponse.Response, tc.want)
			}
		})
	}
}

func TestGeminiAdapterVolatileTailMergesAfterToolResultIssue404(t *testing.T) {
	raw, err := buildBodyBytes(geminiAdapter{}, CompletionRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "stable system"},
			{Role: RoleUser, Content: "question"},
			{
				Role: RoleAssistant,
				ToolCalls: []ToolCall{{
					ID:       "call_lookup",
					Type:     "function",
					Function: FunctionCall{Name: "lookup", Arguments: `{"q":"x"}`},
				}},
			},
			{Role: RoleTool, Name: "lookup", ToolCallID: "call_lookup", Content: `{"answer":"42"}`},
			{Role: RoleUser, Content: "## Task checklist\n☐ verify", Volatile: true},
		},
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal body: %v\n%s", err, raw)
	}
	contents := got["contents"].([]interface{})
	last := contents[len(contents)-1].(map[string]interface{})
	if last["role"] != "user" {
		t.Fatalf("last content role = %v, want merged user turn", last["role"])
	}
	parts := last["parts"].([]interface{})
	if len(parts) != 2 {
		t.Fatalf("last user parts = %v, want functionResponse + volatile text", parts)
	}
	if _, ok := parts[0].(map[string]interface{})["functionResponse"].(map[string]interface{}); !ok {
		t.Fatalf("first merged part = %v, want functionResponse", parts[0])
	}
	if text := parts[1].(map[string]interface{})["text"]; text != "## Task checklist\n☐ verify" {
		t.Fatalf("second merged part text = %v, want volatile context", text)
	}
}

func TestGeminiAdapterBuildBodyToolChoiceModesAndParallelOverride(t *testing.T) {
	baseReq := func(choice ToolChoice, parallel *bool) CompletionRequest {
		return CompletionRequest{
			Messages: []Message{{Role: RoleUser, Content: "use a tool"}},
			Tools: []ToolDef{{
				Type: "function",
				Function: FunctionDef{
					Name:        "lookup",
					Description: "Look up data",
					Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
				},
			}},
			ToolChoice:        &choice,
			ParallelToolCalls: parallel,
		}
	}

	off := false
	cases := []struct {
		name        string
		req         CompletionRequest
		wantMode    string
		wantAllowed []interface{}
	}{
		{
			name:     "auto",
			req:      baseReq(ToolChoice{Mode: ToolChoiceAuto}, nil),
			wantMode: "AUTO",
		},
		{
			name:     "none",
			req:      baseReq(ToolChoice{Mode: ToolChoiceNone}, nil),
			wantMode: "NONE",
		},
		{
			name:     "required",
			req:      baseReq(ToolChoice{Mode: ToolChoiceRequired}, nil),
			wantMode: "ANY",
		},
		{
			// A gogent parallel-disable override (off) must NOT surface a
			// parallelFunctionCalls field — Vertex rejects it (see below).
			name:        "forced tool, parallel override ignored",
			req:         baseReq(ToolChoice{Mode: ToolChoiceTool, Name: "lookup"}, &off),
			wantMode:    "ANY",
			wantAllowed: []interface{}{"lookup"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := buildBodyBytes(geminiAdapter{}, tc.req)
			if err != nil {
				t.Fatalf("buildBody: %v", err)
			}
			var got map[string]interface{}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			cfg := got["toolConfig"].(map[string]interface{})["functionCallingConfig"].(map[string]interface{})
			if cfg["mode"] != tc.wantMode {
				t.Errorf("mode = %v, want %s", cfg["mode"], tc.wantMode)
			}
			// Vertex AI's functionCallingConfig has no parallelFunctionCalls field
			// and 400s on it, so gogent must never emit it.
			if _, ok := cfg["parallelFunctionCalls"]; ok {
				t.Errorf("parallelFunctionCalls present (%v); Vertex rejects this field", cfg["parallelFunctionCalls"])
			}
			if tc.wantAllowed == nil {
				if _, ok := cfg["allowedFunctionNames"]; ok {
					t.Errorf("allowedFunctionNames = %v, want omitted", cfg["allowedFunctionNames"])
				}
			} else if !jsonEqual(mustMarshal(t, cfg["allowedFunctionNames"]), mustMarshal(t, tc.wantAllowed)) {
				t.Errorf("allowedFunctionNames = %v, want %v", cfg["allowedFunctionNames"], tc.wantAllowed)
			}
		})
	}
}

func TestGeminiAdapterBuildBodyDropsNonObjectFunctionCallArgs(t *testing.T) {
	raw, err := buildBodyBytes(geminiAdapter{}, CompletionRequest{
		Messages: []Message{{
			Role: RoleAssistant,
			ToolCalls: []ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: FunctionCall{Name: "lookup", Arguments: `["not","object"]`},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	contents := got["contents"].([]interface{})
	fc := contents[0].(map[string]interface{})["parts"].([]interface{})[0].(map[string]interface{})["functionCall"].(map[string]interface{})
	if _, ok := fc["args"]; ok {
		t.Fatalf("functionCall args = %v, want omitted for non-object arguments", fc["args"])
	}
}

func TestGeminiAdapterParseResponseTextFunctionCallThoughtAndUsage(t *testing.T) {
	body := []byte(`{
		"candidates":[{
			"content":{"role":"model","parts":[
				{"text":"private reasoning","thought":true},
				{"text":"Visible "},
				{"text":"answer."},
				{"functionCall":{"name":"lookup","args":{"q":"vertex"},"id":"call_1"}}
			]},
			"finishReason":"STOP"
		}],
		"usageMetadata":{
			"promptTokenCount":5,
			"candidatesTokenCount":7,
			"totalTokenCount":15,
			"thoughtsTokenCount":3,
			"cachedContentTokenCount":2
		}
	}`)
	resp, err := (geminiAdapter{}).parseResponse(body)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if resp.Content != "Visible answer." {
		t.Errorf("Content = %q, want visible text without thought part", resp.Content)
	}
	if resp.Role != RoleAssistant {
		t.Errorf("Role = %q, want assistant", resp.Role)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "lookup" || tc.Function.Arguments != `{"q":"vertex"}` {
		t.Errorf("ToolCall = %+v, want lookup args and id", tc)
	}
	if resp.Usage == nil {
		t.Fatal("Usage = nil, want usageMetadata mapped")
	}
	if *resp.Usage != (TokenUsage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 15, ReasoningTokens: 3, Cache: CacheStats{ReadTokens: 2}}) {
		t.Errorf("Usage = %+v, want Gemini usage mapping", resp.Usage)
	}
}

func TestGeminiAdapterParseResponseMalformedJSONReturnsError(t *testing.T) {
	if _, err := (geminiAdapter{}).parseResponse([]byte(`{"candidates":[`)); err == nil {
		t.Fatal("parseResponse error = nil, want malformed JSON error")
	}
}

func TestGeminiAdapterParseResponseNoCandidatesReturnsError(t *testing.T) {
	if _, err := (geminiAdapter{}).parseResponse([]byte(`{"usageMetadata":{"promptTokenCount":1}}`)); err == nil {
		t.Fatal("parseResponse error = nil, want malformed response error for missing candidates")
	}
}

func TestGeminiAdapterParseResponseFunctionCallArgsMustBeObject(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":["not","object"],"id":"call_1"}}],"role":"model"},"finishReason":"STOP"}]}`)
	if _, err := (geminiAdapter{}).parseResponse(body); err == nil {
		t.Fatal("parseResponse error = nil, want malformed response error for non-object functionCall args")
	}
}

func TestGeminiAdapterParseStreamSSEThoughtTextFunctionCallAndUsage(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"thinking","thought":true}],"role":"model"}}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"text":"Hello "},{"text":"world"}],"role":"model"}}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"vertex"},"id":"call_1"}}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":4,"totalTokenCount":9,"thoughtsTokenCount":3}}`,
		``,
	}, "\n"))
	ch := make(chan StreamResponse, 10)
	content, usage, err := (geminiAdapter{}).parseStream(stream, ch)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	close(ch)

	if content != "Hello world" {
		t.Errorf("content = %q, want Hello world", content)
	}
	if usage == nil || *usage != (TokenUsage{PromptTokens: 2, CompletionTokens: 4, TotalTokens: 9, ReasoningTokens: 3}) {
		t.Errorf("usage = %+v, want final usageMetadata", usage)
	}

	var events []StreamResponse
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) != 4 {
		t.Fatalf("events len = %d, want 4: %+v", len(events), events)
	}
	if events[0].Reasoning != "thinking" || events[0].Role != RoleAssistant {
		t.Errorf("reasoning event = %+v", events[0])
	}
	if events[1].Content != "Hello " || events[2].Content != "world" {
		t.Errorf("content events = %+v, %+v", events[1], events[2])
	}
	done := events[3]
	if !done.Done {
		t.Fatalf("terminal event = %+v, want Done", done)
	}
	if done.FinishReason == nil || *done.FinishReason != "tool_calls" {
		t.Errorf("terminal finish = %v, want tool_calls", done.FinishReason)
	}
	if len(done.ToolCalls) != 1 || done.ToolCalls[0].Function.Name != "lookup" || done.ToolCalls[0].Function.Arguments != `{"q":"vertex"}` {
		t.Errorf("terminal tool calls = %+v", done.ToolCalls)
	}
	if done.Usage == nil || *done.Usage != *usage {
		t.Errorf("terminal usage = %+v, want %+v", done.Usage, usage)
	}
}

func TestGeminiAdapterParseStreamMalformedChunkReturnsError(t *testing.T) {
	ch := make(chan StreamResponse, 10)
	content, usage, err := (geminiAdapter{}).parseStream(strings.NewReader("data: {not-json}\n\n"), ch)
	if err == nil {
		t.Fatalf("parseStream error = nil, want malformed SSE JSON error; content=%q usage=%+v", content, usage)
	}
}

func TestGeminiAdapterParseStreamFunctionCallArgsMustBeObject(t *testing.T) {
	ch := make(chan StreamResponse, 10)
	stream := strings.NewReader(`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":["not","object"],"id":"call_1"}}],"role":"model"},"finishReason":"STOP"}]}` + "\n\n")
	content, usage, err := (geminiAdapter{}).parseStream(stream, ch)
	if err == nil {
		t.Fatalf("parseStream error = nil, want malformed functionCall args error; content=%q usage=%+v", content, usage)
	}
}

func TestVertexNativeConnectionCompleteAndStreamUseNativeURLsAndADC(t *testing.T) {
	withFakeADCTokenSource(t, func(ctx context.Context, scopes ...string) (oauth2.TokenSource, error) {
		return &staticTokenSource{token: "native-token"}, nil
	})

	var blockingPath, blockingRawBody, blockingAuth string
	var streamPath, streamQuery, streamRawBody, streamAuth, streamAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			blockingPath = r.URL.Path
			blockingRawBody = string(body)
			blockingAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
		case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
			streamPath = r.URL.Path
			streamQuery = r.URL.RawQuery
			streamRawBody = string(body)
			streamAuth = r.Header.Get("Authorization")
			streamAccept = r.Header.Get("Accept")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"stream ok\"}],\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":2,\"totalTokenCount\":3}}\n\n"))
		default:
			t.Fatalf("unexpected path %s", r.URL.String())
		}
	}))
	defer server.Close()

	conn := NewModelConnection(
		&config.ProviderConnection{
			APIType:  "vertex-native",
			Endpoint: server.URL,
			Project:  "ignored",
			Location: "ignored",
		},
		&config.ModelConfig{
			Model:       "gemini-2.5-flash",
			Temperature: 0.2,
			MaxTokens:   64,
		},
	)
	resp, err := conn.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "ok" || resp.FinishReason != "stop" {
		t.Errorf("blocking response = %+v", resp)
	}
	wantBlockingPath := "/publishers/google/models/gemini-2.5-flash:generateContent"
	if blockingPath != wantBlockingPath {
		t.Errorf("blocking path = %q, want %q", blockingPath, wantBlockingPath)
	}
	if blockingAuth != "Bearer native-token" {
		t.Errorf("blocking auth = %q, want ADC bearer", blockingAuth)
	}
	assertNativeBodyHasNoModel(t, []byte(blockingRawBody))

	streamResp, err := conn.CompleteWithToolsStreamCtx(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("CompleteWithToolsStreamCtx: %v", err)
	}
	if streamResp.Content != "stream ok" || streamResp.FinishReason != "stop" {
		t.Errorf("stream response = %+v", streamResp)
	}
	wantStreamPath := "/publishers/google/models/gemini-2.5-flash:streamGenerateContent"
	if streamPath != wantStreamPath {
		t.Errorf("stream path = %q, want %q", streamPath, wantStreamPath)
	}
	if streamQuery != "alt=sse" {
		t.Errorf("stream query = %q, want alt=sse", streamQuery)
	}
	if streamAuth != "Bearer native-token" {
		t.Errorf("stream auth = %q, want ADC bearer", streamAuth)
	}
	if streamAccept != "text/event-stream" {
		t.Errorf("stream Accept = %q, want text/event-stream", streamAccept)
	}
	assertNativeBodyHasNoModel(t, []byte(streamRawBody))
}

func jsonEqual(a, b []byte) bool {
	var av, bv interface{}
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	aa, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return bytes.Equal(aa, bb)
}

func mustMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return b
}

func assertNativeBodyHasNoModel(t *testing.T, raw []byte) {
	t.Helper()
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal native body: %v\n%s", err, raw)
	}
	if _, ok := got["model"]; ok {
		t.Fatalf("native Gemini request body contains model field: %s", raw)
	}
	if _, ok := got["contents"]; !ok {
		t.Fatalf("native Gemini request body missing contents: %s", raw)
	}
}
