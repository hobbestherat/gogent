package ui

import (
	"reflect"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// These tests cover issue #203: Up/Down prompt-history recall on the session
// input. They drive the real boundaries the feature hooks into — the input's
// OnTypeFn wrapper (the same field the event loop calls) for the end-to-end
// recall behaviour, and the submit path (sw.submitFn) for what gets recorded —
// plus targeted unit tests for the visual-line gating math. They do NOT modify
// the implementation; they only exercise it.

// typeKey dispatches a key event to the session input's real OnTypeFn wrapper —
// the exact boundary the event loop drives in production. It returns whatever the
// wrapper returned (true = the key was consumed).
func typeKey(sw *SessionWindow, key tui.KeyCode) bool {
	return sw.input.Component.OnTypeFn(sw.input.Component, tui.TypeEvent{Key: key})
}

// submitText types a prompt into the box and submits it through the real submit
// path, the way pressing Enter / clicking Send does. The submit path records the
// prompt into history before any busy/queue handling, so this is the realistic
// way to populate promptHistory.
func submitText(sw *SessionWindow, text string) {
	sw.input.SetText(text)
	sw.submitFn()
}

// caretAtEnd reports whether the caret sits at the very end of the buffer — the
// last logical line's final column. That is the position SetText leaves the caret
// in and the position the issue requires after every recall.
func caretAtEnd(sw *SessionWindow) bool {
	in := sw.input
	if in == nil || len(in.Lines) == 0 {
		return false
	}
	return in.CursorY == len(in.Lines)-1 &&
		in.CursorX == len([]rune(in.Lines[in.CursorY]))
}

// TestPromptHistoryRecallOrder is the core acceptance test (issue #203): after
// submitting several prompts, Up recalls them newest→oldest, Up past the oldest
// stops (no wrap), Down walks back toward the newest, and Down past the newest
// restores the stashed draft. Every step leaves the caret at the end.
func TestPromptHistoryRecallOrder(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	submitText(sw, "first")
	submitText(sw, "second")
	submitText(sw, "third")
	// history is oldest→newest: [first, second, third]; nav points past the end.
	if got := sw.promptHistory; !reflect.DeepEqual(got, []string{"first", "second", "third"}) {
		t.Fatalf("promptHistory = %v, want [first second third]", got)
	}

	// Up walks newest → oldest, caret at end each time.
	for _, want := range []string{"third", "second", "first"} {
		typeKey(sw, tui.KeyUp)
		if got := sw.input.GetText(); got != want {
			t.Fatalf("after Up, input = %q, want %q", got, want)
		}
		if !caretAtEnd(sw) {
			t.Fatalf("caret not at end after recalling %q (CursorY=%d CursorX=%d)",
				want, sw.input.CursorY, sw.input.CursorX)
		}
	}

	// Up at the oldest stops — no wrap, input unchanged, key still consumed.
	before := sw.input.GetText()
	if !typeKey(sw, tui.KeyUp) {
		t.Error("Up at oldest should still be consumed (so the caret does not move up a line)")
	}
	if got := sw.input.GetText(); got != before {
		t.Errorf("Up at oldest wrapped/changed input: got %q, want %q", got, before)
	}

	// Down walks back toward newest.
	for _, want := range []string{"second", "third"} {
		typeKey(sw, tui.KeyDown)
		if got := sw.input.GetText(); got != want {
			t.Fatalf("after Down, input = %q, want %q", got, want)
		}
		if !caretAtEnd(sw) {
			t.Fatalf("caret not at end after recalling %q", want)
		}
	}

	// Down past the newest restores the stashed draft. The draft was empty (the
	// box was empty when the first Up stashed it), so the box is cleared and nav
	// returns to "not navigating".
	typeKey(sw, tui.KeyDown)
	if got := sw.input.GetText(); got != "" {
		t.Errorf("Down past newest should restore the (empty) draft, got %q", got)
	}
	if sw.historyNav != len(sw.promptHistory) {
		t.Errorf("nav = %d after draft restore, want %d (not navigating)", sw.historyNav, len(sw.promptHistory))
	}

	// Once back at the draft (not navigating) a further Down must decline so the
	// caret is free to move down a line. We assert the history handler itself
	// returns false — the OnTypeFn wrapper's boolean is not a reliable signal
	// here because the input's own handler always reports arrow keys handled.
	if sw.handleHistoryKey(tui.TypeEvent{Key: tui.KeyDown}) {
		t.Error("Down when not navigating should be declined by history")
	}
}

// TestPromptHistoryCaretAtEnd pins the caret position precisely (not just
// "somewhere at the end"): after each Up/Down recall the caret column equals the
// recalled text's rune length on line 0. This guards against a regression where
// recall sets the text but leaves the caret where it was.
func TestPromptHistoryCaretAtEnd(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	submitText(sw, "hello")
	submitText(sw, "world!")

	typeKey(sw, tui.KeyUp) // → "world!"
	if sw.input.CursorY != 0 || sw.input.CursorX != len([]rune("world!")) {
		t.Errorf("after Up to \"world!\": CursorY=%d CursorX=%d, want 0/%d",
			sw.input.CursorY, sw.input.CursorX, len([]rune("world!")))
	}
	typeKey(sw, tui.KeyUp) // → "hello"
	if sw.input.CursorY != 0 || sw.input.CursorX != len([]rune("hello")) {
		t.Errorf("after Up to \"hello\": CursorY=%d CursorX=%d, want 0/%d",
			sw.input.CursorY, sw.input.CursorX, len([]rune("hello")))
	}
	typeKey(sw, tui.KeyDown) // → "world!"
	if sw.input.CursorY != 0 || sw.input.CursorX != len([]rune("world!")) {
		t.Errorf("after Down to \"world!\": CursorY=%d CursorX=%d, want 0/%d",
			sw.input.CursorY, sw.input.CursorX, len([]rune("world!")))
	}
}

// TestPromptHistoryDraftRestore verifies Down past the newest restores exactly
// the in-progress draft the user had typed before navigating, caret at end.
func TestPromptHistoryDraftRestore(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	submitText(sw, "one")
	submitText(sw, "two")
	sw.input.SetText("drafting now") // a real in-progress draft, not submitted

	typeKey(sw, tui.KeyUp) // stash "drafting now", recall "two"
	if got := sw.input.GetText(); got != "two" {
		t.Fatalf("first Up = %q, want two", got)
	}
	typeKey(sw, tui.KeyUp)   // → "one"
	typeKey(sw, tui.KeyDown) // → "two"
	typeKey(sw, tui.KeyDown) // past newest → restore draft
	if got := sw.input.GetText(); got != "drafting now" {
		t.Errorf("draft restore = %q, want %q", got, "drafting now")
	}
	if !caretAtEnd(sw) {
		t.Errorf("caret not at end after draft restore (CursorY=%d CursorX=%d)",
			sw.input.CursorY, sw.input.CursorX)
	}
}

// TestPromptHistoryDraftRestoreMultiline checks a multi-line draft round-trips
// through GetText (join) and SetText (split) intact when stashed and restored.
func TestPromptHistoryDraftRestoreMultiline(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	submitText(sw, "only")
	sw.input.SetText("line a\nline b\nline c")

	typeKey(sw, tui.KeyUp)   // recall "only", stash the 3-line draft
	typeKey(sw, tui.KeyDown) // only entry → past newest → restore draft
	if got := sw.input.GetText(); got != "line a\nline b\nline c" {
		t.Errorf("multiline draft restore = %q", got)
	}
	if !caretAtEnd(sw) {
		t.Errorf("caret not at end of restored multiline draft (CursorY=%d CursorX=%d)",
			sw.input.CursorY, sw.input.CursorX)
	}
	if sw.input.CursorY != 2 || sw.input.CursorX != len([]rune("line c")) {
		t.Errorf("restored caret = (Y=%d X=%d), want (2/%d)",
			sw.input.CursorY, sw.input.CursorX, len([]rune("line c")))
	}
}

// TestPromptHistoryResetOnNewSubmit verifies that submitting a new prompt resets
// navigation to the newest, so the next Up starts from the just-submitted prompt
// rather than the stale nav cursor.
func TestPromptHistoryResetOnNewSubmit(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	submitText(sw, "one")
	submitText(sw, "two")
	typeKey(sw, tui.KeyUp) // nav → "two"
	typeKey(sw, tui.KeyUp) // nav → "one" (navigating in the middle of history)

	// Submit something new: nav must reset so Up next yields the newest.
	submitText(sw, "three")
	if got := sw.promptHistory; !reflect.DeepEqual(got, []string{"one", "two", "three"}) {
		t.Fatalf("promptHistory = %v, want [one two three]", got)
	}
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "three" {
		t.Errorf("Up after new submit = %q, want newest %q (nav should reset on submit)", got, "three")
	}
}

// TestPromptHistoryEmptyIsNoOp verifies that with no submitted prompts, Up/Down
// do not engage history: handleHistoryKey returns false so the input handles the
// key as ordinary caret movement and the buffer is untouched.
func TestPromptHistoryEmptyIsNoOp(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	sw.input.SetText("typing away")
	before := sw.input.GetText()

	// handleHistoryKey itself must decline.
	if sw.handleHistoryKey(tui.TypeEvent{Key: tui.KeyUp}) {
		t.Error("handleHistoryKey(Up) should be false with empty history")
	}
	if sw.handleHistoryKey(tui.TypeEvent{Key: tui.KeyDown}) {
		t.Error("handleHistoryKey(Down) should be false with empty history")
	}
	// Through the real wrapper too: text unchanged.
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != before {
		t.Errorf("Up with empty history changed text: %q -> %q", before, got)
	}
	typeKey(sw, tui.KeyDown)
	if got := sw.input.GetText(); got != before {
		t.Errorf("Down with empty history changed text: %q -> %q", before, got)
	}
}

// TestPromptHistorySingleEntry covers the one-entry boundary: Up recalls it, a
// second Up stops, Down restores the draft, a further Down falls through.
func TestPromptHistorySingleEntry(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	submitText(sw, "solo")

	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "solo" {
		t.Fatalf("Up = %q, want solo", got)
	}
	// Second Up: already at oldest, stop.
	if got := sw.input.GetText(); got != "solo" {
		t.Fatalf("precondition: %q", got)
	}
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "solo" {
		t.Errorf("second Up should stay at the only entry, got %q", got)
	}
	// Down restores the (empty) draft.
	typeKey(sw, tui.KeyDown)
	if got := sw.input.GetText(); got != "" {
		t.Errorf("Down past the only entry should restore the empty draft, got %q", got)
	}
	// Now not navigating again: Down must decline (see TestPromptHistoryRecallOrder
	// for why the wrapper boolean is not used here).
	if sw.handleHistoryKey(tui.TypeEvent{Key: tui.KeyDown}) {
		t.Error("Down when not navigating should be declined by history")
	}
}

// TestPromptHistoryConsecutiveDuplicateSkipped verifies recordHistory de-dupes a
// prompt identical to the most recent one (the only dedup the issue asks for),
// while a different prompt after it is kept.
func TestPromptHistoryConsecutiveDuplicateSkipped(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	submitText(sw, "dup")
	submitText(sw, "dup")   // identical to last → skipped
	submitText(sw, "other") // different → kept

	if got := sw.promptHistory; !reflect.DeepEqual(got, []string{"dup", "other"}) {
		t.Fatalf("promptHistory = %v, want [dup other] (consecutive dup skipped)", got)
	}
	// Recall confirms the stored order: newest first.
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "other" {
		t.Errorf("Up = %q, want other", got)
	}
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "dup" {
		t.Errorf("Up = %q, want dup", got)
	}
	typeKey(sw, tui.KeyUp) // oldest; stays
	if got := sw.input.GetText(); got != "dup" {
		t.Errorf("Up at oldest = %q, want dup", got)
	}
}

// TestPromptHistorySlashCommandIncluded verifies slash commands count as typed
// prompts and enter history (the issue's recommendation), even though they are
// otherwise handled client-side rather than sent to the model.
func TestPromptHistorySlashCommandIncluded(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	submitText(sw, "/goal ship it")

	if got := sw.promptHistory; !reflect.DeepEqual(got, []string{"/goal ship it"}) {
		t.Fatalf("promptHistory = %v, want the slash command recorded", got)
	}
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "/goal ship it" {
		t.Errorf("Up = %q, want the slash command recalled", got)
	}
}

// TestPromptHistoryExcludesSupervisorNudge verifies only USER-typed submissions
// enter history: the supervisor nudge path (nudgeSession, which sets
// nudgingSend) re-enters submit but must not be recorded.
func TestPromptHistoryExcludesSupervisorNudge(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	// The real nudge path: it sets nudgingSend around the submit, so the guard
	// in submit must skip recording. nudgeSession needs an idle window.
	sw.nudgeSession("finish the task")
	if len(sw.promptHistory) != 0 {
		t.Errorf("supervisor nudge leaked into history: %v", sw.promptHistory)
	}

	// Contrast: a genuine user submit on a fresh window is recorded.
	sw2 := w.openWindow("s2", "S2")
	submitText(sw2, "real user message")
	if got := sw2.promptHistory; !reflect.DeepEqual(got, []string{"real user message"}) {
		t.Errorf("normal submit should record, got %v", got)
	}
}

// TestPromptHistoryExcludesDrainDoubleRecord verifies a queued message is
// recorded exactly once — when the user pressed Enter to queue it — and not
// again when it drains on idle (the draining guard).
func TestPromptHistoryExcludesDrainDoubleRecord(t *testing.T) {
	w := newTestWorkbench(t)
	sent := recordSends(w)
	sw := w.openWindow("s", "S")

	sw.busy = true           // a turn is in flight
	submitText(sw, "queued") // recorded once, then stashed in the pending slot
	if got := sw.promptHistory; !reflect.DeepEqual(got, []string{"queued"}) {
		t.Fatalf("queued message should be recorded once, got %v", got)
	}
	if sw.pending != "queued" {
		t.Fatalf("pending = %q, want queued", sw.pending)
	}

	// The turn ends → the queue drains as the next turn.
	sw.busy = false
	sw.drainQueue()

	// Still recorded exactly once; and it was sent exactly once (by the drain).
	if got := sw.promptHistory; !reflect.DeepEqual(got, []string{"queued"}) {
		t.Errorf("drain re-recorded the message: %v", got)
	}
	waitSend(t, sent) // the drain dispatched it
}

// TestPromptHistoryPerSessionIsolated verifies the history is per-session
// (per-SessionWindow): submitting in one window does not seed another's recall.
func TestPromptHistoryPerSessionIsolated(t *testing.T) {
	w := newTestWorkbench(t)
	a := w.openWindow("a", "A")
	b := w.openWindow("b", "B")

	submitText(a, "only in A")

	if len(b.promptHistory) != 0 {
		t.Errorf("window B should have empty history, got %v", b.promptHistory)
	}
	// Up in B does nothing; Up in A recalls A's prompt.
	b.input.SetText("b typing")
	typeKey(b, tui.KeyUp)
	if got := b.input.GetText(); got != "b typing" {
		t.Errorf("B should not recall A's history, got %q", got)
	}
	a.input.SetText("")
	typeKey(a, tui.KeyUp)
	if got := a.input.GetText(); got != "only in A" {
		t.Errorf("A should recall its own history, got %q", got)
	}
}

// TestPromptHistoryIgnoresNonArrowKeys verifies handleHistoryKey only acts on
// Up/Down; other keys (Left, a typed rune) must return false so they reach the
// input unchanged.
func TestPromptHistoryIgnoresNonArrowKeys(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	submitText(sw, "hist")

	for _, key := range []tui.KeyCode{tui.KeyLeft, tui.KeyRight, tui.KeyHome, tui.KeyEnd, tui.KeyRune} {
		if sw.handleHistoryKey(tui.TypeEvent{Key: key, Rune: 'a'}) {
			t.Errorf("handleHistoryKey should not consume key %v", key)
		}
	}
}

// TestPromptHistorySingleLineAlwaysRecallsWithLayout verifies that with the input
// properly laid out (non-zero width), a single-line prompt still triggers recall
// in both directions: the caret is on the first AND the last visual line.
func TestPromptHistorySingleLineAlwaysRecallsWithLayout(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	layout(sw, 80, 24, 3) // input width 70 → text wraps at 69; a short line is one row

	submitText(sw, "the one and only")
	sw.input.SetText("x") // single short line, caret at end

	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "the one and only" {
		t.Errorf("Up on a single-line input should recall, got %q", got)
	}
	// Down from the newest restores the draft (single line is also the last line).
	typeKey(sw, tui.KeyDown)
	if got := sw.input.GetText(); got != "x" {
		t.Errorf("Down should restore the draft, got %q", got)
	}
}

// TestPromptHistoryEdgeGatingMultiline is the key multi-line gating test (issue
// #203): on a multi-logical-line input, Up recalls only from the first line and
// Down only from the last; from an interior position the arrow moves the caret
// between lines instead. This is what keeps history from breaking multi-line
// editing.
func TestPromptHistoryEdgeGatingMultiline(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	layout(sw, 80, 24, 3) // wide enough that "aaa"/"bbb" do not wrap

	submitText(sw, "HIST")

	// Caret on line 0 (first line): Up recalls.
	sw.input.SetText("aaa\nbbb")
	sw.input.CursorY, sw.input.CursorX = 0, 3
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "HIST" {
		t.Errorf("Up from the first line should recall, got %q", got)
	}

	// Caret on line 1 (an interior/last line, but not the first): Up moves the
	// caret up a line instead of recalling.
	sw.input.SetText("aaa\nbbb")
	sw.input.CursorY, sw.input.CursorX = 1, 3
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "aaa\nbbb" {
		t.Errorf("Up from a non-first line should keep the text, got %q", got)
	}
	if sw.input.CursorY != 0 {
		t.Errorf("Up from line 1 should move the caret to line 0, got CursorY=%d", sw.input.CursorY)
	}

	// Caret on line 0 (not the last line): Down moves the caret down a line
	// instead of recalling.
	sw.input.SetText("aaa\nbbb")
	sw.input.CursorY, sw.input.CursorX = 0, 3
	typeKey(sw, tui.KeyDown)
	if got := sw.input.GetText(); got != "aaa\nbbb" {
		t.Errorf("Down from a non-last line should keep the text, got %q", got)
	}
	if sw.input.CursorY != 1 {
		t.Errorf("Down from line 0 should move the caret to line 1, got CursorY=%d", sw.input.CursorY)
	}
}

// TestPromptHistoryEdgeGatingWrappedLine exercises the character-wrap math
// specifically: when one logical line wraps across several visual rows, a caret
// on a middle continuation row does NOT trigger recall (it is neither on the
// first nor the last visual row), while the top and bottom rows do (for Up and
// Down respectively).
func TestPromptHistoryEdgeGatingWrappedLine(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	submitText(sw, "HIST")

	// Force a narrow input so a 12-rune line wraps to 3 visual rows at width 5
	// (rows: [0,5) [5,10) [10,12)).
	sw.input.Component.SetBounds(tv.Rect{X: 0, Y: 0, W: 6, H: 3}) // contentWidth = 5

	// Caret in the middle (row 1): Up must NOT recall — it moves within the wrap.
	sw.input.SetText("abcdefghijkl")
	sw.input.CursorY, sw.input.CursorX = 0, 7
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "abcdefghijkl" {
		t.Errorf("Up from a wrapped continuation row should not recall, got %q", got)
	}

	// Caret at top (row 0): Up recalls.
	sw.input.SetText("abcdefghijkl")
	sw.input.CursorY, sw.input.CursorX = 0, 0
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "HIST" {
		t.Errorf("Up from the first visual row should recall, got %q", got)
	}

	// Caret in the middle (row 1): Down must NOT recall.
	sw.input.SetText("abcdefghijkl")
	sw.input.CursorY, sw.input.CursorX = 0, 7
	typeKey(sw, tui.KeyDown)
	if got := sw.input.GetText(); got != "abcdefghijkl" {
		t.Errorf("Down from a wrapped continuation row should not recall, got %q", got)
	}

	// Caret at the very end (row 2 = last visual row): Down DOES reach history,
	// but we are not navigating, so historyNext declines and the caret is free.
	sw.input.SetText("abcdefghijkl")
	sw.input.CursorY, sw.input.CursorX = 0, 12
	typeKey(sw, tui.KeyDown)
	if got := sw.input.GetText(); got != "abcdefghijkl" {
		t.Errorf("Down from the last visual row while not navigating should keep text, got %q", got)
	}
}

// TestPromptHistoryCompleterPopupPrecedence verifies the @-mention popup keeps
// Up/Down priority when it is open (issue #203 conflict-resolution rule #1): the
// keys move the popup selection and never reach history, so the input text is
// unchanged.
func TestPromptHistoryCompleterPopupPrecedence(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	layout(sw, 80, 24, 3) // give the popup real bounds to anchor against

	// Seed history so a missed-precedence bug would be observable (the input
	// would change to a history entry).
	submitText(sw, "HIST")

	// Wire the workspace-file list so the completer can find matches.
	w.handlers.ListWorkspaceFiles = func() []string {
		return []string{"aaa.go", "aab.go", "bbb.go"}
	}

	// Open the popup by entering a mention token and refreshing the completer.
	sw.input.SetText("@aa")
	sw.completer.update()
	if !sw.completer.active() {
		t.Fatal("mention popup should be open for @aa")
	}
	list := sw.completer.list
	if list == nil || list.Selected() == nil {
		t.Fatal("popup list has no initial selection")
	}
	sel0 := list.Selected().Label

	// Down: the popup must keep the key — selection advances, input untouched.
	typeKey(sw, tui.KeyDown)
	if !sw.completer.active() {
		t.Error("popup should stay open after Down")
	}
	if got := sw.input.GetText(); got != "@aa" {
		t.Errorf("Down should navigate the popup, not recall history; input = %q", got)
	}
	if list.Selected() == nil || list.Selected().Label == sel0 {
		t.Errorf("popup selection should advance on Down: before=%q after=%v", sel0, list.Selected())
	}

	// Up: selection moves back, input still untouched.
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "@aa" {
		t.Errorf("Up should navigate the popup, not recall history; input = %q", got)
	}
}

// TestPromptHistoryUnicodeCaret is a byte-vs-rune guard: a prompt of multi-byte
// runes (emoji) must recall with the caret counted in RUNES at the end, not bytes.
// The caret math and GetText/SetText all flow through []rune; this pins that they
// agree so a recalled "😀😀" leaves CursorX at 2, not 8.
func TestPromptHistoryUnicodeCaret(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	const prompt = "😀😀café" // 6 runes, 12 bytes
	submitText(sw, prompt)
	if got := sw.promptHistory[0]; got != prompt {
		t.Fatalf("recorded = %q, want %q", got, prompt)
	}
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != prompt {
		t.Fatalf("recall = %q, want %q", got, prompt)
	}
	wantX := len([]rune(prompt))
	if sw.input.CursorX != wantX {
		t.Errorf("CursorX = %d (bytes?), want %d runes for %q", sw.input.CursorX, wantX, prompt)
	}
	if !caretAtEnd(sw) {
		t.Error("caret not at end after recalling a multi-byte-rune prompt")
	}
}

// TestPromptHistoryDraftIsFreshEachCycle verifies the stashed draft is re-read on
// every fresh Up, not frozen at the original: after restoring a draft, editing
// it, and navigating again, the edited draft comes back. This guards against a
// regression where historyDraft is only ever set once.
func TestPromptHistoryDraftIsFreshEachCycle(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	submitText(sw, "only")
	sw.input.SetText("draft v1")

	// Cycle once: Up recalls, Down restores "draft v1".
	typeKey(sw, tui.KeyUp)
	typeKey(sw, tui.KeyDown)
	if got := sw.input.GetText(); got != "draft v1" {
		t.Fatalf("first restore = %q, want draft v1", got)
	}
	// Edit the restored draft, then cycle again: the EDITED draft must return.
	sw.input.SetText("draft v2")
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "only" {
		t.Fatalf("Up = %q, want only", got)
	}
	typeKey(sw, tui.KeyDown)
	if got := sw.input.GetText(); got != "draft v2" {
		t.Errorf("second restore = %q, want the edited draft v2 (draft should be re-stashed each cycle)", got)
	}
}

// TestPromptHistoryDedupContract pins the exact de-dup contract: only a prompt
// identical to the MOST RECENT entry is skipped. Re-submitting an older entry
// (different from the most recent) is appended again — it is not a consecutive
// duplicate. This documents the chosen behaviour so a future tightening/loosening
// of the rule is a conscious change.
func TestPromptHistoryDedupContract(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	submitText(sw, "one")
	submitText(sw, "two")
	// Re-submit the OLDEST unchanged: most-recent is "two", so "one" is kept.
	submitText(sw, "one")
	if got := sw.promptHistory; !reflect.DeepEqual(got, []string{"one", "two", "one"}) {
		t.Errorf("non-consecutive dup should be kept, got %v", got)
	}
}

// TestPromptHistoryModifiersDoNotRecall is the regression guard for the
// modifier-handling fix: a modifier turns an arrow into a gesture the input owns
// (Shift+arrow extends the selection), so history must NOT intercept it even at a
// buffer edge where plain Up/Down would recall. The plain-Up case is included as
// a control to prove the setup would otherwise recall.
func TestPromptHistoryModifiersDoNotRecall(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	submitText(sw, "HIST")

	// Control: plain Up at the first visual line DOES recall.
	sw.input.SetText("abc")
	typeKey(sw, tui.KeyUp)
	if got := sw.input.GetText(); got != "HIST" {
		t.Fatalf("control: plain Up should recall, got %q", got)
	}

	// Every modifier + arrow must leave the buffer untouched (no recall).
	for _, tc := range []struct {
		name string
		ev   tui.TypeEvent
	}{
		{"Shift+Up", tui.TypeEvent{Key: tui.KeyUp, Shift: true}},
		{"Shift+Down", tui.TypeEvent{Key: tui.KeyDown, Shift: true}},
		{"Ctrl+Up", tui.TypeEvent{Key: tui.KeyUp, Ctrl: true}},
		{"Ctrl+Down", tui.TypeEvent{Key: tui.KeyDown, Ctrl: true}},
		{"Alt+Up", tui.TypeEvent{Key: tui.KeyUp, Alt: true}},
		{"Alt+Down", tui.TypeEvent{Key: tui.KeyDown, Alt: true}},
	} {
		sw.input.SetText("abc")
		consumed := sw.input.Component.OnTypeFn(sw.input.Component, tc.ev)
		if got := sw.input.GetText(); got != "abc" {
			t.Errorf("%s recalled history (%q); modifier+arrow must not trigger recall", tc.name, got)
		}
		// And history itself must report it did not handle the event.
		if sw.handleHistoryKey(tc.ev) {
			t.Errorf("%s: handleHistoryKey should return false for a modifier+arrow", tc.name)
		}
		// 'consumed' is intentionally not asserted: Shift+Up may be handled by the
		// input's selection logic (true) while Ctrl+Up is unhandled (false); the
		// contract under test is only "history did not recall".
		_ = consumed
	}
}

// TestPromptHistoryRecallDoesNotReopenMentionPopup is the regression guard for the
// post-recall popup fix: recalling a prompt that itself ends in an @mention token
// (caret lands inside it) must NOT reopen the completer popup. History stores the
// raw typed text, so this is a realistic recall.
func TestPromptHistoryRecallDoesNotReopenMentionPopup(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	layout(sw, 80, 24, 3)
	w.handlers.ListWorkspaceFiles = func() []string { return []string{"main.go"} }

	submitText(sw, "see @main.go")
	sw.input.SetText("")

	typeKey(sw, tui.KeyUp) // recall "see @main.go"; caret lands after "@main.go"
	if got := sw.input.GetText(); got != "see @main.go" {
		t.Fatalf("recall = %q, want the mention prompt", got)
	}
	if sw.completer.active() {
		t.Errorf("recalling an @mention prompt should not reopen the completer popup")
	}

	// Sanity: typing at the same position DOES still open the popup (the fix only
	// suppresses the recall side-effect, not normal mention completion).
	sw.input.SetText("see @main.go")
	sw.input.CursorX = len([]rune("see @main.go")) // caret at end, inside the token
	sw.completer.update()
	if !sw.completer.active() {
		t.Error("typing into a mention token should still open the popup (control)")
	}
}

// TestPromptHistoryNavHelpersSafeOnEmpty pins the empty-history safety of the
// navigation helpers directly: calling them with no history must return false and
// not panic, even though handleHistoryKey is the only production caller (which
// already guards). This guards the latent footgun where historyPrev indexed
// promptHistory[-1].
func TestPromptHistoryNavHelpersSafeOnEmpty(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	if sw.historyPrev() {
		t.Error("historyPrev() on empty history should return false")
	}
	if sw.historyNext() {
		t.Error("historyNext() on empty history should return false")
	}
	// Buffer and nav state untouched.
	if got := sw.input.GetText(); got != "" {
		t.Errorf("empty-history nav changed the buffer: %q", got)
	}
}

// --- Visual-line math unit tests -------------------------------------------
//
// These pin the pure helpers that decide when Up/Down recall vs. move the caret,
// independent of the session machinery. They are the most likely place for an
// off-by-one, so they are spelled out as tables.

func TestInputVisualWidth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		boundsW int
		want    int
	}{
		{"unset (before layout)", 0, 0},
		{"single column", 1, 0},
		{"two columns", 2, 1},
		{"typical", 70, 69},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := tv.NewMultiLineInput("x", tv.Rect{W: tc.boundsW})
			if got := inputVisualWidth(in); got != tc.want {
				t.Errorf("inputVisualWidth(W=%d) = %d, want %d", tc.boundsW, got, tc.want)
			}
		})
	}
}

