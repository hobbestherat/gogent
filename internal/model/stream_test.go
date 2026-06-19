package model

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogent/internal/config"
)

// newStreamConfig builds a minimal config pointed at a test server with an API
// key, so the connection installs the APIKeyRoundTripper used by the auth tests.
func newStreamConfig(endpoint, apiKey string) *config.ModelConfig {
	return &config.ModelConfig{
		APIType:  "openai",
		Endpoint: endpoint,
		Model:    "test-model",
		APIKey:   apiKey,
	}
}

// sseServer replays a fixed SSE body as a 200 text/event-stream response.
func sseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// drain collects every event off the streaming channels into the assembled
// content deltas and the single terminal event.
func drain(t *testing.T, streamCh <-chan StreamResponse, errCh <-chan error) (deltas []string, terminal StreamResponse, err error) {
	t.Helper()
	for ev := range streamCh {
		if ev.Done {
			terminal = ev
			continue
		}
		deltas = append(deltas, ev.Content)
	}
	// errCh is buffered and closed after streamCh; a single read is enough.
	if e, ok := <-errCh; ok {
		err = e
	}
	return deltas, terminal, err
}

const contentSSE = `data: {"choices":[{"delta":{"role":"assistant","content":"Hel"},"index":0}]}

data: {"choices":[{"delta":{"content":"lo, "},"index":0}]}

data: {"choices":[{"delta":{"content":"world"},"index":0,"finish_reason":null}]}

data: {"choices":[{"delta":{},"index":0,"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}

data: [DONE]

`

func TestCompleteStreamContent(t *testing.T) {
	server := sseServer(t, contentSSE)
	c := NewModelConnection()
	c.SetURL(server.URL)

	streamCh, errCh := c.CompleteStream([]Message{{Role: RoleUser, Content: "hi"}})
	deltas, terminal, err := drain(t, streamCh, errCh)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := strings.Join(deltas, ""); got != "Hello, world" {
		t.Errorf("assembled content = %q, want %q", got, "Hello, world")
	}
	if len(deltas) != 3 {
		t.Errorf("expected 3 content deltas, got %d (%q)", len(deltas), deltas)
	}
	if terminal.FinishReason == nil || *terminal.FinishReason != "stop" {
		t.Errorf("terminal finish reason = %v, want stop", terminal.FinishReason)
	}
	if terminal.Usage == nil {
		t.Fatal("expected usage in terminal event")
	}
	if terminal.Usage.PromptTokens != 11 || terminal.Usage.CompletionTokens != 3 {
		t.Errorf("usage = %+v, want prompt 11 / completion 3", terminal.Usage)
	}

	// The trailing usage chunk must reach the connector stats (not be dropped at
	// the first finish_reason).
	stats := c.StatsSnapshot()
	if stats.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", stats.SuccessCount)
	}
	if stats.TotalTokensIn != 11 || stats.TotalTokensOut != 3 {
		t.Errorf("stats tokens in/out = %d/%d, want 11/3", stats.TotalTokensIn, stats.TotalTokensOut)
	}
}

// TestCompleteStreamRequestShape asserts the streamed request reuses the
// round-tripper (Authorization header present) and asks for usage.
func TestCompleteStreamRequestShape(t *testing.T) {
	var auth, accept, contentType string
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		accept = r.Header.Get("Accept")
		contentType = r.Header.Get("Content-Type")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		body = buf
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(contentSSE))
	}))
	defer server.Close()

	c := NewModelConnectionFromConfig(newStreamConfig(server.URL, "secret-key"))
	c.SetURL(server.URL)

	streamCh, errCh := c.CompleteStream([]Message{{Role: RoleUser, Content: "hi"}})
	if _, _, err := drain(t, streamCh, errCh); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if auth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer secret-key")
	}
	if accept != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", accept)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
	if !strings.Contains(string(body), `"stream":true`) {
		t.Errorf("request body missing stream:true: %s", body)
	}
	if !strings.Contains(string(body), `"include_usage":true`) {
		t.Errorf("request body missing stream_options.include_usage: %s", body)
	}
}

func TestCompleteStreamToolCalls(t *testing.T) {
	// Tool call streamed as fragments across chunks, correlated by index. The
	// second tool call omits its id (vLLM behaviour) to exercise id synthesis.
	const toolSSE = `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"read","arguments":""}}]},"index":0}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]},"index":0}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]},"index":0}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"calc","arguments":"{\"expression\":\"1+1\"}"}}]},"index":0}]}

data: {"choices":[{"delta":{},"index":0,"finish_reason":"tool_calls"}]}

data: [DONE]

`
	server := sseServer(t, toolSSE)
	c := NewModelConnection()
	c.SetURL(server.URL)

	streamCh, errCh := c.CompleteStream([]Message{{Role: RoleUser, Content: "go"}})
	deltas, terminal, err := drain(t, streamCh, errCh)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deltas) != 0 {
		t.Errorf("expected no content deltas, got %q", deltas)
	}
	if terminal.FinishReason == nil || *terminal.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %v, want tool_calls", terminal.FinishReason)
	}
	if len(terminal.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %+v", len(terminal.ToolCalls), terminal.ToolCalls)
	}

	first := terminal.ToolCalls[0]
	if first.ID != "call_abc" || first.Function.Name != "read" {
		t.Errorf("first tool call = %+v, want id call_abc name read", first)
	}
	if first.Function.Arguments != `{"path":"a.txt"}` {
		t.Errorf("first tool call args = %q, want %q", first.Function.Arguments, `{"path":"a.txt"}`)
	}
	if first.Type != "function" {
		t.Errorf("first tool call type = %q, want function", first.Type)
	}

	second := terminal.ToolCalls[1]
	if second.ID != "call_1" { // synthesized from index, since id was omitted
		t.Errorf("second tool call id = %q, want synthesized call_1", second.ID)
	}
	if second.Function.Name != "calc" || second.Function.Arguments != `{"expression":"1+1"}` {
		t.Errorf("second tool call = %+v", second)
	}
}

// TestCompleteStreamLargeLine verifies a single SSE line far larger than the old
// 64 KB bufio.Scanner token cap is read intact rather than truncated.
func TestCompleteStreamLargeLine(t *testing.T) {
	big := strings.Repeat("x", 200*1024) // 200 KB, well past the Scanner default
	body := `data: {"choices":[{"delta":{"content":"` + big + `"},"index":0,"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	server := sseServer(t, body)
	c := NewModelConnection()
	c.SetURL(server.URL)

	streamCh, errCh := c.CompleteStream([]Message{{Role: RoleUser, Content: "hi"}})
	deltas, _, err := drain(t, streamCh, errCh)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(deltas, ""); got != big {
		t.Errorf("large content truncated: got %d bytes, want %d", len(got), len(big))
	}
}

func TestCompleteStreamHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	c := NewModelConnection()
	c.SetURL(server.URL)

	streamCh, errCh := c.CompleteStream([]Message{{Role: RoleUser, Content: "hi"}})
	_, _, err := drain(t, streamCh, errCh)
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	modelErr, ok := err.(*ModelError)
	if !ok {
		t.Fatalf("expected *ModelError, got %T", err)
	}
	if modelErr.HTTPStatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", modelErr.HTTPStatusCode)
	}
}
