package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// streamSSE builds an OpenAI-style SSE body that streams reasoning_content
// deltas (prefix), optional content, optional tool calls, then a terminal chunk
// with the given finish reason and usage. Each delta is its own SSE event so the
// streaming-thinking path emits one SessionEventThinkingDelta per reasoning
// fragment.
func streamSSE(reasoning []string, content, finishReason string, toolCalls string) string {
	var b strings.Builder
	for _, r := range reasoning {
		b.WriteString(`data: {"choices":[{"delta":{"reasoning_content":` + jsonString(r) + `},"index":0}]}` + "\n\n")
	}
	if content != "" {
		b.WriteString(`data: {"choices":[{"delta":{"content":` + jsonString(content) + `},"index":0` +
			finishChunk(finishReason, content != "") + `}]}` + "\n\n")
	}
	if toolCalls != "" {
		b.WriteString(`data: {"choices":[{"delta":{"tool_calls":` + toolCalls + `},"index":0,"finish_reason":"tool_calls"}]}` + "\n\n")
	}
	if content == "" && toolCalls == "" {
		b.WriteString(`data: {"choices":[{"delta":{},"index":0,"finish_reason":"` + finishReason + `"}]}` + "\n\n")
	}
	b.WriteString(`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// finishChunk appends a finish_reason to a content delta when one is wanted.
func finishChunk(reason string, hasContent bool) string {
	if reason == "" {
		return ""
	}
	return `,"finish_reason":"` + reason + `"`
}

// jsonString quotes a Go string as a JSON string literal (escapes newlines).
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// blockingJSON builds a single (non-streamed) JSON completion response.
func blockingJSON(content string) string {
	return `{"choices":[{"index":0,"message":{"role":"assistant","content":` +
		jsonString(content) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
}

// seqServer serves bodies in sequence (one per request, last one reused) and
// records each request body so a test can assert on the wire shape (e.g.
// "stream":true). Bodies may be SSE or plain JSON; the handler does not care.
type seqServer struct {
	mu       sync.Mutex
	bodies   []string
	calls    int
	requests []string
}

func (s *seqServer) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	idx := s.calls
	if idx >= len(s.bodies) {
		idx = len(s.bodies) - 1
	}
	s.calls++
	s.requests = append(s.requests, string(body))
	isSSE := strings.HasPrefix(strings.TrimSpace(s.bodies[idx]), "data:")
	s.mu.Unlock()

	if isSSE {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s.bodies[idx]))
}

func (s *seqServer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// requestHadStream reports whether the n-th (0-indexed) request asked to stream.
func (s *seqServer) requestHadStream(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 || n >= len(s.requests) {
		return false
	}
	return strings.Contains(s.requests[n], `"stream":true`)
}

// newStreamLoopSession builds a root session (with the calc tool registered)
// pointed at url, mirroring newLoopSession but with a clearer name for these
// streaming-thinking tests.
func newStreamLoopSession(t *testing.T, url string) (*UserSession, *Agent) {
	return newLoopSession(t, url)
}

func filterEvents(events *[]SessionEvent, mu *sync.Mutex, typ SessionEventType) []SessionEvent {
	mu.Lock()
	defer mu.Unlock()
	var out []SessionEvent
	for _, ev := range *events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// eventsOf is a convenience wrapping filterEvents with a fresh mutex-protected
// snapshot. It returns the subset of events of the given type plus the mutex
// guarding the slice.
func eventsOf(events *[]SessionEvent, mu *sync.Mutex, typ SessionEventType) []SessionEvent {
	return filterEvents(events, mu, typ)
}

// snapshotEvents returns a copy of the collected events under the lock.
func snapshotEvents(events *[]SessionEvent, mu *sync.Mutex) []SessionEvent {
	mu.Lock()
	defer mu.Unlock()
	out := make([]SessionEvent, len(*events))
	copy(out, *events)
	return out
}

// TestStreamThinkingDefaultOffAndToggle: StreamThinking defaults to false and
// SetStreamThinking flips it. This is the opt-in gate for the whole feature.
func TestStreamThinkingDefaultOffAndToggle(t *testing.T) {
	us, _ := newStreamLoopSession(t, "http://unused")
	if us.StreamThinking() {
		t.Fatal("StreamThinking must default to off")
	}
	us.SetStreamThinking(true)
	if !us.StreamThinking() {
		t.Error("SetStreamThinking(true) did not enable streaming")
	}
	us.SetStreamThinking(false)
	if us.StreamThinking() {
		t.Error("SetStreamThinking(false) did not disable streaming")
	}
}

// TestStreamThinkingEnabledEmitsDeltaAndDone: with streaming on and a root
// agent, the loop emits one SessionEventThinkingDelta per reasoning fragment and
// a SessionEventThinkingDone after the round-trip, with the answer delivered as
// Final. This is the core happy path of issue #217.
func TestStreamThinkingEnabledEmitsDeltaAndDone(t *testing.T) {
	srv := &seqServer{bodies: []string{
		streamSSE([]string{"first reasoning ", "second line\n"}, "The answer is 42.", "stop", ""),
	}}
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	us, _ := newStreamLoopSession(t, server.URL)
	us.SetStreamThinking(true)

	var mu sync.Mutex
	events := collectEventsWith(&mu, us)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "what is 2+2?"); err != nil {
		t.Fatalf("loop error: %v", err)
	}

	deltas := eventsOf(events, &mu, SessionEventThinkingDelta)
	dones := eventsOf(events, &mu, SessionEventThinkingDone)
	if got := joinDeltas(deltas); got != "first reasoning second line\n" {
		t.Errorf("delta text = %q, want the full reasoning chain", got)
	}
	if len(deltas) != 2 {
		t.Errorf("expected 2 delta events (one per fragment), got %d", len(deltas))
	}
	if len(dones) != 1 {
		t.Errorf("expected 1 ThinkingDone after the round-trip, got %d", len(dones))
	}
	// The single turn must still produce a final answer.
	finals := eventsOf(events, &mu, SessionEventFinal)
	if len(finals) != 1 || !strings.Contains(finals[0].Text, "42") {
		t.Errorf("final event = %+v, want the answer 42", finals)
	}
	// The request must have been streamed (stream:true) — the streaming backend
	// is the one that surfaces reasoning.
	if !srv.requestHadStream(0) {
		t.Error("expected the root loop to send a streaming request when thinking is on")
	}
}

// TestStreamThinkingDisabledEmitsNoDeltaDone: with the option off (the default),
// the loop uses the blocking path and emits neither ThinkingDelta nor
// ThinkingDone — the feature is a true no-op when disabled.
func TestStreamThinkingDisabledEmitsNoDeltaDone(t *testing.T) {
	srv := &seqServer{bodies: []string{blockingJSON("The answer is 42.")}}
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	us, _ := newStreamLoopSession(t, server.URL)
	// StreamThinking left off (default).

	var mu sync.Mutex
	events := collectEventsWith(&mu, us)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "what is 2+2?"); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	if got := eventsOf(events, &mu, SessionEventThinkingDelta); len(got) != 0 {
		t.Errorf("disabled loop emitted %d ThinkingDelta events, want 0", len(got))
	}
	if got := eventsOf(events, &mu, SessionEventThinkingDone); len(got) != 0 {
		t.Errorf("disabled loop emitted %d ThinkingDone events, want 0", len(got))
	}
	// The blocking path was used (no stream:true).
	if srv.requestHadStream(0) {
		t.Error("disabled loop sent a streaming request; it must use the blocking path")
	}
}

// TestStreamThinkingMultiTurnPerStep: across a multi-turn tool loop, each model
// round-trip emits its own batch of deltas followed by its own ThinkingDone, so
// the live entry folds per turn rather than once at the end.
func TestStreamThinkingMultiTurnPerStep(t *testing.T) {
	toolCall := `[{"index":0,"id":"call_1","type":"function","function":{"name":"calc","arguments":"{\"expression\":\"2+2\"}"}}]`
	srv := &seqServer{bodies: []string{
		streamSSE([]string{"turn 0 thinking\n"}, "", "", toolCall),       // first round: reasoning + tool call
		streamSSE([]string{"turn 1 thinking\n"}, "It is 4.", "stop", ""), // second round: reasoning + answer
	}}
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	us, _ := newStreamLoopSession(t, server.URL)
	us.SetStreamThinking(true)

	var mu sync.Mutex
	events := collectEventsWith(&mu, us)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "what is 2+2?"); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	if srv.callCount() != 2 {
		t.Fatalf("expected 2 model round-trips, got %d", srv.callCount())
	}

	deltas := eventsOf(events, &mu, SessionEventThinkingDelta)
	dones := eventsOf(events, &mu, SessionEventThinkingDone)
	if len(dones) != 2 {
		t.Errorf("expected 2 ThinkingDone events (one per round-trip), got %d", len(dones))
	}
	// Steps must be 0 then 1.
	if len(dones) >= 2 && (dones[0].Step != 0 || dones[1].Step != 1) {
		t.Errorf("ThinkingDone steps = [%d,%d], want [0,1]", dones[0].Step, dones[1].Step)
	}
	// Reasoning from both turns reached the observer.
	if got := joinDeltas(deltas); !strings.Contains(got, "turn 0 thinking") || !strings.Contains(got, "turn 1 thinking") {
		t.Errorf("delta text = %q, want reasoning from both turns", got)
	}
	// Ordering: within each step the deltas precede that step's Done.
	snap := snapshotEvents(events, &mu)
	if err := assertDeltaBeforeDone(snap, 0); err != "" {
		t.Error(err)
	}
	if err := assertDeltaBeforeDone(snap, 1); err != "" {
		t.Error(err)
	}
}

// TestStreamThinkingNonRootDoesNotStream: even with the option on, a sub-agent
// (non-root) loop must NOT stream — it takes the blocking path and emits no
// ThinkingDelta events, so sub-agent reasoning never clutters the main window.
func TestStreamThinkingNonRootDoesNotStream(t *testing.T) {
	srv := &seqServer{bodies: []string{blockingJSON("sub-agent answer")}}
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	us, ag := newStreamLoopSession(t, server.URL)
	ag.Kind = KindTool // make it a sub-agent
	us.SetStreamThinking(true)

	var mu sync.Mutex
	events := collectEventsWith(&mu, us)

	if _, err := us.runLoop(context.Background(), ag, ag.ID, "do something", ""); err != nil {
		t.Fatalf("sub-agent loop error: %v", err)
	}
	if got := eventsOf(events, &mu, SessionEventThinkingDelta); len(got) != 0 {
		t.Errorf("sub-agent loop emitted %d ThinkingDelta events, want 0", len(got))
	}
	if got := eventsOf(events, &mu, SessionEventThinkingDone); len(got) != 0 {
		t.Errorf("sub-agent loop emitted %d ThinkingDone events, want 0", len(got))
	}
	// The sub-agent used the blocking path (no stream:true) even though the
	// session-wide option is on.
	if srv.requestHadStream(0) {
		t.Error("sub-agent loop streamed; only the root agent should stream")
	}
}

// TestStreamThinkingNonReasoningModelUnaffected: when the option is on but the
// model streams no reasoning, no ThinkingDelta is emitted — yet ThinkingDone is
// still fired (the UI's fold is a harmless no-op when nothing streamed), and the
// answer is delivered normally.
func TestStreamThinkingNonReasoningModelUnaffected(t *testing.T) {
	srv := &seqServer{bodies: []string{streamSSE(nil, "plain answer", "stop", "")}}
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	us, _ := newStreamLoopSession(t, server.URL)
	us.SetStreamThinking(true)

	var mu sync.Mutex
	events := collectEventsWith(&mu, us)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "hi"); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	if got := eventsOf(events, &mu, SessionEventThinkingDelta); len(got) != 0 {
		t.Errorf("non-reasoning model emitted %d deltas, want 0", len(got))
	}
	dones := eventsOf(events, &mu, SessionEventThinkingDone)
	if len(dones) != 1 {
		t.Errorf("expected 1 ThinkingDone (harmless fold), got %d", len(dones))
	}
	if finals := eventsOf(events, &mu, SessionEventFinal); len(finals) != 1 || !strings.Contains(finals[0].Text, "plain answer") {
		t.Errorf("final = %+v, want the plain answer", finals)
	}
}

// TestStreamThinkingFoldDoneOnError: if a streamed round-trip errors, the loop
// still emits ThinkingDone before reporting the error, so a partially-streamed
// live entry folds rather than staying expanded forever.
func TestStreamThinkingFoldDoneOnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer server.Close()

	us, _ := newStreamLoopSession(t, server.URL)
	us.SetStreamThinking(true)

	var mu sync.Mutex
	events := collectEventsWith(&mu, us)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "hi"); err == nil {
		t.Fatal("expected the loop to error on a 429, got nil")
	}
	// No deltas (the error happens before any reasoning streams), but a Done is
	// still emitted so any live entry folds.
	if got := eventsOf(events, &mu, SessionEventThinkingDelta); len(got) != 0 {
		t.Errorf("expected no deltas before the error, got %d", len(got))
	}
	if got := eventsOf(events, &mu, SessionEventThinkingDone); len(got) != 1 {
		t.Errorf("expected exactly 1 ThinkingDone on error (fold safety), got %d", len(got))
	}
	if got := eventsOf(events, &mu, SessionEventError); len(got) != 1 {
		t.Errorf("expected 1 SessionEventError, got %d", len(got))
	}
}

// --- helpers ---

// collectEventsWith is collectEvents that shares a caller-provided mutex so the
// test can also run eventsOf/snapshotEvents against the same slice.
func collectEventsWith(mu *sync.Mutex, us *UserSession) *[]SessionEvent {
	events := &[]SessionEvent{}
	us.SetObserver(func(ev SessionEvent) {
		mu.Lock()
		*events = append(*events, ev)
		mu.Unlock()
	})
	return events
}

func joinDeltas(ev []SessionEvent) string {
	var b strings.Builder
	for _, e := range ev {
		b.WriteString(e.Text)
	}
	return b.String()
}

// assertDeltaBeforeDone checks that, for the given step, every ThinkingDelta
// event appears before the ThinkingDone event in the snapshot. Returns a
// non-empty explanation string on failure.
func assertDeltaBeforeDone(events []SessionEvent, step int) string {
	deltaIdx, doneIdx := -1, -1
	for i, ev := range events {
		if ev.Step != step {
			continue
		}
		if ev.Type == SessionEventThinkingDelta && deltaIdx == -1 {
			deltaIdx = i
		}
		if ev.Type == SessionEventThinkingDone {
			doneIdx = i
		}
	}
	if doneIdx == -1 {
		return ""
	}
	if deltaIdx == -1 {
		return "" // no deltas this step (e.g. non-reasoning turn) — nothing to order
	}
	if deltaIdx > doneIdx {
		return "step deltas emitted after the step's ThinkingDone"
	}
	return ""
}

// TestStreamThinkingTrailingReasoning covers reasoning that streams AFTER the
// visible answer (some models emit thinking post-content). The loop must still
// surface it as a ThinkingDelta and fold on Done regardless of position, so the
// live entry is not skipped for trailing reasoning.
func TestStreamThinkingTrailingReasoning(t *testing.T) {
	// content first, then reasoning, then finish — reasoning is trailing here.
	const trailingSSE = `data: {"choices":[{"delta":{"content":"the answer"},"index":0}]}

data: {"choices":[{"delta":{"reasoning_content":"afterthought"},"index":0}]}

data: {"choices":[{"delta":{},"index":0,"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]

`
	srv := &seqServer{bodies: []string{trailingSSE}}
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	us, _ := newStreamLoopSession(t, server.URL)
	us.SetStreamThinking(true)

	var mu sync.Mutex
	events := collectEventsWith(&mu, us)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "hi"); err != nil {
		t.Fatalf("loop error: %v", err)
	}
	deltas := eventsOf(events, &mu, SessionEventThinkingDelta)
	if got := joinDeltas(deltas); got != "afterthought" {
		t.Errorf("trailing reasoning delta = %q, want %q", got, "afterthought")
	}
	if len(eventsOf(events, &mu, SessionEventThinkingDone)) != 1 {
		t.Error("expected a ThinkingDone to fold the trailing reasoning")
	}
}
