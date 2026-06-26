package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests cover issue #485: an OpenAI-compatible gateway returning HTTP 200 with
// an empty/whitespace-only body (blocking) or a zero-length SSE stream (streaming) must
// be detected, retried up to maxAttempts, and surfaced as a distinct ErrorEmptyResponse
// — never as the opaque `unexpected end of JSON input` / a silent empty assistant turn.
//
// They exercise four dimensions: the blocking retry+surface behavior, the guard's
// whitespace/empty boundary and scoping to status 200, the streaming detection (and the
// reasoning-only-cut false-positive guard), and the no-regression invariants (happy path,
// stats, context cancellation).

// emptyBodyServer replies 200 OK with the given (possibly empty/whitespace-only) body and
// counts handler hits into *hits. It models an OpenAI-compatible gateway that returns a
// zero-length or whitespace-only body while still sending 200 (issue #485).
func emptyBodyServer(t *testing.T, body string, hits *int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// parseStreamCollect drives parseOpenAIStream on body (mirroring how completeStream runs
// it on a buffered channel) and collects the forwarded reasoning/content deltas, the
// terminal event and the returned error — so the empty-stream guard can be asserted on
// directly and deterministically without an httptest round-trip.
func parseStreamCollect(t *testing.T, body string) (reasoning, content []string, terminal StreamResponse, perr error) {
	t.Helper()
	streamCh := make(chan StreamResponse, 64)
	type result struct {
		full string
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		full, _, err := parseOpenAIStream(strings.NewReader(body), streamCh)
		close(streamCh)
		resCh <- result{full, err}
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
	res := <-resCh
	_ = res.full
	perr = res.err
	return reasoning, content, terminal, perr
}

// ---------------------------------------------------------------------------
// Blocking path (complete -> Complete / CompleteWithToolsCtx)
// ---------------------------------------------------------------------------

// TestCompleteEmpty200RetriedThenSurfaced is the headline acceptance test for #485: a
// 200 OK with an EMPTY body must be retried up to maxAttempts (not abort on the first
// attempt, as it did before — json.Unmarshal("") = `unexpected end of JSON input`) and,
// once exhausted, surface a distinct, actionable ErrorEmptyResponse.
func TestCompleteEmpty200RetriedThenSurfaced(t *testing.T) {
	var hits int32
	c := newTestConn(emptyBodyServer(t, "", &hits).URL) // maxAttempts=3, backoff disabled

	resp, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err == nil {
		t.Fatalf("expected an error for an empty 200, got resp=%+v", resp)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("expected the empty 200 to be retried %d times (maxAttempts), got %d", 3, got)
	}
	me, ok := err.(*ModelError)
	if !ok {
		t.Fatalf("expected *ModelError, got %T: %v", err, err)
	}
	if me.Type != ErrorEmptyResponse {
		t.Errorf("expected error type %q, got %q", ErrorEmptyResponse, me.Type)
	}
	// Acceptance criterion: the message must state the real cause and attempt count.
	if !strings.Contains(me.Message, "(HTTP 200, 0 bytes)") {
		t.Errorf("expected the message to name HTTP 200 / 0 bytes, got %q", me.Message)
	}
	if !strings.Contains(me.Message, "after 3 attempt(s)") {
		t.Errorf("expected the message to report the attempt count, got %q", me.Message)
	}
	// The whole point of the fix: the opaque pre-fix failure must be gone.
	if strings.Contains(me.Error(), "unexpected end of JSON input") {
		t.Errorf("the opaque unmarshal error leaked through: %q", me.Error())
	}
	if strings.Contains(me.Error(), "failed to parse response") {
		t.Errorf("the generic parse wrapper leaked through: %q", me.Error())
	}
	if me.Type == ErrorGeneric {
		t.Errorf("must NOT be classified as ErrorGeneric (it was before the fix)")
	}
}

// TestCompleteEmpty200WhitespaceOnlyTreatedAsEmpty: whitespace-only bodies must be
// detected identically to a truly empty body — json.Unmarshal of "   "/"\n" also yields
// `unexpected end of JSON input`, so they are the same failure mode.
func TestCompleteEmpty200WhitespaceOnlyTreatedAsEmpty(t *testing.T) {
	for _, body := range []string{"", " ", "   ", "\n", "\r\n", "\t \n", "\n\t\r\n   "} {
		var hits int32
		c := newTestConn(emptyBodyServer(t, body, &hits).URL)
		_, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
		if err == nil {
			t.Errorf("body %q: expected an error, got nil", body)
			continue
		}
		me, ok := err.(*ModelError)
		if !ok || me.Type != ErrorEmptyResponse {
			t.Errorf("body %q: expected ErrorEmptyResponse, got %T (%v)", body, err, err)
		}
		if got := atomic.LoadInt32(&hits); got != 3 {
			t.Errorf("body %q: expected 3 retries, got %d", body, got)
		}
	}
}

// TestCompleteEmpty200ThenValidRecovers: a transient empty 200 on the first attempts
// followed by a valid body must self-heal via retry and return the valid completion —
// the core usability win (transient gateway hiccups become invisible).
func TestCompleteEmpty200ThenValidRecovers(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if n < 3 { // first two attempts: empty body
			return
		}
		_ = json.NewEncoder(w).Encode(CompletionResponse{Content: "recovered", Role: RoleAssistant})
	}))
	t.Cleanup(server.Close)

	c := newTestConn(server.URL)
	resp, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("expected recovery after retry, got error: %v", err)
	}
	if resp.Content != "recovered" {
		t.Errorf("expected the recovered content, got %q", resp.Content)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("expected 3 attempts (2 empty + 1 valid), got %d", got)
	}
}

