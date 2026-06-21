package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gogent/internal/model"
	"gogent/internal/tool"
)

// eventRecorder is a thread-safe SessionObserver collector. Tool-result events
// from a concurrent batch arrive from the worker goroutines, so the recorder
// guards its slice with a mutex.
type eventRecorder struct {
	mu     sync.Mutex
	events []SessionEvent
}

func (r *eventRecorder) record(ev SessionEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *eventRecorder) snapshot() []SessionEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SessionEvent, len(r.events))
	copy(out, r.events)
	return out
}

// toolCalls counts SessionEventToolCall events in the slice.
func toolCalls(events []SessionEvent) []SessionEvent {
	var out []SessionEvent
	for _, ev := range events {
		if ev.Type == SessionEventToolCall {
			out = append(out, ev)
		}
	}
	return out
}

// toolResults counts SessionEventToolResult events in the slice.
func toolResults(events []SessionEvent) []SessionEvent {
	var out []SessionEvent
	for _, ev := range events {
		if ev.Type == SessionEventToolResult {
			out = append(out, ev)
		}
	}
	return out
}

// assertCallsPaired is the core issue #187 invariant: every started tool (every
// SessionEventToolCall) must reach exactly one terminal SessionEventToolResult,
// paired by the stable CallID. It fails if any call id has no matching result id
// or if the two multisets disagree (a result with no call, or duplicate ids).
func assertCallsPaired(t *testing.T, events []SessionEvent) {
	t.Helper()
	calls := toolCalls(events)
	results := toolResults(events)
	if len(calls) != len(results) {
		t.Errorf("ToolCall count %d != ToolResult count %d (events: %v)", len(calls), len(results), summarize(events))
	}
	callIDs := idMultiset(calls)
	resultIDs := idMultiset(results)
	if !equalMultiset(callIDs, resultIDs) {
		t.Errorf("ToolCall and ToolResult CallID multisets differ:\n  calls:   %v\n  results: %v", callIDs, resultIDs)
	}
	// A CallID must never be empty: the backend synthesizes one for the fallback
	// path, and the UI keys its pending map by it (issue #187).
	for _, ev := range calls {
		if ev.CallID == "" {
			t.Errorf("SessionEventToolCall for %q has an empty CallID — the UI cannot pair it", ev.Tool)
		}
	}
	for _, ev := range results {
		if ev.CallID == "" {
			t.Errorf("SessionEventToolResult for %q has an empty CallID — it cannot be paired back to its call", ev.Tool)
		}
	}
}

func idMultiset(events []SessionEvent) map[string]int {
	m := map[string]int{}
	for _, ev := range events {
		m[ev.CallID]++
	}
	return m
}

