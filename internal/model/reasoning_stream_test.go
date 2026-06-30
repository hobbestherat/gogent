package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gogent/internal/config"
)

// reasoningEvent partitions a stream into its reasoning deltas, content deltas
// and the single terminal event, preserving arrival order within each bucket.
// Unlike stream_test.go's drain (which keeps only content), this keeps the
// Reasoning channel so the streaming-thinking behaviour (issue #217) can be
// asserted.
func reasoningEvent(t *testing.T, streamCh <-chan StreamResponse, errCh <-chan error) (reasoning, content []string, terminal StreamResponse, err error) {
	t.Helper()
	for ev := range streamCh {
		if ev.Done {
			terminal = ev
			continue
		}
		if ev.Reasoning != "" {
			reasoning = append(reasoning, ev.Reasoning)
		}
		if ev.Content != "" {
			content = append(content, ev.Content)
		}
	}
	if e, ok := <-errCh; ok {
		err = e
	}
	return reasoning, content, terminal, err
}

// runParser invokes parseOpenAIStream on body in a goroutine (mirroring how
// completeStream drives it), draining the resulting events.
func runParser(t *testing.T, body string) (reasoning, content []string, terminal StreamResponse) {
	t.Helper()
	streamCh := make(chan StreamResponse, 256)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = parseOpenAIStream(strings.NewReader(body), streamCh)
		close(streamCh)
	}()
	for ev := range streamCh {
		if ev.Done {
			terminal = ev
			continue
		}
		if ev.Reasoning != "" {
			reasoning = append(reasoning, ev.Reasoning)
		}
		if ev.Content != "" {
			content = append(content, ev.Content)
		}
	}
	<-done
	return reasoning, content, terminal
}

// reasoningContentSSE interleaves reasoning_content (Z.AI/GLM, DeepSeek) deltas
// with visible content and a tool call, so a single stream exercises the
// reasoning side channel, the answer channel and tool assembly together.
const reasoningContentSSE = `data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"Let me "},"index":0}]}

data: {"choices":[{"delta":{"reasoning_content":"think about this.\n"},"index":0}]}

data: {"choices":[{"delta":{"content":"Hel"},"index":0}]}

data: {"choices":[{"delta":{"content":"lo"},"index":0}]}

data: {"choices":[{"delta":{"reasoning_content":"Done thinking."},"index":0}]}

data: {"choices":[{"delta":{},"index":0,"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}

data: [DONE]

`

// TestParseOpenAIStreamReasoningContent verifies the GLM/Z.AI reasoning side
// channel (reasoning_content) is surfaced as separate Reasoning deltas, in
// arrival order, interleaved with — and distinct from — the visible content.
func TestParseOpenAIStreamReasoningContent(t *testing.T) {
	reasoning, content, terminal := runParser(t, reasoningContentSSE)

	if got := strings.Join(reasoning, ""); got != "Let me think about this.\nDone thinking." {
		t.Errorf("reasoning deltas = %q, want concatenated chain-of-thought", got)
	}
	if got := strings.Join(content, ""); got != "Hello" {
		t.Errorf("content deltas = %q, want %q (reasoning must stay out of the answer)", got, "Hello")
	}
	if terminal.FinishReason == nil || *terminal.FinishReason != "stop" {
		t.Errorf("terminal finish = %v, want stop", terminal.FinishReason)
	}
	if terminal.Usage == nil || terminal.Usage.PromptTokens != 11 {
		t.Errorf("terminal usage = %+v, want prompt 11", terminal.Usage)
	}
}

// TestParseOpenAIStreamReasoningField covers the OpenRouter field name
// (`reasoning` rather than `reasoning_content`) — the alternative spelling the
// parser reads as the same side channel.
func TestParseOpenAIStreamReasoningField(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"reasoning":"step one"},"index":0}]}

data: {"choices":[{"delta":{"content":"answer"},"index":0,"finish_reason":"stop"}]}

data: [DONE]