// TestCompleteEmpty200WithSingleAttempt: with maxAttempts=1 the empty 200 must surface
// ErrorEmptyResponse on the first attempt (no retry) with an accurate count — guards the
// attempt-boundary branch (`attempt < attempts-1`) on the no-retry path.
func TestCompleteEmpty200WithSingleAttempt(t *testing.T) {
	var hits int32
	c := newTestConn(emptyBodyServer(t, "", &hits).URL)
	c.maxAttempts = 1

	_, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected exactly 1 attempt with maxAttempts=1, got %d", got)
	}
	me, ok := err.(*ModelError)
	if !ok || me.Type != ErrorEmptyResponse {
		t.Fatalf("expected ErrorEmptyResponse, got %T (%v)", err, err)
	}
	if !strings.Contains(me.Message, "after 1 attempt(s)") {
		t.Errorf("expected 'after 1 attempt(s)' in the message, got %q", me.Message)
	}
}

// TestCompleteHappyPathNotRetried: a normal 200 with a valid body must parse and return
// with exactly one request and one success — the new guard must not spuriously retry good
// responses (the most important no-regression invariant).
func TestCompleteHappyPathNotRetried(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(CompletionResponse{Content: "hello", Role: RoleAssistant})
	}))
	t.Cleanup(server.Close)

	c := newTestConn(server.URL)
	resp, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error on the happy path: %v", err)
	}
	if resp.Content != "hello" {
		t.Errorf("expected 'hello', got %q", resp.Content)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("happy path must make exactly 1 request, got %d", got)
	}
	if c.Stats.SuccessCount != 1 || c.Stats.RequestCount != 1 {
		t.Errorf("expected SuccessCount=1 RequestCount=1, got success=%d request=%d",
			c.Stats.SuccessCount, c.Stats.RequestCount)
	}
}

// TestCompleteEmpty200NoStatsInflation: a persistent empty 200 returns from inside the
// retry loop, before the post-loop bookkeeping, so it must not inflate the success (or
// request) counter — consistent with the sibling network-error / analyzeError terminal
// branches, which also skip the post-loop stats.
func TestCompleteEmpty200NoStatsInflation(t *testing.T) {
	var hits int32
	c := newTestConn(emptyBodyServer(t, "", &hits).URL)

	_, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if c.Stats.SuccessCount != 0 {
		t.Errorf("a failed empty-200 turn must not count as success, got SuccessCount=%d", c.Stats.SuccessCount)
	}
	if c.Stats.RequestCount != 0 {
		t.Errorf("expected RequestCount=0 (terminal return precedes post-loop stats), got %d", c.Stats.RequestCount)
	}
}

