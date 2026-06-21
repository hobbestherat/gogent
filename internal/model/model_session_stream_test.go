package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// nonStreamingConnector is a fake Connector that does NOT implement
// StreamingToolCompleter. It records whether the blocking tool path was used so
// SendWithToolsStreamCtx can be shown to fall back to it (rather than panic on
// a missing streaming capability) when a sink is requested but the backend
// cannot stream.
type nonStreamingConnector struct {
	content        string
	toolCalls      []ToolCall
	blockingCalled bool
}

func (n *nonStreamingConnector) Complete(messages []Message) (*CompletionResponse, error) {
	return &CompletionResponse{Content: n.content, Role: RoleAssistant}, nil
}

func (n *nonStreamingConnector) CompleteWithTools(messages []Message, tools []ToolDef) (*CompletionResponse, error) {
	n.blockingCalled = true
	return &CompletionResponse{Content: n.content, Role: RoleAssistant, ToolCalls: n.toolCalls}, nil
}

func (n *nonStreamingConnector) CompleteWithToolsCtx(ctx context.Context, messages []Message, tools []ToolDef) (*CompletionResponse, error) {
	n.blockingCalled = true
	return &CompletionResponse{Content: n.content, Role: RoleAssistant, ToolCalls: n.toolCalls}, nil
}

func (n *nonStreamingConnector) CompleteStream(messages []Message) (<-chan StreamResponse, <-chan error) {
	ch := make(chan StreamResponse)
	errCh := make(chan error)
	close(ch)
	close(errCh)
	return ch, errCh
}

func (n *nonStreamingConnector) GetStats() *ModelStats { return &ModelStats{} }

func (n *nonStreamingConnector) StatsSnapshot() StatsSnapshot { return StatsSnapshot{} }

// Compile-time check: it satisfies Connector but NOT StreamingToolCompleter.
var (
	_ Connector = (*nonStreamingConnector)(nil)
)

// TestSendWithToolsStreamCtxStreamingForwardsReasoning drives SendWithToolsStreamCtx
// through a real *ModelConnection against a reasoning SSE server and verifies the
// reasoning reaches the sink while the transcript, response content and usage are
// recorded exactly like the blocking path.
func TestSendWithToolsStreamCtxStreamingForwardsReasoning(t *testing.T) {
	server := reasoningServer(t, reasoningContentSSE, nil)
	conn := NewModelConnection()
	conn.SetURL(server.URL)
	sess := NewModelSession("s1", conn)

	var sink []string
	resp, err := sess.SendWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil,
		func(delta string) { sink = append(sink, delta) })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(sink, ""); got != "Let me think about this.\nDone thinking." {
		t.Errorf("sink = %q, want the reasoning chain", got)
	}
	if resp.Content != "Hello" {
		t.Errorf("resp.Content = %q, want Hello", resp.Content)
	}

	// The assistant turn must be appended to the transcript (parity with the
	// blocking path's bookkeeping).
	tt := sess.GetTranscript()
	var sawAssistant bool
	for _, m := range tt {
		if m.Role == RoleAssistant && m.Content == "Hello" {
			sawAssistant = true
		}
	}
	if !sawAssistant {
		t.Errorf("assistant turn not appended to transcript: %+v", tt)
	}
}

// TestSendWithToolsStreamCtxNilSinkStillWorks asserts that a nil sink selects
// the blocking path (byte-for-byte the prior behaviour): it produces a fully
// assembled response and does not require the backend to stream. The blocking
// path speaks a single JSON response, so the server here serves JSON (not SSE).
func TestSendWithToolsStreamCtxNilSinkStillWorks(t *testing.T) {
	server := blockingJSONServer(t, `{"choices":[{"index":0,"message":{"role":"assistant","content":"Hello, world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	conn := NewModelConnection()
	conn.SetURL(server.URL)
	sess := NewModelSession("nil-sink", conn)

	resp, err := sess.SendWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello, world" {
		t.Errorf("resp.Content = %q, want Hello, world", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("resp.FinishReason = %q, want stop", resp.FinishReason)
	}
	if sess.CurrentTokenCount != 7 {
		t.Errorf("CurrentTokenCount = %d, want 7", sess.CurrentTokenCount)
	}
}

// blockingJSONServer serves a single JSON (non-streaming) completion response —
// the shape the blocking complete() path parses.
func blockingJSONServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestSendWithToolsStreamCtxFallsBackWhenBackendCannotStream: when the backend
// does not implement StreamingToolCompleter, a non-nil sink must NOT panic or
// error — sendCtx falls back to the blocking CompleteWithToolsCtx, and the sink
// is simply never invoked (a non-streaming backend has no reasoning to surface).
func TestSendWithToolsStreamCtxFallsBackWhenBackendCannotStream(t *testing.T) {
	conn := &nonStreamingConnector{content: "answer"}
	sess := NewModelSession("fb", conn)

	sinkCalled := false
	resp, err := sess.SendWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil,
		func(string) { sinkCalled = true })
	if err != nil {
		t.Fatalf("fallback path errored: %v", err)
	}
	if !conn.blockingCalled {
		t.Error("expected fallback to the blocking CompleteWithToolsCtx, but it was not called")
	}
	if sinkCalled {
		t.Error("sink must not be invoked when the backend cannot stream reasoning")
	}
	if resp.Content != "answer" {
		t.Errorf("resp.Content = %q, want answer", resp.Content)
	}
}

// TestSendWithToolsStreamCtxRecordsUsage verifies token accounting still runs on
// the streaming path (CurrentTokenCount is updated from usage), matching the
// blocking path — so enabling live thinking does not lose token stats.
func TestSendWithToolsStreamCtxRecordsUsage(t *testing.T) {
	server := reasoningServer(t, reasoningContentSSE, nil)
	conn := NewModelConnection()
	conn.SetURL(server.URL)
	sess := NewModelSession("usage", conn)

	if _, err := sess.SendWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil, func(string) {}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess.CurrentTokenCount != 14 {
		t.Errorf("CurrentTokenCount = %d, want 14 (usage total must be recorded on the stream path)", sess.CurrentTokenCount)
	}
}