`
	reasoning, content, _ := runParser(t, sse)
	if got := strings.Join(reasoning, ""); got != "step one" {
		t.Errorf("reasoning (openrouter spelling) = %q, want %q", got, "step one")
	}
	if got := strings.Join(content, ""); got != "answer" {
		t.Errorf("content = %q, want %q", got, "answer")
	}
}

// TestParseOpenAIStreamReasoningContentPreferredOverReasoning documents the
// tie-break when a (pathological) delta sets both fields: reasoning_content wins.
func TestParseOpenAIStreamReasoningContentPreferredOverReasoning(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"reasoning_content":"from_content","reasoning":"from_plain"},"index":0,"finish_reason":"stop"}]}

data: [DONE]

`
	reasoning, _, _ := runParser(t, sse)
	if len(reasoning) != 1 || reasoning[0] != "from_content" {
		t.Errorf("reasoning = %q, want single %q (reasoning_content preferred)", reasoning, "from_content")
	}
}

// TestParseOpenAIStreamNoReasoningIsNoOp verifies a stream that carries no
// reasoning at all yields zero reasoning deltas — the no-op case for
// non-thinking models.
func TestParseOpenAIStreamNoReasoningIsNoOp(t *testing.T) {
	reasoning, content, _ := runParser(t, contentSSE)
	if len(reasoning) != 0 {
		t.Errorf("non-thinking stream emitted reasoning deltas %q, want none", reasoning)
	}
	if got := strings.Join(content, ""); got != "Hello, world" {
		t.Errorf("content = %q, want %q", got, "Hello, world")
	}
}

// TestStreamDeltaDecodesBothReasoningFields is a focused JSON-decode check for
// streamDelta: reasoning_content and reasoning both unmarshal, independently.
func TestStreamDeltaDecodesBothReasoningFields(t *testing.T) {
	cases := []struct {
		name, body, wantReasoningContent, wantReasoning string
	}{
		{"reasoning_content", `{"reasoning_content":"rc"}`, "rc", ""},
		{"reasoning", `{"reasoning":"r"}`, "", "r"},
		{"neither", `{"content":"c"}`, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var d streamDelta
			if err := json.Unmarshal([]byte(tc.body), &d); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if d.ReasoningContent != tc.wantReasoningContent {
				t.Errorf("ReasoningContent = %q, want %q", d.ReasoningContent, tc.wantReasoningContent)
			}
			if d.Reasoning != tc.wantReasoning {
				t.Errorf("Reasoning = %q, want %q", d.Reasoning, tc.wantReasoning)
			}
		})
	}
}

// --- CompleteWithToolsStreamCtx: the new streaming tool-calling entry point ---

// reasoningServer serves an SSE body and records whether the request advertised
// tools / asked for a stream, so the streaming-thinking path can be asserted
// against the same wire shape as the blocking path.
func reasoningServer(t *testing.T, body string, capture func(reqBody []byte)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		if capture != nil {
			capture(buf)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestCompleteWithToolsStreamCtxReasoning end-to-end: reasoning deltas flow to
// the sink in arrival order while content, tool calls, finish reason and usage
// are assembled into the returned response exactly like the blocking path.
func TestCompleteWithToolsStreamCtxReasoning(t *testing.T) {
	server := reasoningServer(t, reasoningContentSSE, nil)
	c := newPlaceholderConnection()
	c.SetURL(server.URL)

	var sink []string
	resp, err := c.CompleteWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}},
		[]ToolDef{{Type: "function", Function: FunctionDef{Name: "calc"}}},
		func(delta string) { sink = append(sink, delta) },
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(sink, ""); got != "Let me think about this.\nDone thinking." {
		t.Errorf("sink received %q, want the full reasoning chain", got)
	}
	if resp.Content != "Hello" {
		t.Errorf("resp.Content = %q, want %q", resp.Content, "Hello")
	}
	if resp.FinishReason != "stop" {
		t.Errorf("resp.FinishReason = %q, want stop", resp.FinishReason)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 3 {
		t.Errorf("resp.Usage = %+v, want prompt 11 / completion 3", resp.Usage)
	}
	if resp.Role != RoleAssistant {
		t.Errorf("resp.Role = %q, want assistant", resp.Role)
	}
}

// TestCompleteWithToolsStreamCtxAssemblesToolCalls verifies native tool calls
// streamed as fragments still assemble on the streaming path (parity with the
// blocking path's tool handling).
func TestCompleteWithToolsStreamCtxAssemblesToolCalls(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"planning"},"index":0}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"calc","arguments":""}}]},"index":0}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"expression\":\"2+2\"}"}}]},"index":0}]}

data: {"choices":[{"delta":{},"index":0,"finish_reason":"tool_calls"}]}

data: [DONE]