// TestCompleteEmptyBodyScopedToStatusOK: the empty-body guard is nested inside the
// `StatusCode == 200` check, so an empty body on a NON-200 (e.g. 504) must still flow
// through analyzeError (→ ErrorTimeout) and NOT be misclassified as ErrorEmptyResponse.
// Guards against the fix leaking into the existing non-200 retry path.
func TestCompleteEmptyBodyScopedToStatusOK(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusGatewayTimeout) // empty body, non-200
	}))
	t.Cleanup(server.Close)

	c := newTestConn(server.URL)
	_, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	me, ok := err.(*ModelError)
	if !ok {
		t.Fatalf("expected *ModelError, got %T", err)
	}
	if me.Type != ErrorTimeout {
		t.Errorf("a 504 + empty body must surface ErrorTimeout via analyzeError, got %q", me.Type)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("expected the 504 to be retried 3 times (5xx is retryable), got %d", got)
	}
}

// TestCompleteEmpty200RetryHonorsContextCancel: the new retry branch reuses sleepCtx, so a
// cancelled context must abort the empty-200 retry immediately (issue #24) rather than
// burning all attempts or hanging in backoff. The handler cancels ctx before returning
// the first empty 200, so the retry's sleepCtx deterministically observes the cancellation.
func TestCompleteEmpty200RetryHonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK) // empty 200
		cancel()                     // cancel before returning -> the retry's sleepCtx sees it
	}))
	t.Cleanup(server.Close)

	c := newTestConn(server.URL)
	c.retryBaseDelay = 200 * time.Millisecond // make the backoff sleep interruptible
	c.retryMaxDelay = 200 * time.Millisecond

	_, err := c.CompleteWithToolsCtx(ctx, []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("expected the cancelled empty-200 retry to surface an error")
	}
	me, ok := err.(*ModelError)
	if !ok {
		t.Fatalf("expected *ModelError, got %T: %v", err, err)
	}
	if me.Type != ErrorConnection {
		t.Errorf("expected the cancelled retry to surface a connection (ctx) error, got %q", me.Type)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected exactly 1 attempt before the ctx abort, got %d", got)
	}
}

// TestCompleteMalformedNonEmpty200StillGeneric pins the scope boundary of the fix
// (design Open Question #1): the guard catches EMPTY/whitespace bodies only. A NON-empty
// body that fails to parse (e.g. a gateway's HTML/garbage returned with 200) is
// intentionally left as a terminal ErrorGeneric on the first attempt — out of scope for
// this issue. This test locks that boundary so the empty-200 check is not accidentally
// broadened into retrying arbitrary malformed bodies.
func TestCompleteMalformedNonEmpty200StillGeneric(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{not valid json")) // non-empty, malformed
	}))
	t.Cleanup(server.Close)

	c := newTestConn(server.URL)
	_, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err == nil {
		t.Fatal("expected an error for a malformed 200 body")
	}
	me, ok := err.(*ModelError)
	if !ok {
		t.Fatalf("expected *ModelError, got %T", err)
	}
	if me.Type != ErrorGeneric {
		t.Errorf("a malformed non-empty 200 is out of scope and must remain ErrorGeneric, got %q", me.Type)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("a malformed 200 must not be retried (out of scope), got %d attempts", got)
	}
}

// ---------------------------------------------------------------------------
// Streaming path (parseOpenAIStream / completeStream)
// ---------------------------------------------------------------------------

