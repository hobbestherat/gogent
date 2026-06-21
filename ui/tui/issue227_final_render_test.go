package ui

import (
	"errors"
	"strings"
	"testing"

	"gogent/internal/agent"
)

// issue227FinalSentinel is the answer text delivered as a SessionEventFinal. It is
// deliberately free of Markdown-significant characters (no _ * ` # > - !) so that
// the rich-render path (assistant answers are rendered as formatted Markdown) lays
// the runes down verbatim and a screen-cell scan can find them character-for-
// character. It is distinctive enough not to collide with filler content.
const issue227FinalSentinel = "SENTINELFINAL9X"

// issue227FillOverflow adds enough user records to force the transcript past its
// viewport, each carrying a unique marker, and returns the first and last markers
// so a test can assert the top-anchored "scrolled up" precondition (first visible,
// last not) before delivering the final.
func issue227FillOverflow(sw *SessionWindow, n int) (first, last string) {
	for i := 0; i < n; i++ {
		marker := paddedMarker(i)
		sw.addUser("ROW-" + marker)
		if i == 0 {
			first = "ROW-" + marker
		}
		last = "ROW-" + marker
	}
	return first, last
}

// paddedMarker renders i as a zero-padded 4-digit string so record markers sort
// lexicographically in insertion order and never collide with the sentinel.
func paddedMarker(i int) string {
	const digits = "0123456789"
	var b [4]byte
	for j := 3; j >= 0; j-- {
		b[j] = digits[i%10]
		i /= 10
	}
	return string(b[:])
}

// silenceNotifications disables the workbench's terminal notifier so a
// SessionEventFinal/Error (which maybeNotify turns into an OSC notification
// sequence written to os.Stdout) does not spew escape codes into the test log.
// Delivery is otherwise unaffected: maybeNotify no-ops when w.notify is nil.
func silenceNotifications(w *Workbench) {
	w.notify = nil
}