`
	server := reasoningServer(t, sse, nil)
	c := newPlaceholderConnection()
	c.SetURL(server.URL)

	var sinkCalls int
	resp, err := c.CompleteWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "go"}},
		[]ToolDef{{Type: "function", Function: FunctionDef{Name: "calc"}}},
		func(string) { sinkCalls++ },
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sinkCalls != 1 {
		t.Errorf("sink called %d times, want 1", sinkCalls)
	}
	if resp.Content != "" {
		t.Errorf("resp.Content = %q, want empty (tool-call turn)", resp.Content)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("resp.FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("resp.ToolCalls = %d, want 1: %+v", len(resp.ToolCalls), resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "calc" || tc.Function.Arguments != `{"expression":"2+2"}` {
		t.Errorf("assembled tool call = %+v", tc)
	}
}

// TestCompleteWithToolsStreamCtxNilSinkDiscardsReasoning: a nil sink must still
// produce the fully assembled response (a plain streamed completion) and never
// panic.
func TestCompleteWithToolsStreamCtxNilSinkDiscardsReasoning(t *testing.T) {
	server := reasoningServer(t, reasoningContentSSE, nil)
	c := newPlaceholderConnection()
	c.SetURL(server.URL)

	resp, err := c.CompleteWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello" {
		t.Errorf("resp.Content = %q, want Hello (nil sink must not lose content)", resp.Content)
	}
}

// TestCompleteWithToolsStreamCtxNoReasoningModelUnaffected: a backend/turn that
// streams no reasoning never invokes the sink, yet the answer is correct.
func TestCompleteWithToolsStreamCtxNoReasoningModelUnaffected(t *testing.T) {
	server := sseServer(t, contentSSE)
	c := newPlaceholderConnection()
	c.SetURL(server.URL)

	called := false
	resp, err := c.CompleteWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil,
		func(string) { called = true })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("sink was invoked for a non-reasoning stream; non-thinking models must be unaffected")
	}
	if resp.Content != "Hello, world" {
		t.Errorf("resp.Content = %q, want Hello, world", resp.Content)
	}
}

// TestCompleteWithToolsStreamCtxHTTPError: a non-200 response surfaces as an
// error, and the sink is never invoked (the error happens before any stream
// event is emitted).
func TestCompleteWithToolsStreamCtxHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	c := newPlaceholderConnection()
	c.SetURL(server.URL)

	called := false
	resp, err := c.CompleteWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil, func(string) { called = true })
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if resp != nil {
		t.Errorf("expected nil response on error, got %+v", resp)
	}
	if called {
		t.Error("sink must not be invoked when the request errors before streaming")
	}
	modelErr, ok := err.(*ModelError)
	if !ok {
		t.Fatalf("expected *ModelError, got %T", err)
	}
	if modelErr.HTTPStatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", modelErr.HTTPStatusCode)
	}
}

// TestCompleteWithToolsStreamCtxCancelledContext: a cancelled context aborts
// the streamed request (the underlying HTTP request is bound to ctx), returning
// an error rather than hanging.
func TestCompleteWithToolsStreamCtxCancelledContext(t *testing.T) {
	// A server that never responds, so the only way out is context cancellation.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	c := newPlaceholderConnection()
	c.SetURL(server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before we send

	done := make(chan error, 1)
	go func() {
		_, err := c.CompleteWithToolsStreamCtx(ctx,
			[]Message{{Role: RoleUser, Content: "hi"}}, nil, func(string) {})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected error from cancelled streaming request, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CompleteWithToolsStreamCtx hung on a cancelled context")
	}
}

// TestStreamingToolCompleterImplemented: *ModelConnection must satisfy the
// optional StreamingToolCompleter capability so ModelSession can type-assert to
// it (issue #217).
func TestStreamingToolCompleterImplemented(t *testing.T) {
	var _ StreamingToolCompleter = (*ModelConnection)(nil)
	if _, ok := any(newPlaceholderConnection()).(StreamingToolCompleter); !ok {
		t.Error("*ModelConnection does not implement StreamingToolCompleter")
	}
}

// TestCompleteWithToolsStreamCtxContentParity asserts the streaming path
// rebuilds content from deltas identically to what the parser assembled — i.e.
// nothing is dropped or doubled between the parser's content builder and the
// delta-forwarding path.
func TestCompleteWithToolsStreamCtxContentParity(t *testing.T) {
	// Mixed reasoning/content with a large content payload.
	big := strings.Repeat("ab", 4096)
	body := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"r\"},\"index\":0}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"" + big + "\"},\"index\":0,\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	server := reasoningServer(t, body, nil)
	c := newPlaceholderConnection()
	c.SetURL(server.URL)

	resp, err := c.CompleteWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil, func(string) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != big {
		t.Errorf("streamed content length = %d, want %d (delta reassembly diverged)", len(resp.Content), len(big))
	}
}

// TestCompleteWithToolsStreamCtxDoesNotRetry documents a real behavioural
// difference from the blocking path (issue #217): the blocking complete() retries
// transient failures (429/5xx) with backoff, but a streamed response cannot be
// safely replayed mid-stream, so CompleteWithToolsStreamCtx makes a single
// attempt. Enabling stream_thinking therefore changes a session's failure
// semantics on transient errors — this test pins that so a later change is
// intentional.
func TestCompleteWithToolsStreamCtxDoesNotRetry(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		// A retrying client would succeed here on the second attempt.
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(reasoningContentSSE))
	}))
	defer server.Close()

	c := newPlaceholderConnection()
	c.SetURL(server.URL)
	// Keep any backoff negligible so the test does not wait on retry delays even
	// if a retry were (incorrectly) attempted.
	c.retryBaseDelay = 0
	c.retryMaxDelay = 0

	_, err := c.CompleteWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil, func(string) {})
	if err == nil {
		t.Fatal("expected the 429 to surface as an error, got nil")
	}
	if calls != 1 {
		t.Errorf("streaming path made %d requests, want 1 (it must not retry transient failures)", calls)
	}
}

// --- Anthropic extended-thinking (thinking_delta) ---

// anthropicThinkingSSE is an Anthropic SSE stream that emits a thinking_delta
// block (extended thinking) alongside a text block.
const anthropicThinkingSSE = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Reasoning "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Answer"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

`