func TestLineVisualRows(t *testing.T) {
	// width 5 (W=6): "abcde" (5) → 1 row; "abcdef" (6) → 2; "" → 1.
	for _, tc := range []struct {
		name  string
		text  string
		width int // contentWidth
		want  int
	}{
		{"empty is one row", "", 5, 1},
		{"exactly one row", "abcde", 5, 1},
		{"one past wraps to two", "abcdef", 5, 2},
		{"exact multiple", "aaaaa", 5, 1},
		{"three rows", "abcdefghijkl", 5, 3},
		{"unwrapped width treats line as one row", "a very long line", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := tv.NewMultiLineInput(tc.text, tv.Rect{})
			in.Component.SetBounds(tv.Rect{W: tc.width + 1}) // contentWidth = width
			if got := lineVisualRows(in, 0); got != tc.want {
				t.Errorf("lineVisualRows(%q, width=%d) = %d, want %d", tc.text, tc.width, got, tc.want)
			}
		})
	}
}

func TestVisualRowInLine(t *testing.T) {
	// width 5 (W=6): 12-rune line → rows [0,5)[5,10)[10,12).
	in := tv.NewMultiLineInput("abcdefghijkl", tv.Rect{})
	in.Component.SetBounds(tv.Rect{W: 6})
	for _, tc := range []struct {
		cursorX int
		want    int
	}{
		{0, 0},  // start of first row
		{4, 0},  // still first row
		{5, 1},  // start of second row
		{9, 1},  // still second row
		{10, 2}, // start of last row
		{12, 2}, // end clamps to last row
	} {
		if got := visualRowInLine(in, 0, tc.cursorX); got != tc.want {
			t.Errorf("visualRowInLine(cursorX=%d) = %d, want %d", tc.cursorX, got, tc.want)
		}
	}

	// Unwrapped width: always row 0.
	in2 := tv.NewMultiLineInput("abcdefghijkl", tv.Rect{})
	if got := visualRowInLine(in2, 0, 12); got != 0 {
		t.Errorf("visualRowInLine with unwrapped width = %d, want 0", got)
	}
}