func equalMultiset(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func summarize(events []SessionEvent) string {
	var b strings.Builder
	for _, ev := range events {
		switch ev.Type {
		case SessionEventToolCall:
			b.WriteString("\n  CALL  id=" + ev.CallID + " tool=" + ev.Tool)
		case SessionEventToolResult:
			b.WriteString("\n  RESULT id=" + ev.CallID + " tool=" + ev.Tool + " result=" + truncate(ev.Result, 40))
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// newLifecycleSession wires a session at url whose registry exposes the given
// tools, and attaches an event recorder as the observer.
func newLifecycleSession(t *testing.T, url string, tools ...*tool.Tool) (*UserSession, *eventRecorder) {
	t.Helper()
	conn := model.NewModelConnection()
	conn.SetURL(url)
	sess := model.NewModelSession("test", conn)

	reg := tool.NewToolRegistry()
	for _, tl := range tools {
		reg.Register(tl)
	}

	ag := NewAgent("root", sess)
	ag.SetToolRegistry(reg)
	us := NewUserSession("s1", ag)

	rec := &eventRecorder{}
	us.SetObserver(rec.record)
	return us, rec
}

// readOnlyTool builds a registered read-only tool whose Execute returns the
// given result string.
func readOnlyTool(name, result string) *tool.Tool {
	return &tool.Tool{
		Name: name, ReadOnly: true, InputSchema: map[string]interface{}{"type": "object"},
		Execute: func(map[string]interface{}, tool.ToolContext) (interface{}, error) { return result, nil },
	}
}

// panickingReadOnlyTool builds a registered read-only tool that always panics.
func panickingReadOnlyTool(name string) *tool.Tool {
	return &tool.Tool{
		Name: name, ReadOnly: true, InputSchema: map[string]interface{}{"type": "object"},
		Execute: func(map[string]interface{}, tool.ToolContext) (interface{}, error) { panic(name + " exploded") },
	}
}

// blockingReadOnlyTool builds a registered read-only tool that blocks until its
// release channel is closed or its context is cancelled, returning whichever
// happened. Used to observe a batch mid-flight and to prove cancellation still
// emits a terminal result (issue #187).
func blockingReadOnlyTool(name string, release <-chan struct{}) *tool.Tool {
	return &tool.Tool{
		Name: name, ReadOnly: true, InputSchema: map[string]interface{}{"type": "object"},
		Execute: func(_ map[string]interface{}, tctx tool.ToolContext) (interface{}, error) {
			select {
			case <-release:
				return name + ":ok", nil
			case <-tctx.Context.Done():
				return name + ":cancelled", nil
			}
		},
	}
}

// TestSerialPanicEmitsTerminalResult drives a single panicking tool through the
// serial path and asserts its ToolCall is paired with a terminal ToolResult that
// reports the contained panic — the started entry must never be left "running"
// (issue #187). A single call is not all-read-only, so it runs serially.
//
// Note: a panic inside tool.Execute is recovered by the registry itself (issue
// #8, tool.go ExecuteToolCall), surfaced as an error response, so runToolCall
// returns a string and the normal-path result emit fires. This test therefore
// proves the END-TO-END property (a panicking tool still yields a paired terminal
// result) regardless of which layer recovers. The per-call recover in
// runAndEmitResult itself is covered by TestRunAndEmitResultRecoversPanic.
func TestSerialPanicEmitsTerminalResult(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		toolCallResponse("call_1", "boom", `{}`),
		finalResponse("Recovered."),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, rec := newLifecycleSession(t, server.URL, panickingReadOnlyTool("boom"))

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "use boom"); err != nil {
		t.Fatalf("loop should not error on a contained tool panic: %v", err)
	}

	events := rec.snapshot()
	assertCallsPaired(t, events)

	// The terminal result must carry the panic and the same id as its call.
	results := toolResults(events)
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 ToolResult, got %d", len(results))
	}
	if !strings.Contains(results[0].Result, "panicked") {
		t.Errorf("panic result should describe the panic, got %q", results[0].Result)
	}
	if results[0].CallID == "" || results[0].CallID != toolCalls(events)[0].CallID {
		t.Errorf("panic result CallID %q must match its call %q", results[0].CallID, toolCalls(events)[0].CallID)
	}
}

// TestConcurrentPanicEmitsTerminalResult drives a concurrent (all-read-only)
// batch where one of the tools panics. Both calls must still reach a terminal
// result paired by id — the panic in one slot must not strand the other, nor
// itself (issue #187).
func TestConcurrentPanicEmitsTerminalResult(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		multiToolCallResponse(
			[3]string{"call_ok", "read_ok", `{}`},
			[3]string{"call_boom", "read_boom", `{}`},
		),
		finalResponse("done"),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, rec := newLifecycleSession(t, server.URL,
		readOnlyTool("read_ok", "ok-result"),
		panickingReadOnlyTool("read_boom"),
	)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "read then boom"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	events := rec.snapshot()
	assertCallsPaired(t, events)

	// Locate each result by its call id and assert the panic was contained.
	byID := map[string]SessionEvent{}
	for _, ev := range toolResults(events) {
		byID[ev.CallID] = ev
	}
	if r, ok := byID["call_ok"]; !ok || !strings.Contains(r.Result, "ok-result") {
		t.Errorf("read_ok result missing/wrong: %+v", r)
	}
	if r, ok := byID["call_boom"]; !ok || !strings.Contains(r.Result, "panicked") {
		t.Errorf("read_boom should report a contained panic, got: %+v", r)
	}
}

// TestConcurrentBatchPairsEveryCallToResult drives a larger all-read-only batch
// whose results complete out of submission order, and asserts every call id has
// exactly one matching result id — no entry left "running" (issue #187).
func TestConcurrentBatchPairsEveryCallToResult(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		multiToolCallResponse(
			[3]string{"call_a", "slow_read", `{"id":"A","delay_ms":60}`},
			[3]string{"call_b", "slow_read", `{"id":"B","delay_ms":5}`},
			[3]string{"call_c", "slow_read", `{"id":"C","delay_ms":30}`},
		),
		finalResponse("done"),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	slowRead := &tool.Tool{
		Name: "slow_read", ReadOnly: true, InputSchema: map[string]interface{}{"type": "object"},
		Execute: func(args map[string]interface{}, _ tool.ToolContext) (interface{}, error) {
			d, _ := args["delay_ms"].(float64)
			time.Sleep(time.Duration(d) * time.Millisecond)
			return args["id"], nil
		},
	}
	us, rec := newLifecycleSession(t, server.URL, slowRead)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "read A B C"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	events := rec.snapshot()
	assertCallsPaired(t, events)

	// The model-supplied ids must be carried verbatim on the events.
	want := map[string]bool{"call_a": true, "call_b": true, "call_c": true}
	for _, ev := range toolCalls(events) {
		if !want[ev.CallID] {
			t.Errorf("unexpected ToolCall CallID %q", ev.CallID)
		}
	}
}

// TestNativeCallIDPropagatedToEvents verifies the model-supplied tool-call id is
// the id carried on both the ToolCall and ToolResult events (the preferred
// stable pairing id of issue #187), not a synthesized one.
func TestNativeCallIDPropagatedToEvents(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		multiToolCallResponse(
			[3]string{"native-id-xyz", "read_ok", `{}`},
		),
		finalResponse("done"),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	// A single read-only call still runs serially (allReadOnly needs >= 2); the
	// serial path uses the same toolEventID helper, so the native id propagates.
	us, rec := newLifecycleSession(t, server.URL, readOnlyTool("read_ok", "v"))

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "read"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	for _, ev := range append(toolCalls(rec.snapshot()), toolResults(rec.snapshot())...) {
		if ev.CallID != "native-id-xyz" {
			t.Errorf("%v event CallID = %q, want native-id-xyz", ev.Type, ev.CallID)
		}
	}
}

// TestMixedBatchSerialPanicPairsResults drives a non-read-only (mixed) batch —
// which runs serially — where one call panics. Both calls must still pair to a
// terminal result (issue #187).
func TestMixedBatchSerialPanicPairsResults(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		multiToolCallResponse(
			[3]string{"call_write", "write", `{}`},
			[3]string{"call_boom", "boom", `{}`},
		),
		finalResponse("done"),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	writeTool := &tool.Tool{
		Name: "write", InputSchema: map[string]interface{}{"type": "object"},
		Execute: func(map[string]interface{}, tool.ToolContext) (interface{}, error) { return "wrote", nil },
	}
	us, rec := newLifecycleSession(t, server.URL, writeTool, panickingReadOnlyTool("boom"))
	// boom is read-only but write is not, so the batch is mixed -> serial path.

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "write then boom"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	assertCallsPaired(t, rec.snapshot())
}

// TestMultiStepTurnPairsAcrossSteps drives a turn with two tool rounds and
// asserts the call/result pairing holds across steps — the synthetic id embeds
// the step, so a tool reused across rounds does not collide (issue #187).
func TestMultiStepTurnPairsAcrossSteps(t *testing.T) {
	fs := &fakeServer{responses: []map[string]interface{}{
		// Step 0: native call to calc (single -> serial).
		toolCallResponse("step0_call", "calc", `{"expression":"1+1"}`),
		// Step 1: native call to calc again (a fresh id from the model).
		toolCallResponse("step1_call", "calc", `{"expression":"2+2"}`),
		finalResponse("done"),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	reg := tool.NewToolRegistry()
	reg.RegisterCalcTool()
	conn := model.NewModelConnection()
	conn.SetURL(server.URL)
	sess := model.NewModelSession("test", conn)
	ag := NewAgent("root", sess)
	ag.SetToolRegistry(reg)
	us := NewUserSession("s1", ag)
	rec := &eventRecorder{}
	us.SetObserver(rec.record)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "calc twice"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	events := rec.snapshot()
	assertCallsPaired(t, events)
	if got := len(toolCalls(events)); got != 2 {
		t.Fatalf("expected 2 ToolCall events across the two steps, got %d", got)
	}
}

// TestCancelMidBatchStillEmitsResults starts a concurrent batch of blocking
// read-only tools, cancels the loop mid-flight via StopAgent, and asserts every
// started ToolCall still receives a terminal ToolResult. Cancellation must not
// strand a started tool (issue #187): the tools honour ctx, return, and the
// shared execute-and-report path emits their results before the loop unwinds.
func TestCancelMidBatchStillEmitsResults(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	fs := &fakeServer{responses: []map[string]interface{}{
		multiToolCallResponse(
			[3]string{"call_x", "block_read", `{}`},
			[3]string{"call_y", "block_read", `{}`},
		),
		finalResponse("done"),
	}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	us, rec := newLifecycleSession(t, server.URL, blockingReadOnlyTool("block_read", release))

	started := make(chan struct{}, 1)
	// Wrap the recorder so we can detect the moment both calls are announced.
	var callCount int
	var countMu sync.Mutex
	us.SetObserver(func(ev SessionEvent) {
		rec.record(ev)
		if ev.Type == SessionEventToolCall {
			countMu.Lock()
			callCount++
			if callCount == 2 {
				select {
				case started <- struct{}{}:
				default:
				}
			}
			countMu.Unlock()
		}
	})

	done := make(chan error, 1)
	go func() {
		_, err := us.ExecuteTaskLoop(context.Background(), "root", "block")
		done <- err
	}()

	// Wait until both calls are in flight (run(tasks) is blocking on the tools),
	// then cancel. The tools select on ctx.Done() and return promptly.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("never observed both ToolCall events emitted up-front")
	}

	if err := us.StopAgent("root"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not return after StopAgent (a tool likely ignored ctx)")
	}

	// Every started call must still have a terminal result paired by id.
	assertCallsPaired(t, rec.snapshot())
}

// TestStructuredOutputFinalDoesNotAnnounceCall drives a serial batch ending in a
// terminal structured_output{final:true}. No ToolCall is emitted for it (so
// nothing can be left "running" when the loop breaks early), while any preceding
// real tool still pairs normally (issue #187).
func TestStructuredOutputFinalDoesNotAnnounceCall(t *testing.T) {
	// Step 0: a real calc call, then a structured_output{final:true} in the same
	// serial batch. The batch is mixed (calc + structured_output) so it runs
	// serially.
	mixedThenFinal := map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role": "assistant",
				"tool_calls": []map[string]interface{}{
					{
						"id":   "call_calc",
						"type": "function",
						"function": map[string]interface{}{
							"name":      "calc",
							"arguments": `{"expression":"1+1"}`,
						},
					},
					{
						"id":   "call_final",
						"type": "function",
						"function": map[string]interface{}{
							"name":      "structured_output",
							"arguments": `{"response":"the answer","final":true}`,
						},
					},
				},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
	fs := &fakeServer{responses: []map[string]interface{}{mixedThenFinal}}
	server := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer server.Close()

	reg := tool.NewToolRegistry()
	reg.RegisterCalcTool()
	conn := model.NewModelConnection()
	conn.SetURL(server.URL)
	sess := model.NewModelSession("test", conn)
	ag := NewAgent("root", sess)
	ag.SetToolRegistry(reg)
	us := NewUserSession("s1", ag)
	rec := &eventRecorder{}
	us.SetObserver(rec.record)

	if _, err := us.ExecuteTaskLoop(context.Background(), "root", "calc then finalize"); err != nil {
		t.Fatalf("loop returned error: %v", err)
	}

	events := rec.snapshot()
	// calc is announced and resolved; structured_output{final:true} is folded in
	// and breaks the loop, so it must NOT be announced as a ToolCall.
	calls := toolCalls(events)
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 ToolCall (calc only), got %d: %v", len(calls), summarize(events))
	}
	if calls[0].Tool != "calc" {
		t.Errorf("expected the single ToolCall to be calc, got %q", calls[0].Tool)
	}
	assertCallsPaired(t, events)

	// The final answer folded from structured_output must surface as the final.
	var final string
	for _, ev := range events {
		if ev.Type == SessionEventFinal {
			final = ev.Text
		}
	}
	if !strings.Contains(final, "the answer") {
		t.Errorf("final text should carry the structured_output response, got %q", final)
	}
}

// TestRunAndEmitResultRecoversPanic exercises runAndEmitResult's OWN per-call
// recover — the defense-in-depth path the shared helper adds on top of the
// registry's issue #8 recovery. A panic inside tool.Execute never reaches this
// recover (the registry catches it first), so we force a panic that escapes the
// registry: a nil ToolRegistry dereferences at tool.go:332, before the registry's
// own defer/recover is registered. The panic then unwinds into runAndEmitResult,
// which must still emit exactly one terminal result carrying the call's id, so the
// started tool is never left "running" (issue #187).
func TestRunAndEmitResultRecoversPanic(t *testing.T) {
	conn := model.NewModelConnection()
	sess := model.NewModelSession("t", conn)
	ag := NewAgent("root", sess)
	ag.SetToolRegistry(nil) // -> runToolCall panics before the registry recover

	us := NewUserSession("s", ag)

	var mu sync.Mutex
	var got []SessionEvent
	emit := func(ev SessionEvent) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev)
	}

	call := tool.ToolCall{Tool: "anything", CallID: "native-x"}
	msg := us.runAndEmitResult(context.Background(), ag, "root", call, 0, "native-x", emit)

	var results []SessionEvent
	for _, ev := range got {
		if ev.Type == SessionEventToolResult {
			results = append(results, ev)
		}
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 ToolResult from the recovered panic, got %d (events: %v)", len(results), got)
	}
	if results[0].CallID != "native-x" {
		t.Errorf("recovered result CallID = %q, want native-x", results[0].CallID)
	}
	if !strings.Contains(results[0].Result, "panicked") {
		t.Errorf("recovered result should describe the panic, got %q", results[0].Result)
	}
	// The message fed back to the model must be non-empty so the loop can continue.
	if strings.TrimSpace(msg.Content) == "" {
		t.Error("runAndEmitResult returned an empty message after recovering a panic")
	}
}

// TestToolEventID covers the stable-id helper that pairs a call to its result
// (issue #187): the native id wins; otherwise a turn-unique synthetic id is
// built from tool/step/index, so repeated tool names and the fallback JSON path
// still pair one to one; it is never empty.
func TestToolEventID(t *testing.T) {
	tests := []struct {
		name string
		call tool.ToolCall
		step int
		idx  int
		want string
	}{
		{"native id wins", tool.ToolCall{Tool: "read", CallID: "native-1"}, 3, 0, "native-1"},
		{"fallback synthetic id embeds step and index", tool.ToolCall{Tool: "read"}, 3, 0, "read#3.0"},
		{"repeated tool name differs by index", tool.ToolCall{Tool: "read"}, 3, 1, "read#3.1"},
		{"same tool next step differs", tool.ToolCall{Tool: "read"}, 4, 0, "read#4.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := toolEventID(tc.call, tc.step, tc.idx)
			if got != tc.want {
				t.Errorf("toolEventID(%+v,%d,%d) = %q, want %q", tc.call, tc.step, tc.idx, got, tc.want)
			}
			if got == "" {
				t.Error("toolEventID must never return an empty id")
			}
		})
	}

	// Distinctness across a realistic batch: two same-named fallback calls plus a
	// native call produce three distinct ids.
	ids := map[string]bool{}
	for _, c := range []struct {
		call tool.ToolCall
		step int
		idx  int
	}{
		{tool.ToolCall{Tool: "read"}, 1, 0},
		{tool.ToolCall{Tool: "read"}, 1, 1},
		{tool.ToolCall{Tool: "read", CallID: "native"}, 1, 2},
	} {
		id := toolEventID(c.call, c.step, c.idx)
		if ids[id] {
			t.Errorf("toolEventID produced a duplicate id %q — calls would collide and strand (issue #187)", id)
		}
		ids[id] = true
	}
}