// TestParseOpenAIStreamEmptyBodyDetected: a zero-length or sentinel-only SSE stream must
// be detected as an empty-response failure rather than silently returning ("", nil, nil)
// — the streaming half of the bug.
func TestParseOpenAIStreamEmptyBodyDetected(t *testing.T) {
	for _, body := range []string{"", "\n", "   \n\n  ", "data: [DONE]\n\n"} {
		reasoning, content, terminal, err := parseStreamCollect(t, body)
		if err == nil {
			t.Errorf("body %q: expected ErrorEmptyResponse, got nil (content=%q reasoning=%v terminal.Done=%v)",
				body, content, reasoning, terminal.Done)
			continue
		}
		me, ok := err.(*ModelError)
		if !ok || me.Type != ErrorEmptyResponse {
			t.Errorf("body %q: expected ErrorEmptyResponse, got %T (%v)", body, err, err)
		}
		if terminal.Done {
			t.Errorf("body %q: an empty stream must not emit a terminal Done event", body)
		}
	}
}

// TestParseOpenAIStreamReasoningOnlyCutNotFlagged: the critical false-positive guard
// (Defect A). A reasoning model that streamed thinking and was then cut before any
// finish/usage chunk is NOT empty — the `reasoning.Len() == 0` term in the conjunction
// prevents a spurious ErrorEmptyResponse that would otherwise discard the delivered
// thinking. Covers both the reasoning_content (Z.AI/GLM/DeepSeek) and reasoning
// (OpenRouter) field-name variants.
func TestParseOpenAIStreamReasoningOnlyCutNotFlagged(t *testing.T) {
	for _, name := range []struct {
		label, field, body string
	}{
		{"reasoning_content", "reasoning_content",
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking...\"},\"index\":0}]}\n\n"},
		{"reasoning", "reasoning",
			"data: {\"choices\":[{\"delta\":{\"reasoning\":\"plain thinking\"},\"index\":0}]}\n\n"},
	} {
		t.Run(name.label, func(t *testing.T) {
			reasoning, content, terminal, err := parseStreamCollect(t, name.body)
			if err != nil {
				t.Fatalf("a reasoning-only stream must not be flagged empty, got %v", err)
			}
			want := map[string]string{"reasoning_content": "thinking...", "reasoning": "plain thinking"}[name.field]
			if len(reasoning) == 0 || reasoning[0] != want {
				t.Errorf("expected the reasoning delta %q to be forwarded, got %v", want, reasoning)
			}
			if len(content) != 0 {
				t.Errorf("expected no visible content, got %v", content)
			}
			if !terminal.Done {
				t.Errorf("expected a terminal event (stream completed normally), got none")
			}
		})
	}
}

// TestParseOpenAIStreamFinishReasonOnlyNotFlagged: a turn that legitimately finishes with
// empty content still carries a finish reason, so it must not be flagged — pins the
// `finishReason == nil` term of the conjunction.
func TestParseOpenAIStreamFinishReasonOnlyNotFlagged(t *testing.T) {
	const body = "data: {\"choices\":[{\"delta\":{},\"index\":0,\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	_, _, terminal, err := parseStreamCollect(t, body)
	if err != nil {
		t.Fatalf("a finish-reason-only stream must not be flagged, got %v", err)
	}
	if terminal.FinishReason == nil || *terminal.FinishReason != "stop" {
		t.Errorf("expected finish reason 'stop', got %v", terminal.FinishReason)
	}
}

// TestParseOpenAIStreamUsageOnlyNotFlagged: the include_usage final chunk makes a stream
// non-empty even with no content/finish, so it must not be flagged — pins the
// `usage == nil` term.
func TestParseOpenAIStreamUsageOnlyNotFlagged(t *testing.T) {
	const body = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\ndata: [DONE]\n\n"
	_, _, terminal, err := parseStreamCollect(t, body)
	if err != nil {
		t.Fatalf("a usage-only stream must not be flagged, got %v", err)
	}
	if terminal.Usage == nil || terminal.Usage.TotalTokens != 7 {
		t.Errorf("expected usage with 7 total tokens, got %v", terminal.Usage)
	}
}

