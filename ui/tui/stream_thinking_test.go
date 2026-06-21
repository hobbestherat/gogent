package ui

import (
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/agent"
)

// newThinkingTestSession builds a SessionWindow with the transcript model and
// pending-tools map wired (the minimum appendThinkingDelta/foldLiveThought/apply
// need), reusing the headless-textview pattern from transcript_model_test.go.
func newThinkingTestSession() *SessionWindow {
	return &SessionWindow{
		transcript:   newTranscriptModel(tv.NewTextView("", tv.Rect{})),
		pendingTools: map[string]*transcriptRecord{},
	}
}

// newThinkingCommandSession builds a window whose workbench exposes a
// /thinking handler, so the slash command can be exercised end to end.
func newThinkingCommandSession(id string, handler func(sessionID string, set *bool) bool) *SessionWindow {
	sw := newThinkingTestSession()
	sw.id = id
	sw.wb = &Workbench{handlers: Handlers{StreamThinking: handler}}
	return sw
}

// lastRecord returns the most recently added transcript record, or nil.
func lastRecord(sw *SessionWindow) *transcriptRecord {
	m := sw.transcript
	if len(m.records) == 0 {
		return nil
	}
	return m.records[len(m.records)-1]
}

// thinkingRecords returns every kindThinking record in the transcript.
func thinkingRecords(sw *SessionWindow) []*transcriptRecord {
	var out []*transcriptRecord
	for _, r := range sw.transcript.records {
		if r.kind == kindThinking {
			out = append(out, r)
		}
	}
	return out
}

// TestAppendThinkingDeltaLazyCreate: the live thinking entry is created on the
// first non-empty delta (and starts expanded so the user watches it build up);
// an empty delta creates nothing.
func TestAppendThinkingDeltaLazyCreate(t *testing.T) {
	sw := newThinkingTestSession()
	if sw.liveThought != nil {
		t.Fatal("liveThought must start nil")
	}

	// An empty delta is a no-op: no entry is created.
	sw.appendThinkingDelta("")
	if sw.liveThought != nil {
		t.Error("empty delta created a live entry; it must be ignored")
	}
	if got := lastRecord(sw); got != nil {
		t.Errorf("empty delta added a record %+v", got)
	}

	sw.appendThinkingDelta("reasoning")
	if sw.liveThought == nil {
		t.Fatal("non-empty delta must lazily create the live entry")
	}
	rec := lastRecord(sw)
	if rec == nil || rec.kind != kindThinking {
		t.Fatalf("live entry not a kindThinking record: %+v", rec)
	}
	if rec.header != "thinking…" {
		t.Errorf("live header = %q, want %q", rec.header, "thinking…")
	}
	if rec.collapsed {
		t.Error("live entry must start expanded so the user watches thinking build up")
	}
}

// TestAppendThinkingDeltaLineBuffering: only complete (newline-terminated) lines
// are committed live; a trailing partial is held back until the entry folds.
func TestAppendThinkingDeltaLineBuffering(t *testing.T) {
	sw := newThinkingTestSession()
	sw.appendThinkingDelta("first\nsecond")
	rec := sw.liveThought
	if rec == nil {
		t.Fatal("no live entry")
	}
	// "first" committed as a complete line; "second" held as a partial.
	if len(rec.lines) != 1 || rec.lines[0].text != "first" {
		t.Errorf("live lines = %+v, want only [first] committed", rec.lines)
	}
	if sw.liveThoughtBuf != "second" {
		t.Errorf("partial buffer = %q, want %q", sw.liveThoughtBuf, "second")
	}

	// A further delta completing the line commits it.
	sw.appendThinkingDelta(" line\n")
	if len(rec.lines) != 2 || rec.lines[1].text != "second line" {
		t.Errorf("after completing the line, lines = %+v, want [first, second line]", rec.lines)
	}
	if sw.liveThoughtBuf != "" {
		t.Errorf("partial buffer = %q, want empty after the newline", sw.liveThoughtBuf)
	}
}

// TestFoldLiveThoughtFlushRelabelCollapse: folding flushes the trailing partial,
// relabels "thinking…" → "thought", collapses the entry and clears the slot.
func TestFoldLiveThoughtFlushRelabelCollapse(t *testing.T) {
	sw := newThinkingTestSession()
	sw.appendThinkingDelta("whole line\n")
	sw.appendThinkingDelta("trailing partial") // held in the buffer

	sw.foldLiveThought()

	rec := lastRecord(sw)
	if rec == nil || rec.kind != kindThinking {
		t.Fatalf("folded entry missing/wrong kind: %+v", rec)
	}
	if rec.header != "thought" {
		t.Errorf("header = %q, want %q", rec.header, "thought")
	}
	if !rec.collapsed {
		t.Error("folded entry must be collapsed")
	}
	// Both the complete line and the flushed partial are present.
	if len(rec.lines) != 2 || rec.lines[0].text != "whole line" || rec.lines[1].text != "trailing partial" {
		t.Errorf("folded lines = %+v, want [whole line, trailing partial]", rec.lines)
	}
	if sw.liveThought != nil {
		t.Error("liveThought must be cleared after folding")
	}
	if sw.liveThoughtBuf != "" {
		t.Errorf("buffer = %q, want empty after fold", sw.liveThoughtBuf)
	}
}

