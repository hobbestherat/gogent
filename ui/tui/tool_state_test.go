package ui

import (
	"errors"
	"strings"
	"testing"

	"gogent/internal/agent"
)

// countRunning reports how many transcript entries still show "(running...)" —
// the stuck-tool symptom of issue #187. A clean turn leaves none.
func countRunning(sw *SessionWindow) int {
	return strings.Count(sw.transcript.view.AllText(), "(running...)")
}

// countDone reports how many tool entries reached the terminal "(done)" state.
func countDone(sw *SessionWindow) int {
	return strings.Count(sw.transcript.view.AllText(), "(done)")
}

// assertNoRunning fails if any tool entry is still in the "(running...)" state
// and the pending map is not empty — the two halves of the issue #187 invariant
// for the UI side.
func assertNoRunning(t *testing.T, sw *SessionWindow) {
	t.Helper()
	if n := countRunning(sw); n != 0 {
		t.Errorf("expected no tool entries stuck \"(running...)\", got %d\n%s", n, sw.transcript.view.AllText())
	}
	if n := len(sw.pendingTools); n != 0 {
		t.Errorf("pendingTools map should be empty after the turn, still has %d entry/entries: %v", n, sw.pendingTools)
	}
}

// TestToolStateConcurrentBatchPairsByID drives a concurrent batch through apply:
// two calls announced, then their results arriving out of order. Both entries
// must flip to "(done)" and the pending map must clear — no entry left
// "(running...)" just because the results came back in a different order
// (issue #187). This is the exact path the old single-slot pendingTool broke on.
func TestToolStateConcurrentBatchPairsByID(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order []string // result order: call ids in the order their results arrive
	}{
		{"results in announce order", []string{"a", "b"}},
		{"results out of order", []string{"b", "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sw := newTestSession()
			sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "a", Tool: "Read", Args: map[string]interface{}{"path": "a.go"}})
			sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "b", Tool: "Read", Args: map[string]interface{}{"path": "b.go"}})

			// Both calls are now in flight; the pending map holds both.
			if got, want := len(sw.pendingTools), 2; got != want {
				t.Fatalf("pendingTools has %d entries after two begins, want %d (old code tracked only 1)", got, want)
			}

			for _, id := range tc.order {
				sw.apply(agent.SessionEvent{Type: agent.SessionEventToolResult, CallID: id, Tool: "Read", Result: id + "-body"})
			}

			if got, want := countDone(sw), 2; got != want {
				t.Errorf("expected %d \"(done)\" tool entries, got %d", want, got)
			}
			assertNoRunning(t, sw)

			// Each result landed under its own call's entry, not collapsed together.
			text := sw.transcript.view.AllText()
			for _, want := range []string{"a-body", "b-body"} {
				if !strings.Contains(text, want) {
					t.Errorf("result %q missing from transcript; results may have been paired to the wrong entry", want)
				}
			}
		})
	}
}

// TestToolStateBeginWithoutFinishIsSweptOnIdle starts a tool, then ends the turn
// (SessionEventFinal) without ever delivering its result — the orphaned entry
// must be flipped to a terminal "(interrupted)" state by the busy→idle safety
// net rather than staying "(running...)" forever (issue #187).
func TestToolStateBeginWithoutFinishIsSweptOnIdle(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	sw.setBusy(true)
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "orphan", Tool: "Read", Args: map[string]interface{}{"path": "x.go"}})
	if got := len(sw.pendingTools); got != 1 {
		t.Fatalf("expected 1 pending tool after begin, got %d", got)
	}

	// The turn ends with a final answer but no tool result for "orphan".
	sw.apply(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"})

	assertNoRunning(t, sw)
	if !strings.Contains(sw.transcript.view.AllText(), "(interrupted)") {
		t.Errorf("orphaned tool should be marked \"(interrupted)\" on busy→idle, got:\n%s", sw.transcript.view.AllText())
	}
}