// screenText renders the whole workbench frame and returns the concatenation of
// every on-screen cell row, joined by newlines. It is the headless equivalent of
// "what the user sees": only records scrolled into the transcript viewport appear,
// whereas transcript.view.AllText() returns every appended entry regardless of
// scroll position (issue #227 — the bug hid because AllText always had the answer).
func screenText(w *Workbench) string {
	w.desktop.Redraw()
	var b strings.Builder
	for y := 0; y < w.app.Height(); y++ {
		for x := 0; x < w.app.Width(); x++ {
			ch := w.app.ReadCell(x, y).Ch
			if ch == 0 {
				ch = ' '
			}
			b.WriteRune(ch)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// containsOnScreen reports whether needle appears in any rendered row.
func containsOnScreen(screen, needle string) bool {
	return strings.Contains(screen, needle)
}

// TestIssue227_FinalAnswerVisibleWhenScrolledUp is the integration repro for issue
// #227: a SessionEventFinal driven through the real live delivery seam
// (Workbench.deliverSessionEvent) must reach the rendered transcript VIEW — not
// merely AllText — even when the user had scrolled up to read earlier output
// mid-turn. Without the addAssistant re-anchor (scrollToBottom before the append)
// the final lands in AllText but is scrolled off-screen: the agent looks like it
// stopped replying. It must FAIL without the fix and PASS with it.
func TestIssue227_FinalAnswerVisibleWhenScrolledUp(t *testing.T) {
	w := newTestWorkbench(t)
	silenceNotifications(w)
	sw := w.openWindow("s1", "Session 1")

	// Fill the transcript well past the viewport and establish a "scrolled up"
	// posture (follow off, anchored at the top).
	first, last := issue227FillOverflow(sw, 40)
	sw.transcript.view.ScrollToTop()

	// Precondition: the transcript really does overflow and is top-anchored, so the
	// top record is on screen and the bottom record is off-screen. If this fails the
	// test environment's viewport is too tall to reproduce, and the assertion below
	// would pass vacuously — so fail loudly instead.
	screen := screenText(w)
	if !containsOnScreen(screen, first) {
		t.Fatalf("precondition: top record %q not on screen after ScrollToTop — viewport too tall?\n%s", first, screen)
	}
	if containsOnScreen(screen, last) {
		t.Fatalf("precondition: bottom record %q already on screen — transcript does not overflow, repro is invalid\n%s", last, screen)
	}

	// Sanity: the answer is not on screen yet.
	if containsOnScreen(screen, issue227FinalSentinel) {
		t.Fatalf("sentinel already on screen before the final was delivered")
	}

	// The real live seam (the body of EmitSessionEvent's desktop.Post callback).
	delivered := w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventFinal, Text: issue227FinalSentinel})
	if !delivered {
		t.Fatalf("deliverSessionEvent reported the final was not delivered to an open window")
	}

	screen = screenText(w)
	if !containsOnScreen(screen, issue227FinalSentinel) {
		t.Fatalf("issue #227: final answer %q is in AllText but NOT on screen after delivery — dropped from the live render\n%s",
			issue227FinalSentinel, screen)
	}
	// And it is genuinely in the model (the persisted/on-disk equivalent), which the
	// bug never lost — this anchors the "data fine, render broken" framing.
	if !strings.Contains(sw.transcript.view.AllText(), issue227FinalSentinel) {
		t.Fatalf("final answer missing from AllText — the data path itself regressed")
	}
}

// newIssue227Workbench is a real Workbench with a single open, focused session
// and the terminal notifier silenced, ready for a live delivery-seam test.
func newIssue227Workbench(t *testing.T) (*Workbench, *SessionWindow) {
	t.Helper()
	w := newTestWorkbench(t)
	silenceNotifications(w)
	sw := w.openWindow("s1", "Session 1")
	return w, sw
}

// requireScrolledUp asserts the transcript is genuinely overflowing and pinned to
// the top: the first filler record is on screen, the last is not. A test that
// relies on "the final is off-screen without the fix" is only meaningful while
// this holds, so it fails loudly instead of passing vacuously on a huge viewport.
func requireScrolledUp(t *testing.T, w *Workbench, first, last string) {
	t.Helper()
	screen := screenText(w)
	if !containsOnScreen(screen, first) {
		t.Fatalf("scrolled-up precondition broken: top record %q not on screen\n%s", first, screen)
	}
	if containsOnScreen(screen, last) {
		t.Fatalf("scrolled-up precondition broken: bottom record %q on screen (no overflow)\n%s", last, screen)
	}
}

// TestIssue227_FinalVisibleAtBottomByDefault is the happy-path counterpart to the
// scrolled-up repro: when the user is already following the stream (the default),
// the final answer lands on screen too. Guards against a fix that only handles the
// scrolled-up case and regresses the common case.
func TestIssue227_FinalVisibleAtBottomByDefault(t *testing.T) {
	w, sw := newIssue227Workbench(t)
	issue227FillOverflow(sw, 40)
	// No ScrollToTop: follow stays on (the default for a fresh view).
	if delivered := w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventFinal, Text: issue227FinalSentinel}); !delivered {
		t.Fatalf("final not delivered to open window")
	}
	if !containsOnScreen(screenText(w), issue227FinalSentinel) {
		t.Fatalf("final answer not on screen in the default follow-on state\n%s", screenText(w))
	}
}

// TestIssue227_ErrorRevealedWhenScrolledUp mirrors the final-answer repro for the
// turn-ending error path (addError). A user who scrolled up mid-turn must still see
// why the turn ended. Shares the addAssistant re-anchor mechanism, so it must also
// fail without the fix.
func TestIssue227_ErrorRevealedWhenScrolledUp(t *testing.T) {
	w, sw := newIssue227Workbench(t)
	first, last := issue227FillOverflow(sw, 40)
	sw.transcript.view.ScrollToTop()
	requireScrolledUp(t, w, first, last)

	const sentinel = "SENTINELError7Y"
	if delivered := w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventError, Err: errors.New(sentinel)}); !delivered {
		t.Fatalf("error event not delivered to open window")
	}
	if !containsOnScreen(screenText(w), sentinel) {
		t.Fatalf("issue #227: turn-ending error %q not revealed when scrolled up\n%s", sentinel, screenText(w))
	}
}