// TestFoldLiveThoughtNoOpWhenNothingLive: folding with no live entry is a no-op
// (and must not panic) — it is called on every ThinkingDone and the busy→idle
// edge even for turns that streamed nothing.
func TestFoldLiveThoughtNoOpWhenNothingLive(t *testing.T) {
	sw := newThinkingTestSession()
	before := len(sw.transcript.records)
	sw.foldLiveThought() // nothing live
	sw.foldLiveThought() // idempotent
	if len(sw.transcript.records) != before {
		t.Errorf("fold with no live entry added records: %+v", sw.transcript.records)
	}
}

// TestApplyThinkingDeltaThenDone drives the two new events through apply() — the
// public event-routing path — and asserts the entry auto-collapses on Done.
func TestApplyThinkingDeltaThenDone(t *testing.T) {
	sw := newThinkingTestSession()
	sw.apply(agent.SessionEvent{Type: agent.SessionEventThinkingDelta, Text: "live reasoning\n"})
	rec := sw.liveThought
	if rec == nil || rec.collapsed { // expanded while streaming
		t.Fatalf("live entry must exist and be expanded mid-stream: %+v", rec)
	}
	sw.apply(agent.SessionEvent{Type: agent.SessionEventThinkingDone})
	rec = lastRecord(sw)
	if rec.header != "thought" || !rec.collapsed {
		t.Errorf("after Done, entry = header %q collapsed %v, want thought/collapsed", rec.header, rec.collapsed)
	}
	if sw.liveThought != nil {
		t.Error("liveThought must clear after the Done event")
	}
}

// TestApplyThinkingDoneWithNoDeltaCreatesNoEntry: a turn whose model streams no
// reasoning fires only ThinkingDone — the UI must not create an empty entry.
func TestApplyThinkingDoneWithNoDeltaCreatesNoEntry(t *testing.T) {
	sw := newThinkingTestSession()
	before := len(sw.transcript.records)
	sw.apply(agent.SessionEvent{Type: agent.SessionEventThinkingDone})
	if len(sw.transcript.records) != before {
		t.Errorf("ThinkingDone with no prior delta created an entry: %+v", sw.transcript.records)
	}
	if sw.liveThought != nil {
		t.Error("liveThought must remain nil when no delta streamed")
	}
}

// TestMultipleTurnsFoldSeparately: each turn creates its own entry that folds
// independently, so completed thoughts accumulate as separate collapsed entries.
func TestMultipleTurnsFoldSeparately(t *testing.T) {
	sw := newThinkingTestSession()
	sw.apply(agent.SessionEvent{Type: agent.SessionEventThinkingDelta, Text: "turn 1\n"})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventThinkingDone})

	sw.apply(agent.SessionEvent{Type: agent.SessionEventThinkingDelta, Text: "turn 2\n"})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventThinkingDone})

	recs := thinkingRecords(sw)
	if len(recs) != 2 {
		t.Fatalf("expected 2 folded thinking records, got %d: %+v", len(recs), recs)
	}
	for i, r := range recs {
		if r.header != "thought" || !r.collapsed {
			t.Errorf("record %d = header %q collapsed %v, want thought/collapsed", i, r.header, r.collapsed)
		}
	}
}

// TestSetBusyFalseFoldsLiveThought: the busy→idle safety net folds any entry
// left streaming by a cancelled or crashed turn so it does not stay expanded
// "thinking…" forever (issue #217). The session needs a workbench + status label
// because setBusy refreshes the status line.
func TestSetBusyFalseFoldsLiveThought(t *testing.T) {
	sw := &SessionWindow{
		transcript:   newTranscriptModel(tv.NewTextView("", tv.Rect{})),
		pendingTools: map[string]*transcriptRecord{},
		wb:           &Workbench{},
		status:       tv.NewLabel("idle", tv.Rect{}),
	}
	sw.appendThinkingDelta("interrupted mid-thought")
	// wasBusy is false (we never set busy=true), so the idle-edge side effects
	// (drain/supervise/focus restore) are skipped; only the fold + status refresh
	// run — exactly the safety net under test.
	sw.setBusy(false)

	rec := lastRecord(sw)
	if rec == nil || rec.header != "thought" || !rec.collapsed {
		t.Errorf("safety net did not fold the live entry: %+v", rec)
	}
	if sw.liveThought != nil {
		t.Error("liveThought must clear on busy→idle")
	}
}

// --- /thinking slash command ---