// TestParseOpenAIStreamToolCallOnlyNotFlagged: a tool-call-only turn is not empty — pins
// the `len(toolCalls) == 0` term.
func TestParseOpenAIStreamToolCallOnlyNotFlagged(t *testing.T) {
	const body = "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"calc\",\"arguments\":\"{}\"}}]},\"index\":0,\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	_, _, terminal, err := parseStreamCollect(t, body)
	if err != nil {
		t.Fatalf("a tool-call stream must not be flagged, got %v", err)
	}
	if len(terminal.ToolCalls) != 1 {
		t.Errorf("expected 1 assembled tool call, got %d", len(terminal.ToolCalls))
	}
}

// TestCompleteStreamEmptyBodySurfacesError: end-to-end via CompleteStream — a 200 with an
// empty SSE body must surface ErrorEmptyResponse on errCh and must NOT assemble a silent
// empty assistant turn (no terminal Done, no content deltas).
func TestCompleteStreamEmptyBodySurfacesError(t *testing.T) {
	server := emptyBodyServer(t, "", new(int32))
	c := newTestConn(server.URL)

	streamCh, errCh := c.CompleteStream([]Message{{Role: RoleUser, Content: "hi"}})
	deltas, terminal, err := drain(t, streamCh, errCh)
	if err == nil {
		t.Fatalf("expected ErrorEmptyResponse, got nil (deltas=%v terminal.Done=%v)", deltas, terminal.Done)
	}
	me, ok := err.(*ModelError)
	if !ok || me.Type != ErrorEmptyResponse {
		t.Fatalf("expected ErrorEmptyResponse, got %T (%v)", err, err)
	}
	if len(deltas) != 0 {
		t.Errorf("expected no content deltas on an empty stream, got %v", deltas)
	}
	if terminal.Done {
		t.Errorf("an empty stream must not emit a terminal Done event")
	}
}

// TestCompleteWithToolsStreamCtxEmptyBodySurfacesError: end-to-end via the live-thinking
// entry — an empty stream surfaces ErrorEmptyResponse and returns a nil response (not a
// spurious empty completion).
func TestCompleteWithToolsStreamCtxEmptyBodySurfacesError(t *testing.T) {
	server := emptyBodyServer(t, "", new(int32))
	c := newTestConn(server.URL)

	resp, err := c.CompleteWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil, func(string) {})
	if err == nil {
		t.Fatalf("expected ErrorEmptyResponse, got resp=%+v", resp)
	}
	if resp != nil {
		t.Errorf("expected a nil response on an empty stream, got %+v", resp)
	}
	me, ok := err.(*ModelError)
	if !ok || me.Type != ErrorEmptyResponse {
		t.Errorf("expected ErrorEmptyResponse, got %T (%v)", err, err)
	}
}

// TestCompleteWithToolsStreamCtxReasoningOnlyCutReturnsReasoning: the Defect-A guard at
// the public API — a reasoning-only cut stream returns the delivered reasoning (and a nil
// error), rather than collapsing to (nil, ErrorEmptyResponse). The reasoning reaches both
// the onReasoning sink and the assembled response.
func TestCompleteWithToolsStreamCtxReasoningOnlyCutReturnsReasoning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"reasoning_content":"thinking hard"},"index":0}]}`)
	}))
	t.Cleanup(server.Close)

	c := newTestConn(server.URL)
	var sink []string
	resp, err := c.CompleteWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil,
		func(d string) { sink = append(sink, d) })
	if err != nil {
		t.Fatalf("a reasoning-only cut stream must not error, got %v", err)
	}
	if resp == nil {
		t.Fatal("expected a non-nil response carrying the reasoning")
	}
	if resp.Reasoning != "thinking hard" {
		t.Errorf("expected Reasoning 'thinking hard', got %q", resp.Reasoning)
	}
	if resp.Content != "" {
		t.Errorf("expected empty visible content, got %q", resp.Content)
	}
	if len(sink) != 1 || sink[0] != "thinking hard" {
		t.Errorf("expected the reasoning delta to reach the sink, got %v", sink)
	}
}