// TestIssue227_StreamingDoesNotYankScrolledUser is the counter-property that keeps
// the fix honest: intermediate/streaming events (thoughts, tool calls and results)
// must NOT re-anchor the view, so a user reading scrolled-up history mid-turn is
// not yanked to the bottom on every step. Only the turn-ending final/error reveals.
// This is a regression guard: if a future change adds a re-anchor to a streaming
// path, this fails.
func TestIssue227_StreamingDoesNotYankScrolledUser(t *testing.T) {
	w, sw := newIssue227Workbench(t)
	first, last := issue227FillOverflow(sw, 40)
	sw.transcript.view.ScrollToTop()
	requireScrolledUp(t, w, first, last)

	const tool = "ZZSTREAMTOOL"
	// A representative streaming sequence within one turn: a thought, a tool call
	// and its result. None of these should move the scroll position.
	w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventAssistantStep, Text: "ZZSTREAMTHOUGHT"})
	w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventToolCall, CallID: "c1", Tool: tool, Args: map[string]interface{}{"q": "x"}})
	w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventToolResult, CallID: "c1", Tool: tool, Result: "ZZSTREAMRESULT"})

	screen := screenText(w)
	// The view stayed pinned at the top.
	if !containsOnScreen(screen, first) {
		t.Fatalf("streaming event yanked the view: top record %q no longer on screen\n%s", first, screen)
	}
	if containsOnScreen(screen, last) {
		t.Fatalf("streaming event yanked the view: bottom filler %q is now on screen\n%s", last, screen)
	}
	// And none of the just-streamed content was pulled into view (it appended below
	// the viewport). The thought, the tool name and the result must all stay off-screen.
	for _, marker := range []string{"ZZSTREAMTHOUGHT", "ZZSTREAMRESULT", tool} {
		if containsOnScreen(screen, marker) {
			t.Fatalf("streaming marker %q was yanked into view — streaming must not re-anchor\n%s", marker, screen)
		}
	}
	// Sanity: the streamed content IS in the model (just not scrolled into view).
	all := sw.transcript.view.AllText()
	if !strings.Contains(all, "ZZSTREAMRESULT") || !strings.Contains(all, tool) {
		t.Fatalf("streamed content missing from AllText — delivery regressed\n%s", all)
	}
}

// TestIssue227_EmptyFinalAddsNoRecord verifies an empty/whitespace-only final adds
// no transcript record and — because the empty check precedes the re-anchor — does
// not perturb the scroll position. A genuinely empty final is the #171 symptom
// class; this guards that the #227 re-anchor does not fire for it.
func TestIssue227_EmptyFinalAddsNoRecord(t *testing.T) {
	w, sw := newIssue227Workbench(t)
	first, last := issue227FillOverflow(sw, 40)
	sw.transcript.view.ScrollToTop()
	requireScrolledUp(t, w, first, last)
	before := len(sw.transcript.records)

	for _, empty := range []string{"", "   ", "\n\t  "} {
		w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventFinal, Text: empty})
	}
	if got := len(sw.transcript.records); got != before {
		t.Fatalf("empty finals added records: %d -> %d", before, got)
	}
	// Still pinned at the top: no re-anchor fired for an empty final.
	requireScrolledUp(t, w, first, last)
}

// TestIssue227_RichMarkdownAnswerRevealed drives the actual failure shape from the
// issue: a long Markdown answer with code fences (assistant answers are
// rich-rendered). The distinctive plain-text marker line must be visible on screen
// even when the user had scrolled up.
func TestIssue227_RichMarkdownAnswerRevealed(t *testing.T) {
	w, sw := newIssue227Workbench(t)
	first, last := issue227FillOverflow(sw, 40)
	sw.transcript.view.ScrollToTop()
	requireScrolledUp(t, w, first, last)

	// A realistic Markdown answer. The marker line carries no Markdown-significant
	// runes so the rich renderer lays it down verbatim and a cell scan finds it.
	answer := "Here is the sorted result.\n\n" +
		"```perl\nmy @a = sort @list;\n```\n\n" +
		"SENTINELRICHLINE4Z final summary of the work done above."
	w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventFinal, Text: answer})

	screen := screenText(w)
	if !containsOnScreen(screen, "SENTINELRICHLINE4Z") {
		t.Fatalf("rich Markdown final marker not on screen when scrolled up\n%s", screen)
	}
}