// TestThinkingCommandOnOffExplicit: /thinking on and /thinking off drive the
// handler with *bool and surface the resulting state as a transcript note.
func TestThinkingCommandOnOffExplicit(t *testing.T) {
	var state bool
	var captured *bool
	sw := newThinkingCommandSession("s1", func(sessionID string, set *bool) bool {
		if sessionID != "s1" {
			t.Errorf("handler called with session %q, want s1", sessionID)
		}
		captured = set
		if set != nil {
			state = *set
		}
		return state
	})

	if !sw.handleSlashCommand("/thinking on") {
		t.Fatal("/thinking was not recognized as a command")
	}
	if captured == nil || *captured != true {
		t.Errorf("/thinking on: handler got set=%v, want *true", captured)
	}
	if !state {
		t.Error("/thinking on did not turn the state on")
	}
	if got := lastRecord(sw); got == nil || got.header != "[System]" {
		t.Errorf("expected an on note, got %+v", got)
	}

	sw.handleSlashCommand("/thinking off")
	if captured == nil || *captured != false {
		t.Errorf("/thinking off: handler got set=%v, want *false", captured)
	}
	if state {
		t.Error("/thinking off did not turn the state off")
	}
}

// TestThinkingCommandAliases: true/false/1/0 are all accepted spellings.
func TestThinkingCommandAliases(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		want bool
	}{
		{"true", true}, {"1", true},
		{"false", false}, {"0", false},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			var got *bool
			sw := newThinkingCommandSession("s1", func(_ string, set *bool) bool {
				got = set
				if set != nil {
					return *set
				}
				return false
			})
			sw.handleSlashCommand("/thinking " + tc.arg)
			if got == nil || *got != tc.want {
				t.Errorf("/thinking %s: handler set=%v, want *%v", tc.arg, got, tc.want)
			}
		})
	}
}

// TestThinkingCommandFlipNoArg: with no argument, /thinking flips the current
// state (queries with set==nil, then applies the negation).
func TestThinkingCommandFlipNoArg(t *testing.T) {
	state := false
	sw := newThinkingCommandSession("s1", func(_ string, set *bool) bool {
		if set != nil {
			state = *set
		}
		return state
	})
	sw.handleSlashCommand("/thinking")
	if !state {
		t.Error("no-arg /thinking should flip false→true")
	}
	sw.handleSlashCommand("/thinking")
	if state {
		t.Error("a second no-arg /thinking should flip true→false")
	}
}

// TestThinkingCommandBadArg: an unrecognized argument prints a usage note and
// does not touch the state.
func TestThinkingCommandBadArg(t *testing.T) {
	called := false
	sw := newThinkingCommandSession("s1", func(_ string, _ *bool) bool { called = true; return false })
	sw.handleSlashCommand("/thinking maybe")
	if called {
		t.Error("handler must not be invoked for a bad argument")
	}
	rec := lastRecord(sw)
	if rec == nil || rec.header != "[System]" {
		t.Errorf("expected a usage note, got %+v", rec)
	}
	body := rec.body()
	if body == "" || (body != "usage: /thinking [on|off]" && !contains(body, "usage")) {
		t.Errorf("usage note body = %q, want a usage hint", body)
	}
}

// TestThinkingCommandUnavailable: with no handler wired (/thinking not backed by
// a backend), the command reports the feature unavailable instead of panicking.
func TestThinkingCommandUnavailable(t *testing.T) {
	sw := newThinkingCommandSession("s1", nil)
	sw.handleSlashCommand("/thinking on")
	rec := lastRecord(sw)
	if rec == nil {
		t.Fatal("expected an 'unavailable' note")
	}
	if body := rec.body(); !contains(body, "unavailable") {
		t.Errorf("note body = %q, want 'unavailable'", body)
	}
}

// TestThinkingCommandOnNoteWarnsNoRetry pins the driver's round-1 UX fix: since
// enabling live streaming changes failure semantics (transient errors are not
// retried), the on-note must surface that trade-off so the user is not surprised
// by a 429 that used to be retried. The off-note stays plain.
func TestThinkingCommandOnNoteWarnsNoRetry(t *testing.T) {
	state := false
	sw := newThinkingCommandSession("s1", func(_ string, set *bool) bool {
		if set != nil {
			state = *set
		}
		return state
	})

	// Turning on warns about the retry trade-off.
	sw.handleSlashCommand("/thinking on")
	rec := lastRecord(sw)
	if rec == nil {
		t.Fatal("expected an on-note")
	}
	body := rec.body()
	if !contains(body, "on") {
		t.Errorf("on-note body = %q, want it to state streaming is on", body)
	}
	if !contains(body, "retr") { // matches "retry"/"retried"/"not retried"
		t.Errorf("on-note body = %q, want it to warn that errors are not retried", body)
	}

	// Turning off is plain (no caveat needed).
	sw.handleSlashCommand("/thinking off")
	offRec := lastRecord(sw)
	if offBody := offRec.body(); contains(offBody, "retr") {
		t.Errorf("off-note body = %q, should not carry the no-retry caveat", offBody)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