// TestAnthropicStreamThinkingDelta verifies Anthropic extended-thinking deltas
// are surfaced as Reasoning events, kept out of the visible content, on the
// streaming path.
func TestAnthropicStreamThinkingDelta(t *testing.T) {
	server := sseServer(t, anthropicThinkingSSE)
	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "anthropic", Endpoint: server.URL, APIKey: "k"},
		&config.ModelConfig{Model: "claude-sonnet-4-6"},
	)

	streamCh, errCh := conn.CompleteStream([]Message{{Role: RoleUser, Content: "hi"}})
	reasoning, content, terminal, err := reasoningEvent(t, streamCh, errCh)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if got := strings.Join(reasoning, ""); got != "Reasoning step." {
		t.Errorf("anthropic reasoning deltas = %q, want %q", got, "Reasoning step.")
	}
	if got := strings.Join(content, ""); got != "Answer" {
		t.Errorf("anthropic content = %q, want %q (thinking must stay out of the answer)", got, "Answer")
	}
	// Anthropic maps end_turn onto the OpenAI-style "stop" finish reason.
	if terminal.FinishReason == nil || *terminal.FinishReason != "stop" {
		got := "<nil>"
		if terminal.FinishReason != nil {
			got = *terminal.FinishReason
		}
		t.Errorf("finish = %s, want stop (end_turn mapped)", got)
	}
}

// TestAnthropicCompleteWithToolsStreamCtxThinkingDelta runs the same Anthropic
// thinking stream through CompleteWithToolsStreamCtx so the reasoning reaches
// the sink and content reaches the response.
func TestAnthropicCompleteWithToolsStreamCtxThinkingDelta(t *testing.T) {
	server := sseServer(t, anthropicThinkingSSE)
	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "anthropic", Endpoint: server.URL, APIKey: "k"},
		&config.ModelConfig{Model: "claude-sonnet-4-6"},
	)

	var sink []string
	resp, err := conn.CompleteWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil,
		func(delta string) { sink = append(sink, delta) })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(sink, ""); got != "Reasoning step." {
		t.Errorf("sink = %q, want the anthropic thinking chain", got)
	}
	if resp.Content != "Answer" {
		t.Errorf("resp.Content = %q, want Answer", resp.Content)
	}
}

// --- Round 2: validate the driver's panic-recovery + errCh-close fix ---

