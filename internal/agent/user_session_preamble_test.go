package agent

// Tests for the bounded in-loop preamble continuation nudge (issue #307).
//
// Two layers:
//   - White-box unit tests of the classification helpers (looksLikePreamble,
//     shouldNudgeContinuation), which are pure and need no backend.
//   - End-to-end runLoop tests that drive ExecuteTaskLoop against a scripted
//     httptest backend speaking OpenAI chat-completions JSON (the blocking path
//     runLoop takes when stream-thinking is off, the default). These exercise the
//     real loop semantics: that a preamble earns exactly one extra round-trip,
//     that a genuine final still terminates in one turn, that the nudge is
//     bounded, that the budget resets on a real tool call, and that the spliced
//     note rides into the model request but stays out of the visible transcript.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gogent/internal/model"
)

// --- unit tests: looksLikePreamble -----------------------------------------

func TestLooksLikePreamble(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		// Genuine preambles — open with an intent phrase, short, no result marker.
		{"ill start by", "I'll start by exploring the workspace structure.", true},
		{"let me", "Let me look at the dialog-related code.", true},
		{"first comma", "First, I will read the file.", true},
		{"i will", "I will analyze the issue now.", true},
		{"now ill", "Now I'll check the failing tests.", true},
		{"to begin", "To begin, gather the relevant files.", true},
		{"starting", "Starting with the configuration module.", true},
		{"im going to", "I'm going to refactor this function.", true},
		{"i plan to", "I plan to inspect each module in turn.", true},
		{"case insensitive", "LET ME CHECK THE LOGS.", true},
		{"leading whitespace trimmed", "\n\t  I'll begin now.", true},

		// Genuine final answers — must NOT be classified as preambles.
		{"plain statement no prefix", "The function returns the sum of two integers.", false},
		{"we should not i", "We should read the file before editing.", false},
		{"i think not a prefix", "I think this approach is reasonable.", false},
		{"done marker", "Done. The file has been updated.", false},
		{"here is marker", "Here is the summary of the changes.", false},
		{"in summary marker", "In summary, the bug was a missing nil check.", false},
		{"i have marker", "I have fixed the bug and verified the tests.", false},
		{"ive marker", "I've updated the config and reran the suite.", false},
		{"code fence marker", "```go\nfunc x() {}\n```", false},
		{"empty", "", false},
		{"whitespace only", "   \n  \t ", false},

		// Boundary: the prefix is REQUIRED — intent-ish prose that does not open
		// with a recognised phrase is treated as final (the narrow-test contract).
		{"intent mid sentence", "After this, I'll read the file.", false},

		// Boundary: a completion marker anywhere vetoes even a real intent prefix.
		// This is the deliberately conservative side (false-stop over false-go).
		{"prefix but completion marker", "I'll show you the result: the answer is 42.", false},
		// Pinning a subtle substring veto: "abandoned" contains "done", so this
		// preamble is (surprisingly) treated as final. Documented, not desired —
		// guards against an unintentional change to the marker set.
		{"substring marker veto", "I'll start by checking the abandoned files.", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikePreamble(tc.content); got != tc.want {
				t.Errorf("looksLikePreamble(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// TestLooksLikePreambleLengthBound pins the maxPreambleLen boundary: a turn of
// exactly the cap still qualifies, one byte longer is treated as final. A long
// answer that merely opens like a preamble must not be nudged.
func TestLooksLikePreambleLengthBound(t *testing.T) {
	const prefix = "I'll " // a recognised intent phrase with no completion marker
	atCap := prefix + strings.Repeat("a", maxPreambleLen-len(prefix))
	if len(atCap) != maxPreambleLen {
		t.Fatalf("test setup: len(atCap) = %d, want %d", len(atCap), maxPreambleLen)
	}
	if !looksLikePreamble(atCap) {
		t.Errorf("a preamble of exactly maxPreambleLen (%d) should still qualify", maxPreambleLen)
	}
	overCap := atCap + "a"
	if looksLikePreamble(overCap) {
		t.Errorf("a preamble of maxPreambleLen+1 (%d) must be treated as final", len(overCap))
	}
}

// --- unit tests: shouldNudgeContinuation -----------------------------------

func TestShouldNudgeContinuation(t *testing.T) {
	s := &UserSession{}
	preamble := &model.CompletionResponse{Content: "I'll start by reading the file."}
	final := &model.CompletionResponse{Content: "The file contains three functions."}

	tests := []struct {
		name          string
		resp          *model.CompletionResponse
		alreadyNudged int
		want          bool
	}{
		{"preamble within budget", preamble, 0, true},
		{"preamble at budget is refused", preamble, maxContinuationNudges, false},
		{"preamble over budget is refused", preamble, maxContinuationNudges + 1, false},
		{"final never nudged", final, 0, false},
		{"nil response never nudged", nil, 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.shouldNudgeContinuation(tc.resp, tc.alreadyNudged); got != tc.want {
				t.Errorf("shouldNudgeContinuation(%v, %d) = %v, want %v",
					tc.resp, tc.alreadyNudged, got, tc.want)
			}
		})
	}
}

// TestMaxContinuationNudgesIsBounded guards the core safety invariant: the
// per-stretch nudge budget is a small positive number. A zero or negative bound
// would disable the fix; a large one would let a chronically-narrating model
// burn many round-trips per stretch.
func TestMaxContinuationNudgesIsBounded(t *testing.T) {
	if maxContinuationNudges < 1 {
		t.Fatalf("maxContinuationNudges = %d, must be >= 1 (else the fix is a no-op)", maxContinuationNudges)
	}
	if maxContinuationNudges > 2 {
		t.Errorf("maxContinuationNudges = %d, want a small bound (<= 2) per issue #307", maxContinuationNudges)
	}
}

// --- end-to-end runLoop tests ----------------------------------------------

// scriptedBackend is an OpenAI-compatible chat-completions stub. It returns one
// canned JSON body per request from seq (falling back to def once seq is
// exhausted), records every request body, and counts requests. The blocking
// completion path runLoop uses when stream-thinking is off posts plain JSON, so
// no SSE is needed.
type scriptedBackend struct {
	mu     sync.Mutex
	seq    []string
	def    string
	n      int
	bodies []string
}

func (b *scriptedBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	b.bodies = append(b.bodies, string(body))
	i := b.n
	b.n++
	resp := b.def
	if i < len(b.seq) {
		resp = b.seq[i]
	}
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(resp))
}

func (b *scriptedBackend) requestCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

func (b *scriptedBackend) requestBodies() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.bodies))
	copy(out, b.bodies)
	return out
}