// TestCaretEdgePredicates covers caretOnFirstVisualLine / caretOnLastVisualLine
// across the cases that matter for gating: single line (both true), wrapped
// first/last rows, and interior logical lines.
func TestCaretEdgePredicates(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	// Single short line: caret is on both the first and last visual line.
	sw.input.Component.SetBounds(tv.Rect{W: 70})
	sw.input.SetText("hi")
	if !sw.caretOnFirstVisualLine() {
		t.Error("single line: caret should be on the first visual line")
	}
	if !sw.caretOnLastVisualLine() {
		t.Error("single line: caret should be on the last visual line")
	}

	// Wrapped single line (width 5): caret at the end is on the last row but
	// NOT the first.
	sw.input.Component.SetBounds(tv.Rect{W: 6})
	sw.input.SetText("abcdefghijkl") // 3 rows
	sw.input.CursorY, sw.input.CursorX = 0, 12
	if sw.caretOnFirstVisualLine() {
		t.Error("caret at end of a wrapped line is not on the first visual row")
	}
	if !sw.caretOnLastVisualLine() {
		t.Error("caret at end of a wrapped line should be on the last visual row")
	}
	// Caret at the start is on the first row but NOT the last.
	sw.input.CursorX = 0
	if !sw.caretOnFirstVisualLine() {
		t.Error("caret at start of a wrapped line should be on the first visual row")
	}
	if sw.caretOnLastVisualLine() {
		t.Error("caret at start of a wrapped line is not on the last visual row")
	}

	// Two logical lines: caret on line 0 is first-but-not-last; line 1 is
	// last-but-not-first.
	sw.input.Component.SetBounds(tv.Rect{W: 70})
	sw.input.SetText("aaa\nbbb")
	sw.input.CursorY, sw.input.CursorX = 0, 3
	if !sw.caretOnFirstVisualLine() || sw.caretOnLastVisualLine() {
		t.Error("caret on line 0 of two: first=yes, last=no")
	}
	sw.input.CursorY, sw.input.CursorX = 1, 3
	if sw.caretOnFirstVisualLine() || !sw.caretOnLastVisualLine() {
		t.Error("caret on line 1 of two: first=no, last=yes")
	}
}