// TestToolStateCancellationSweepsInFlightTool drives the cancellation path: a
// tool is started, then the loop is cancelled (SessionEventError, e.g. via
// StopAgent). The busy→idle edge must sweep the in-flight tool to a terminal
// state so it is not left "(running...)" (issue #187).
func TestToolStateCancellationSweepsInFlightTool(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	sw.setBusy(true)
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "inflight", Tool: "Grep", Args: map[string]interface{}{"q": "todo"}})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventError, Err: errors.New("session loop cancelled")})

	assertNoRunning(t, sw)
	if !strings.Contains(sw.transcript.view.AllText(), "(interrupted)") {
		t.Errorf("in-flight tool should be swept to \"(interrupted)\" on cancellation, got:\n%s", sw.transcript.view.AllText())
	}
	if !strings.Contains(sw.transcript.view.AllText(), "session loop cancelled") {
		t.Errorf("the error line should be recorded alongside the swept tool")
	}
}

// TestToolStateFullTurnLeavesNothingRunning drives a realistic full turn through
// apply on a workbench-backed window: busy, a concurrent batch that all resolve,
// then final. Nothing may remain "(running...)" (issue #187 regression guard).
func TestToolStateFullTurnLeavesNothingRunning(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	sw.setBusy(true)
	sw.apply(agent.SessionEvent{Type: agent.SessionEventThinking, Step: 0})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "call_a", Tool: "Read", Args: map[string]interface{}{"path": "a.go"}})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "call_b", Tool: "Read", Args: map[string]interface{}{"path": "b.go"}})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolResult, CallID: "call_b", Tool: "Read", Result: "b-contents"})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolResult, CallID: "call_a", Tool: "Read", Result: "a-contents"})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "here is the answer"})

	assertNoRunning(t, sw)
	if got, want := countDone(sw), 2; got != want {
		t.Errorf("expected %d done tool entries, got %d", want, got)
	}
}

// TestToolStateResultWithoutMatchingCallMakesFreshEntry drives a ToolResult whose
// id matches no announced call (a legacy/stray event). It must still render as a
// fresh terminal entry so the result is never silently dropped (issue #187).
func TestToolStateResultWithoutMatchingCallMakesFreshEntry(t *testing.T) {
	sw := newTestSession()

	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolResult, CallID: "stray", Tool: "Read", Result: "payload"})

	text := sw.transcript.view.AllText()
	if !strings.Contains(text, "payload") {
		t.Errorf("stray result should still render its payload, got:\n%s", text)
	}
	assertNoRunning(t, sw)
}

// TestToolStateMergeByID verifies the happy-path pairing: a call and its result
// with the same id collapse into one "(done)" entry holding both args and result.
func TestToolStateMergeByID(t *testing.T) {
	sw := newTestSession()
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "c1", Tool: "Edit", Args: map[string]interface{}{"file": "x.go"}})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolResult, CallID: "c1", Tool: "Edit", Result: "ok"})

	if _, stillPending := sw.pendingTools["c1"]; stillPending {
		t.Error("the c1 entry should be removed from pendingTools after its result")
	}
	text := sw.transcript.view.AllText()
	for _, want := range []string{"tool: Edit (done)", "file: x.go", "result:", "ok"} {
		if !strings.Contains(text, want) {
			t.Errorf("merged entry missing %q; got:\n%s", want, text)
		}
	}
}

// TestToolStateFailPendingToolsDirect exercises the safety-net helper directly:
// orphaned "(running...)" entries are flipped to the given terminal state and
// dropped from the pending map. toolHeaderName must recover the original tool
// name from the header so the rebuilt header reads correctly.
func TestToolStateFailPendingToolsDirect(t *testing.T) {
	sw := newTestSession()
	sw.beginToolCall("read1", "Read", map[string]interface{}{"path": "a"})
	sw.beginToolCall("write1", "WriteFile", map[string]interface{}{"path": "b"})
	if got := len(sw.pendingTools); got != 2 {
		t.Fatalf("expected 2 pending tools, got %d", got)
	}

	sw.failPendingTools("cancelled")

	if got := len(sw.pendingTools); got != 0 {
		t.Errorf("pendingTools should be empty after failPendingTools, got %d", got)
	}
	text := sw.transcript.view.AllText()
	for _, want := range []string{"tool: Read (cancelled)", "tool: WriteFile (cancelled)"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected %q in transcript after sweep, got:\n%s", want, text)
		}
	}
	if strings.Contains(text, "(running...)") {
		t.Errorf("no entry should remain \"(running...)\" after failPendingTools, got:\n%s", text)
	}
}