// respText builds a blocking chat-completions JSON body carrying assistant text
// and no tool calls (finish_reason "stop").
func respText(content string) string {
	return marshalCompletion(map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
}

// respToolCall builds a blocking chat-completions JSON body carrying a single
// native tool call (finish_reason "tool_calls").
func respToolCall(name, arguments string) string {
	return marshalCompletion(map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":       "call_1",
					"type":     "function",
					"function": map[string]any{"name": name, "arguments": arguments},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
}

func marshalCompletion(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// newLoopHarness wires a UserSession whose root agent talks to the scripted
// backend, registers the read-only calc tool so scripted tool calls execute, and
// records every emitted SessionEvent. It returns the session, the agent id and a
// thread-safe accessor for the captured events.
func newLoopHarness(t *testing.T, b *scriptedBackend) (*UserSession, string, func() []SessionEvent) {
	t.Helper()
	server := httptest.NewServer(b)
	t.Cleanup(server.Close)

	conn := model.NewModelConnection()
	conn.SetURL(server.URL)
	sess := model.NewModelSession("m", conn)
	agent := NewAgent("root", sess)
	agent.ToolRegistry.RegisterCalcTool()
	us := NewUserSession("s", agent)

	var mu sync.Mutex
	var events []SessionEvent
	us.SetObserver(func(ev SessionEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	get := func() []SessionEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]SessionEvent, len(events))
		copy(out, events)
		return out
	}
	return us, "root", get
}

func runLoopCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	// A timeout so a regression that loops forever fails fast instead of hanging
	// the suite (runLoop checks ctx between iterations and on every round-trip).
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func finalText(events []SessionEvent) (string, bool) {
	for _, ev := range events {
		if ev.Type == SessionEventFinal {
			return ev.Text, true
		}
	}
	return "", false
}

func countEvents(events []SessionEvent, typ SessionEventType) int {
	n := 0
	for _, ev := range events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// TestRunLoopPreambleEarnsOneContinuation: a bare preamble turn must NOT end the
// session. The loop splices one continuation note, the model then emits the tool
// call it described, the tool runs, and the next turn's genuine answer
// terminates. Exactly three round-trips: preamble, tool call, final.
func TestRunLoopPreambleEarnsOneContinuation(t *testing.T) {
	b := &scriptedBackend{seq: []string{
		respText("I'll start by computing the sum."),
		respToolCall("calc", `{"expression":"1+1"}`),
		respText("All done — the answer is 2."),
	}}
	us, id, getEvents := newLoopHarness(t, b)
	ctx, cancel := runLoopCtx(t)
	defer cancel()

	if _, err := us.ExecuteTaskLoop(ctx, id, "add one and one"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	if got := b.requestCount(); got != 3 {
		t.Errorf("round-trips = %d, want 3 (preamble + tool + final)", got)
	}
	events := getEvents()
	if n := countEvents(events, SessionEventToolCall); n != 1 {
		t.Errorf("tool-call events = %d, want 1 (the narrated call must actually run)", n)
	}
	got, ok := finalText(events)
	if !ok {
		t.Fatal("no SessionEventFinal emitted")
	}
	if got != "All done — the answer is 2." {
		t.Errorf("final text = %q, want the genuine answer", got)
	}
}

// TestRunLoopPreambleThenModelStatesDone: the continuation note is a neutral
// either/or — a model that truly has nothing to do may answer "done" on the
// extra turn and terminate. One nudge, two round-trips, no tool call, and the
// model's real answer is surfaced.
func TestRunLoopPreambleThenModelStatesDone(t *testing.T) {
	b := &scriptedBackend{seq: []string{
		respText("Let me check whether anything remains."),
		respText("Nothing further is needed; the task is already complete."),
	}}
	us, id, getEvents := newLoopHarness(t, b)
	ctx, cancel := runLoopCtx(t)
	defer cancel()

	if _, err := us.ExecuteTaskLoop(ctx, id, "anything to do?"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	if got := b.requestCount(); got != 2 {
		t.Errorf("round-trips = %d, want 2 (preamble + one nudge that the model answers)", got)
	}
	events := getEvents()
	if n := countEvents(events, SessionEventToolCall); n != 0 {
		t.Errorf("tool-call events = %d, want 0", n)
	}
	got, ok := finalText(events)
	if !ok {
		t.Fatal("no SessionEventFinal emitted")
	}
	if got != "Nothing further is needed; the task is already complete." {
		t.Errorf("final text = %q, want the model's stated-done answer", got)
	}
}

// TestRunLoopGenuineFinalTerminatesInOneTurn: the common path must not regress —
// a substantive first answer that is not a preamble terminates immediately with
// a single round-trip and no continuation nudge.
func TestRunLoopGenuineFinalTerminatesInOneTurn(t *testing.T) {
	const answer = "The capital of France is Paris."
	b := &scriptedBackend{seq: []string{respText(answer)}}
	us, id, getEvents := newLoopHarness(t, b)
	ctx, cancel := runLoopCtx(t)
	defer cancel()

	if _, err := us.ExecuteTaskLoop(ctx, id, "capital of France?"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	if got := b.requestCount(); got != 1 {
		t.Errorf("round-trips = %d, want 1 (a genuine final must not be continued)", got)
	}
	got, ok := finalText(getEvents())
	if !ok {
		t.Fatal("no SessionEventFinal emitted")
	}
	if got != answer {
		t.Errorf("final text = %q, want %q", got, answer)
	}
}

// TestRunLoopContinuationIsBounded: a model that narrates a preamble on EVERY
// turn must still terminate. With the budget at maxContinuationNudges the loop
// nudges at most that many times and then accepts the text as final. MaxSteps is
// set well above the expected stop so that hitting it (instead of the nudge
// bound) would be a distinguishable failure rather than a hang.
func TestRunLoopContinuationIsBounded(t *testing.T) {
	b := &scriptedBackend{def: respText("Let me keep thinking about the next step.")}
	us, id, getEvents := newLoopHarness(t, b)
	us.SetMaxSteps(8)
	ctx, cancel := runLoopCtx(t)
	defer cancel()

	if _, err := us.ExecuteTaskLoop(ctx, id, "go"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	// Initial request + exactly maxContinuationNudges continuation round-trips.
	want := 1 + maxContinuationNudges
	if got := b.requestCount(); got != want {
		t.Errorf("round-trips = %d, want %d (1 initial + %d bounded nudges); a higher count means the bound leaked",
			got, want, maxContinuationNudges)
	}
	if _, ok := finalText(getEvents()); !ok {
		t.Error("loop did not emit a final answer; it should fall back to the normal break once the bound is hit")
	}
}

// TestRunLoopNudgeBudgetResetsOnToolCall: the budget is per uninterrupted
// stretch of tool-free turns, not per task. A real tool call between two
// preambles must restore the budget so the second preamble is nudged too.
//
// With maxContinuationNudges == 1 this is decisive: preamble, tool, preamble,
// tool, final = 5 round-trips and 2 tool calls. Were the counter NOT reset, the
// second preamble would arrive with the budget already spent, the loop would
// break there, and neither the second tool call nor the final answer would ever
// be reached.
func TestRunLoopNudgeBudgetResetsOnToolCall(t *testing.T) {
	b := &scriptedBackend{seq: []string{
		respText("I'll start by computing the first sum."),
		respToolCall("calc", `{"expression":"1+1"}`),
		respText("Let me now compute the second sum."),
		respToolCall("calc", `{"expression":"2+2"}`),
		respText("Both sums are computed; the answer is 6."),
	}}
	us, id, getEvents := newLoopHarness(t, b)
	ctx, cancel := runLoopCtx(t)
	defer cancel()

	if _, err := us.ExecuteTaskLoop(ctx, id, "compute two sums"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	if got := b.requestCount(); got != 5 {
		t.Errorf("round-trips = %d, want 5; the budget must reset on each tool call", got)
	}
	events := getEvents()
	if n := countEvents(events, SessionEventToolCall); n != 2 {
		t.Errorf("tool-call events = %d, want 2 (the second preamble must be nudged after the reset)", n)
	}
	got, ok := finalText(events)
	if !ok {
		t.Fatal("no SessionEventFinal emitted")
	}
	if got != "Both sums are computed; the answer is 6." {
		t.Errorf("final text = %q, want the genuine answer", got)
	}
}

// TestRunLoopContinuationNoteStaysOutOfTranscript: the spliced note must reach
// the model (it rides into the very next request body) but, like a queued user
// note, must NOT surface to the UI as an assistant step or final answer.
func TestRunLoopContinuationNoteStaysOutOfTranscript(t *testing.T) {
	b := &scriptedBackend{seq: []string{
		respText("I'll begin by reading the configuration."),
		respText("The configuration is valid; nothing further to do."),
	}}
	us, id, getEvents := newLoopHarness(t, b)
	ctx, cancel := runLoopCtx(t)
	defer cancel()

	if _, err := us.ExecuteTaskLoop(ctx, id, "check config"); err != nil {
		t.Fatalf("ExecuteTaskLoop: %v", err)
	}

	// The note must be delivered to the model: it appears in the second request
	// body (the one issued after the preamble nudge).
	bodies := b.requestBodies()
	if len(bodies) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(bodies))
	}
	if !strings.Contains(bodies[1], continuationNudgeNote) {
		t.Errorf("continuation note not delivered to the model in the post-preamble request:\n%s", bodies[1])
	}

	// The note must NOT be emitted to the UI as a visible event.
	for _, ev := range getEvents() {
		if strings.Contains(ev.Text, continuationNudgeNote) {
			t.Errorf("continuation note leaked into a visible %v event: %q", ev.Type, ev.Text)
		}
	}
}