// TestIssue227_FilterActiveRevealsFinal documents that while a search/filter is
// active, add() takes the full render() path (which ends in ScrollToBottom), so the
// final is revealed independently of the addAssistant re-anchor. This confirms the
// fix is consistent with the filter path and a filtered view never hides a new
// final that matches the query.
func TestIssue227_FilterActiveRevealsFinal(t *testing.T) {
	w, sw := newIssue227Workbench(t)
	issue227FillOverflow(sw, 40)
	// Enable a filter that the final matches, then scroll up within the filtered view.
	sw.transcript.setQuery(strings.ToLower(issue227FinalSentinel))
	sw.transcript.view.ScrollToTop()

	w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventFinal, Text: issue227FinalSentinel})
	if !containsOnScreen(screenText(w), issue227FinalSentinel) {
		t.Fatalf("final not on screen while a matching filter is active\n%s", screenText(w))
	}
}

// TestIssue227_DeliverSessionEventKnownWindowReturnsTrue checks the delivery seam's
// contract: an event for an open window is applied (returns true) and is NOT
// counted as undelivered.
func TestIssue227_DeliverSessionEventKnownWindowReturnsTrue(t *testing.T) {
	w, _ := newIssue227Workbench(t)
	if w.UndeliveredEventCount() != 0 {
		t.Fatalf("fresh workbench should have 0 undelivered events, got %d", w.UndeliveredEventCount())
	}
	delivered := w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventFinal, Text: "x"})
	if !delivered {
		t.Fatalf("deliverSessionEvent for open window returned false")
	}
	if got := w.UndeliveredEventCount(); got != 0 {
		t.Fatalf("delivered-to-open-window event counted as undelivered: %d", got)
	}
}

// TestIssue227_DeliverSessionEventUnknownWindowCountsUndelivered verifies the
// observability hook: an event for an id with no open window returns false and is
// counted via UndeliveredEventCount instead of being silently dropped — the exact
// regression (suspect #1) that let a final vanish with no trace.
func TestIssue227_DeliverSessionEventUnknownWindowCountsUndelivered(t *testing.T) {
	w, _ := newIssue227Workbench(t) // window open for "s1" only

	delivered := w.deliverSessionEvent("ghost", agent.SessionEvent{Type: agent.SessionEventFinal, Text: "x"})
	if delivered {
		t.Fatalf("deliverSessionEvent for unknown window returned true")
	}
	if got := w.UndeliveredEventCount(); got != 1 {
		t.Fatalf("UndeliveredEventCount = %d after one event to unknown id, want 1", got)
	}
	// Each subsequent undelivered event accumulates.
	w.deliverSessionEvent("ghost2", agent.SessionEvent{Type: agent.SessionEventError, Err: errors.New("boom")})
	w.deliverSessionEvent("ghost3", agent.SessionEvent{Type: agent.SessionEventFinal, Text: "y"})
	if got := w.UndeliveredEventCount(); got != 3 {
		t.Fatalf("UndeliveredEventCount = %d after three unknown-id events, want 3", got)
	}
	// A delivered event for the open window does not change the count.
	w.deliverSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventFinal, Text: "z"})
	if got := w.UndeliveredEventCount(); got != 3 {
		t.Fatalf("UndeliveredEventCount changed after a delivered event: %d, want 3", got)
	}
}

// TestIssue227_EmitSessionEventQueuesNotSynchronous documents why the test seam is
// deliverSessionEvent and not EmitSessionEvent: EmitSessionEvent marshals via
// desktop.Post, whose closure only runs on the UI loop goroutine (turbotui's
// drainPosts is unexported and is never invoked under headless test). So immediately
// after EmitSessionEvent the final is NOT yet applied. This must stay true so the
// async seam remains the thing a real integration test has to account for.
func TestIssue227_EmitSessionEventQueuesNotSynchronous(t *testing.T) {
	w, sw := newIssue227Workbench(t)

	// Must not panic from a background goroutine stand-in.
	w.EmitSessionEvent("s1", agent.SessionEvent{Type: agent.SessionEventFinal, Text: issue227FinalSentinel})

	// The posted closure has not run (no loop draining the post-queue under test),
	// so the final is not yet in the transcript...
	if strings.Contains(sw.transcript.view.AllText(), issue227FinalSentinel) {
		t.Fatalf("EmitSessionEvent applied the final synchronously — post-queue drained unexpectedly")
	}
	// ...and, importantly, it was NOT silently counted as undelivered either: it is
	// queued, not dropped. A nil-window drop is the only thing UndeliveredEventCount
	// tracks.
	if got := w.UndeliveredEventCount(); got != 0 {
		t.Fatalf("queued (not dropped) event counted as undelivered: %d", got)
	}
}