// TestToolStateFailPendingToolsNoopOnCleanTurn verifies the safety net is a
// no-op on a clean turn — calling it when the pending map is already empty does
// not disturb the transcript.
func TestToolStateFailPendingToolsNoopOnCleanTurn(t *testing.T) {
	sw := newTestSession()
	sw.addAssistant("hello")
	before := sw.transcript.view.AllText()

	sw.failPendingTools("interrupted")

	if got := sw.transcript.view.AllText(); got != before {
		t.Errorf("failPendingTools should be a no-op on an empty pending map; transcript changed:\nbefore: %s\nafter:  %s", before, got)
	}
}

// TestToolStateMultipleTurnsReset verifies the pending map does not leak across
// turns: a clean first turn leaves it empty, and a second turn with its own calls
// starts fresh with no stale entries (issue #187).
func TestToolStateMultipleTurnsReset(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	// Turn 1: a single call that resolves, then final.
	sw.setBusy(true)
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "t1", Tool: "Read", Args: map[string]interface{}{"path": "1"}})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolResult, CallID: "t1", Tool: "Read", Result: "1"})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "turn 1 done"})
	assertNoRunning(t, sw)

	// Turn 2: the pending map must start empty — no stale "t1" entry lingering.
	if got := len(sw.pendingTools); got != 0 {
		t.Fatalf("pendingTools leaked %d entry/entries into turn 2: %v", got, sw.pendingTools)
	}
	sw.setBusy(true)
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "t2a", Tool: "Read", Args: map[string]interface{}{"path": "2"}})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "t2b", Tool: "Read", Args: map[string]interface{}{"path": "3"}})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolResult, CallID: "t2a", Tool: "Read", Result: "2"})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolResult, CallID: "t2b", Tool: "Read", Result: "3"})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "turn 2 done"})
	assertNoRunning(t, sw)
}

// TestToolStateDuplicateCallIDSupersededFirstEntry covers the duplicate-CallID
// case a misbehaving model can produce (two tool_calls sharing one id). The first
// entry can no longer be matched to a result, so it must be flipped terminal
// ("(superseded)") at overwrite time rather than left "(running...)" beyond the
// safety net's reach (issue #187 regression guard).
func TestToolStateDuplicateCallIDSupersededFirstEntry(t *testing.T) {
	sw := newTestSession()
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "dup", Tool: "Read", Args: map[string]interface{}{"path": "a"}})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "dup", Tool: "Read", Args: map[string]interface{}{"path": "b"}})

	// The first entry is already terminal ("(superseded)"); only the second (live)
	// call is in flight and tracked under "dup". So exactly one entry is running.
	if got := len(sw.pendingTools); got != 1 {
		t.Fatalf("pendingTools should hold exactly the live call after a dup id, got %d (%v)", got, sw.pendingTools)
	}
	if got, want := countRunning(sw), 1; got != want {
		t.Fatalf("expected exactly the live call to be \"(running...)\" (%d), got %d:\n%s", want, got, sw.transcript.view.AllText())
	}
	if !strings.Contains(sw.transcript.view.AllText(), "tool: Read (superseded)") {
		t.Errorf("the displaced first entry should already be \"(superseded)\", got:\n%s", sw.transcript.view.AllText())
	}

	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolResult, CallID: "dup", Tool: "Read", Result: "b-body"})

	text := sw.transcript.view.AllText()
	if got := strings.Count(text, "(superseded)"); got != 1 {
		t.Errorf("expected 1 \"(superseded)\" entry, got %d:\n%s", got, text)
	}
	if !strings.Contains(text, "tool: Read (done)") {
		t.Errorf("the live call should be \"(done)\", got:\n%s", text)
	}
	if strings.Contains(text, "(running...)") {
		t.Errorf("no entry should remain \"(running...)\" after the duplicate-id batch resolves, got:\n%s", text)
	}
	if got := len(sw.pendingTools); got != 0 {
		t.Errorf("pendingTools should be empty after the result, got %d", got)
	}
}