// TestCompleteWithToolsStreamCtxRequestShape pins the wire contract for the
// thinking path against real backends (the round-1 SSE servers replay fixed
// bodies and ignore the request, so without this a buildRequest change could
// silently drop usage on the streaming path and no test would fail). It asserts
// the streamed request asks to stream, asks for usage, and advertises the tool.
func TestCompleteWithToolsStreamCtxRequestShape(t *testing.T) {
	var captured []byte
	server := reasoningServer(t, reasoningContentSSE, func(b []byte) { captured = b })
	c := newPlaceholderConnection()
	c.SetURL(server.URL)

	if _, err := c.CompleteWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}},
		[]ToolDef{{Type: "function", Function: FunctionDef{Name: "calc"}}},
		func(string) {}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := string(captured)
	if !strings.Contains(body, `"stream":true`) {
		t.Errorf("stream request missing stream:true: %s", body)
	}
	if !strings.Contains(body, `"include_usage":true`) {
		t.Errorf("stream request missing include_usage (token stats would be lost on real backends): %s", body)
	}
	if !strings.Contains(body, `"calc"`) {
		t.Errorf("stream request did not advertise the tool: %s", body)
	}
}

// TestCompleteWithToolsStreamCtxRecoversPanic validates the driver's round-1
// fix: completeStream runs on a separate goroutine outside runLoop's recover, so
// a panic during stream parsing must be contained and surfaced as a *ModelError
// rather than crashing the process. A nil Stats forces a nil-deref panic inside
// completeStream (at Stats.Mutex.Lock), exercising the recover path.
func TestCompleteWithToolsStreamCtxRecoversPanic(t *testing.T) {
	server := sseServer(t, contentSSE)
	c := newPlaceholderConnection()
	c.SetURL(server.URL)
	c.Stats = nil // forces a panic inside completeStream

	resp, err := c.CompleteWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil, func(string) {})
	if err == nil {
		t.Fatal("expected the recovered panic to surface as an error, got nil")
	}
	if resp != nil {
		t.Errorf("expected nil response on a recovered panic, got %+v", resp)
	}
	me, ok := err.(*ModelError)
	if !ok {
		t.Fatalf("expected *ModelError from a recovered panic, got %T: %v", err, err)
	}
	if !strings.Contains(me.Message, "stream panicked") {
		t.Errorf("error message = %q, want it to mention the recovered panic", me.Message)
	}
}

// TestCompleteWithToolsStreamCtxErrChClosedNoHang verifies the errCh is now
// closed (the round-1 fix) so the single reader never blocks and a would-be
// second reader terminates — the regression the close guards against.
func TestCompleteWithToolsStreamCtxErrChClosedNoHang(t *testing.T) {
	server := sseServer(t, contentSSE)
	c := newPlaceholderConnection()
	c.SetURL(server.URL)

	done := make(chan struct{})
	go func() {
		_, _ = c.CompleteWithToolsStreamCtx(context.Background(),
			[]Message{{Role: RoleUser, Content: "hi"}}, nil, func(string) {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CompleteWithToolsStreamCtx hung — errCh close / reader mismatch regressed")
	}
}

// TestCompleteWithToolsStreamCtxPartialStreamDeliversDeltas covers the cut-mid-
// stream edge: when the connection drops after some reasoning has streamed, the
// deltas already received must reach the sink and the call must return (not hang)
// — the invariant that lets the UI fold whatever partial thinking arrived.
func TestCompleteWithToolsStreamCtxPartialStreamDeliversDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"reasoning_content":"partial reasoning"},"index":0}]}` + "\n\n"))
		if fl != nil {
			fl.Flush()
		}
		// Abrupt close: no finish chunk, no [DONE].
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	}))
	defer server.Close()

	c := newPlaceholderConnection()
	c.SetURL(server.URL)

	var sink []string
	done := make(chan error, 1)
	go func() {
		_, err := c.CompleteWithToolsStreamCtx(context.Background(),
			[]Message{{Role: RoleUser, Content: "hi"}}, nil,
			func(d string) { sink = append(sink, d) })
		done <- err
	}()

	select {
	case <-done:
		// Whether the cut surfaces as EOF-completion or a read error, the call must
		// return, and the delta that did arrive must have reached the sink.
	case <-time.After(5 * time.Second):
		t.Fatal("CompleteWithToolsStreamCtx hung on a cut stream")
	}
	if len(sink) == 0 || sink[0] != "partial reasoning" {
		t.Errorf("sink = %q, wanted the delta delivered before the cut", sink)
	}
}