// TestToolStateTripleDuplicateCallIDAllTerminal stacks three calls on one id:
// the first two are superseded, the third resolves. Every entry must reach a
// terminal state (issue #187) — toolHeaderName must recover the name even from an
// already-superseded header when the second overwrite happens.
func TestToolStateTripleDuplicateCallIDAllTerminal(t *testing.T) {
	sw := newTestSession()
	for i := 0; i < 3; i++ {
		sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "dup", Tool: "Read", Args: map[string]interface{}{"path": "p"}})
	}
	// Only the third (live) call is in flight; the two earlier are superseded.
	if got, want := len(sw.pendingTools), 1; got != want {
		t.Fatalf("pendingTools = %d, want %d (only the live call tracked)", got, want)
	}
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolResult, CallID: "dup", Tool: "Read", Result: "r"})

	text := sw.transcript.view.AllText()
	if got := strings.Count(text, "(superseded)"); got != 2 {
		t.Errorf("expected 2 \"(superseded)\" entries, got %d:\n%s", got, text)
	}
	if got := countDone(sw); got != 1 {
		t.Errorf("expected 1 \"(done)\" entry, got %d:\n%s", got, text)
	}
	assertNoRunning(t, sw)
}

// TestToolStateEmptyCallIDEntryIsSweptOnIdle covers the id-less (legacy/stray)
// ToolCall: it cannot be paired to a result by id, but it must still be tracked
// under a synthetic key so the busy→idle safety net sweeps it instead of leaving
// it "(running...)" forever (issue #187 regression guard).
func TestToolStateEmptyCallIDEntryIsSweptOnIdle(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	sw.setBusy(true)
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "", Tool: "Read", Args: map[string]interface{}{"path": "legacy"}})
	// Tracked (under a synthetic key), not dropped from the pending map.
	if got := len(sw.pendingTools); got != 1 {
		t.Fatalf("id-less call should be tracked under a synthetic key, pendingTools = %d", got)
	}
	if countRunning(sw) != 1 {
		t.Fatalf("expected the id-less call to render as \"(running...)\" while in flight")
	}

	// Turn ends with no matching result (it can't be paired by empty id).
	sw.apply(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"})

	assertNoRunning(t, sw)
	if !strings.Contains(sw.transcript.view.AllText(), "(interrupted)") {
		t.Errorf("id-less \"(running...)\" entry should be swept to \"(interrupted)\" on idle, got:\n%s", sw.transcript.view.AllText())
	}
}

// TestToolStateEmptyCallIDResultFallsBackToFreshEntry pairs an id-less result
// with an id-less call: the result cannot match the synthetic-tracked call, so it
// renders as its own fresh entry (never dropped), while the original running entry
// is swept on busy→idle (issue #187).
func TestToolStateEmptyCallIDResultFallsBackToFreshEntry(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	sw.setBusy(true)
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "", Tool: "Read", Args: map[string]interface{}{"path": "x"}})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventToolResult, CallID: "", Tool: "Read", Result: "legacy-payload"})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"})

	text := sw.transcript.view.AllText()
	// The result body must be rendered somewhere — never silently dropped.
	if !strings.Contains(text, "legacy-payload") {
		t.Errorf("id-less result should still render its payload, got:\n%s", text)
	}
	assertNoRunning(t, sw)
}
