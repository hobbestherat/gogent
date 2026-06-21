package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/agent"
)

// recordInject installs an OnInject handler that forwards every interjected
// message over a buffered channel, so a test can observe what the (goroutine-
// dispatched) interject path actually handed to the backend. Mirrors recordSends
// for the OnInject path added by issue #201.
func recordInject(w *Workbench) <-chan string {
	injected := make(chan string, 8)
	w.handlers.OnInject = func(_, message string) { injected <- message }
	return injected
}

// waitInject reads one interjected message or fails the test if none arrives.
// interject() dispatches OnInject on a goroutine, so a small timeout decouples
// the assertion from scheduling.
func waitInject(t *testing.T, injected <-chan string) string {
	t.Helper()
	select {
	case m := <-injected:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an injected message")
		return ""
	}
}

// noInject asserts that nothing is interjected within a short window.
func noInject(t *testing.T, injected <-chan string) {
	t.Helper()
	select {
	case m := <-injected:
		t.Fatalf("unexpected injected message %q", m)
	case <-time.After(150 * time.Millisecond):
	}
}

// recordStop installs an OnStop handler that forwards the cancelled session id
// over a buffered channel, so a test can observe the Stop button / /stop action.
func recordStop(w *Workbench) <-chan string {
	stopped := make(chan string, 4)
	w.handlers.OnStop = func(id string) { stopped <- id }
	return stopped
}

// waitStop reads one stopped id or fails the test if none arrives.
func waitStop(t *testing.T, stopped <-chan string) string {
	t.Helper()
	select {
	case id := <-stopped:
		return id
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OnStop")
		return ""
	}
}

// layout drives the busy-state input-row layout for a window of the given content
// width/height, the way a real draw would. It is a thin wrapper so the layout
// tests spell out the geometry they exercise.
func layout(sw *SessionWindow, wd, ht, inputH int) {
	sw.layoutInputRow(wd, ht, inputH)
}

// boundsOf returns a button's current bounds for layout assertions.
func boundsOf(b *tv.Button) tv.Rect { return b.Component.Bounds }

// rectsMeet reports whether two rects touch or overlap (their column ranges
// intersect). Used to assert the input-row widgets never collide.
func rectsMeet(a, b tv.Rect) bool {
	if a.Empty() || b.Empty() {
		return false
	}
	return a.X < b.X+b.W && b.X < a.X+a.W
}

// TestButtonWidthHelper checks the running-button sizing helpers (issue #201):
// buttonWidth is the label plus the "[ … ]" frame (so "Send" stays the 8 cells
// the idle row already reserves), and runningButtonsWidth folds in the inter-
// button gaps and right margin.
func TestButtonWidthHelper(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  int
	}{
		{"Send", 8},       // 4-cell label + 4-cell frame — matches the idle Send button
		{"Interject", 13}, // 9 + 4
		{"Queue ⏎", 11},   // 7 + 4
		{"■ Stop", 10},    // 6 + 4
		{"»", 5},          // glyph + frame
		{"⏎", 5},
		{"■", 5},
	} {
		if got := buttonWidth(tc.label); got != tc.want {
			t.Errorf("buttonWidth(%q) = %d, want %d", tc.label, got, tc.want)
		}
	}
	// Full labels, uniformly sized (issue #214): the three buttons share the
	// widest label's width (Interject → 13), so the footprint is 3*13 + two gaps
	// + right margin = 42 (was 37 when each button sized to its own label).
	if got := runningButtonsWidth(interjectLabel, queueLabel, stopLabel); got != 42 {
		t.Errorf("runningButtonsWidth(full) = %d, want 42 (3*uniformButtonWidth + gaps + margin)", got)
	}
	// Glyphs: 5 + 5 + 5 + two gaps + right margin (all glyphs already equal).
	if got := runningButtonsWidth(interjectGlyph, queueGlyph, stopGlyph); got != 18 {
		t.Errorf("runningButtonsWidth(glyph) = %d, want 18", got)
	}
}

// TestIdleRowShowsSendHidesRunningButtons verifies the idle input row (issue
// #201): the single Send button is laid out at the right with its original
// geometry, and the three running-turn buttons carry zero bounds so only Send is
// visible. The prompt keeps its full idle width.
func TestIdleRowShowsSendHidesRunningButtons(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	const wd, ht, inputH = 80, 24, 3

	layout(sw, wd, ht, inputH)

	send := boundsOf(sw.sendButton)
	if send.Empty() {
		t.Fatal("idle row should show the Send button")
	}
	if send.X != wd-9 || send.W != 8 {
		t.Errorf("idle Send bounds = %+v, want X=%d W=8", send, wd-9)
	}
	for name, b := range map[string]*tv.Button{
		"interject": sw.interjectButton,
		"queue":     sw.queueButton,
		"stop":      sw.stopButton,
	} {
		if r := boundsOf(b); !r.Empty() {
			t.Errorf("idle row should hide the %s button, got bounds %+v", name, r)
		}
	}
	if r := boundsOfInput(sw); r.W != wd-10 {
		t.Errorf("idle input width = %d, want %d", r.W, wd-10)
	}
}

// boundsOfInput returns the input box's current bounds.
func boundsOfInput(sw *SessionWindow) tv.Rect { return sw.input.Component.Bounds }

// TestBusyRowShowsRunningButtonsHidesSend verifies the busy input row (issue
// #201): Send is hidden and the three running-turn buttons appear right-aligned
// in the recommended order — [ Interject ] [ Queue ⏎ ] [ ■ Stop ] — with Stop on
// the far right and the prompt shrunk to the room left of them.
func TestBusyRowShowsRunningButtonsHidesSend(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	const wd, ht, inputH = 80, 24, 3

	layout(sw, wd, ht, inputH)

	if r := boundsOf(sw.sendButton); !r.Empty() {
		t.Errorf("busy row should hide Send, got bounds %+v", r)
	}
	interject := boundsOf(sw.interjectButton)
	queue := boundsOf(sw.queueButton)
	stop := boundsOf(sw.stopButton)
	for name, r := range map[string]tv.Rect{"interject": interject, "queue": queue, "stop": stop} {
		if r.Empty() {
			t.Errorf("busy row should show the %s button", name)
		}
	}
	// Button order left-to-right: interject, queue, stop (Stop far right).
	if interject.X >= queue.X || queue.X >= stop.X {
		t.Errorf("buttons not in [Interject][Queue][Stop] order: i=%+v q=%+v s=%+v", interject, queue, stop)
	}
	// Widths are uniform: all three share one width (the widest label's, issue
	// #214) rather than each sizing to its own label. On a wide window that is the
	// full-label uniform width (13); Queue/Stop are NOT their own (shorter) widths.
	wantW := uniformButtonWidth(interjectLabel, queueLabel, stopLabel)
	if interject.W != wantW || queue.W != wantW || stop.W != wantW {
		t.Errorf("button widths not uniform: i.W=%d q.W=%d s.W=%d, want all %d", interject.W, queue.W, stop.W, wantW)
	}
	if queue.W == buttonWidth(queueLabel) || stop.W == buttonWidth(stopLabel) {
		t.Errorf("a button reverted to per-label width: i=%+v q=%+v s=%+v", interject, queue, stop)
	}
	// Stop sits flush against the right margin.
	if got := stop.X + stop.W; got != wd-inputRowMargin {
		t.Errorf("stop right edge = %d, want %d", got, wd-inputRowMargin)
	}
	// No widget overlaps another (one-cell gaps between neighbours).
	in := boundsOfInput(sw)
	for _, pair := range [][2]tv.Rect{{in, interject}, {interject, queue}, {queue, stop}} {
		if rectsMeet(pair[0], pair[1]) {
			t.Errorf("input-row widgets overlap: %+v and %+v", pair[0], pair[1])
		}
	}
	// The prompt shrinks to make room for the buttons.
	if in.W <= 0 || in.W >= wd-10 {
		t.Errorf("busy input width = %d, want it shrunk below the idle %d", in.W, wd-10)
	}
}

// TestBusyRowLabelsAndStopColour checks the running buttons carry the recommended
// labels/glyphs and that Stop is rendered in the error colour to separate it from
// the two send-actions (issue #201).
func TestBusyRowLabelsAndStopColour(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	// Wide window: full labels.
	layout(sw, 80, 24, 3)
	if sw.interjectButton.Label != interjectLabel {
		t.Errorf("interject label = %q, want %q", sw.interjectButton.Label, interjectLabel)
	}
	if sw.queueButton.Label != queueLabel {
		t.Errorf("queue label = %q, want %q", sw.queueButton.Label, queueLabel)
	}
	if sw.stopButton.Label != stopLabel {
		t.Errorf("stop label = %q, want %q", sw.stopButton.Label, stopLabel)
	}
	if sw.stopButton.FG != colorError {
		t.Errorf("stop button FG = %v, want error colour %v", sw.stopButton.FG, colorError)
	}
}

// TestBusyRowDegradesToGlyphsOnNarrowWindow verifies the labels degrade to single
// glyphs when the window is too narrow to show them beside a usable prompt, and
// that the choice is recomputed on every layout pass (a narrow pass then a wide
// pass restores the full labels) (issue #201).
func TestBusyRowDegradesToGlyphsOnNarrowWindow(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	// Narrow window: glyphs.
	layout(sw, 30, 24, 3)
	if sw.interjectButton.Label != interjectGlyph || sw.queueButton.Label != queueGlyph || sw.stopButton.Label != stopGlyph {
		t.Errorf("narrow labels = %q/%q/%q, want glyphs",
			sw.interjectButton.Label, sw.queueButton.Label, sw.stopButton.Label)
	}
	// Layout is recomputed every draw: a subsequent wide pass restores the full set.
	layout(sw, 80, 24, 3)
	if sw.interjectButton.Label != interjectLabel || sw.queueButton.Label != queueLabel || sw.stopButton.Label != stopLabel {
		t.Errorf("wide labels after a narrow pass = %q/%q/%q, want full",
			sw.interjectButton.Label, sw.queueButton.Label, sw.stopButton.Label)
	}
}

// TestBusyRowLabelDegradationThreshold pins the exact width at which the labels
// flip between glyph and full form, and — crucially — that at the flip point the
// prompt gets exactly minInputWidth cells (issue #201). The threshold budget
// includes the one-cell gap between the prompt and the first button
// (runningButtonsWidth > wd-minInputWidth-inputRowGap); without that gap the
// prompt ended up a cell short of the floor, so this guards against the off-by-one
// regressing.
func TestBusyRowLabelDegradationThreshold(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	// One cell below the flip: still glyphs. Uniform sizing (issue #214) widened
	// the full footprint from 37 to 42, so the flip moved from wd=58 to wd=63.
	layout(sw, 62, 24, 3)
	if sw.interjectButton.Label != interjectGlyph {
		t.Errorf("wd=62 should use glyphs, got %q", sw.interjectButton.Label)
	}
	// At the flip (wd=63): full labels, and the prompt gets exactly minInputWidth.
	layout(sw, 63, 24, 3)
	if sw.interjectButton.Label != interjectLabel {
		t.Errorf("wd=63 should use full labels, got %q", sw.interjectButton.Label)
	}
	if in := boundsOfInput(sw).W; in != minInputWidth {
		t.Errorf("flip-point input width = %d, want exactly %d (minInputWidth)", in, minInputWidth)
	}
	// And the prompt only grows from there with the full labels.
	layout(sw, 80, 24, 3)
	if in := boundsOfInput(sw).W; in <= minInputWidth {
		t.Errorf("wd=80 input width = %d, want > %d", in, minInputWidth)
	}
}

// TestBusyRowNoOverlapAcrossWidths is a property check that, across realistic
// window widths, the prompt and the three running buttons never overlap and Stop
// stays flush with the right margin (issue #201). Widths start at 40, matching
// the window minimum, so every rect stays on screen.
func TestBusyRowNoOverlapAcrossWidths(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	for _, wd := range []int{40, 44, 50, 56, 57, 60, 70, 80, 100, 120} {
		layout(sw, wd, 24, 3)
		in := boundsOfInput(sw)
		interject := boundsOf(sw.interjectButton)
		queue := boundsOf(sw.queueButton)
		stop := boundsOf(sw.stopButton)
		if in.W < 1 {
			t.Errorf("wd=%d: input width %d must be >= 1", wd, in.W)
		}
		if got := stop.X + stop.W; got != wd-inputRowMargin {
			t.Errorf("wd=%d: stop right edge %d, want %d", wd, got, wd-inputRowMargin)
		}
		for _, pair := range [][2]tv.Rect{{in, interject}, {interject, queue}, {queue, stop}} {
			if rectsMeet(pair[0], pair[1]) {
				t.Errorf("wd=%d: widgets overlap: %+v and %+v", wd, pair[0], pair[1])
			}
		}
		// Buttons stay on screen for realistic widths.
		for name, r := range map[string]tv.Rect{"interject": interject, "queue": queue, "stop": stop} {
			if r.X < 0 {
				t.Errorf("wd=%d: %s button off-screen left at X=%d", wd, name, r.X)
			}
		}
	}
}

// TestBusyIdleSwapSwapsButtons verifies the button set follows the busy flag
// across transitions: busy shows the three running buttons, returning to idle
// hides them and restores Send (issue #201).
func TestBusyIdleSwapSwapsButtons(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	layout(sw, 80, 24, 3)
	if boundsOf(sw.sendButton).Empty() || !boundsOf(sw.stopButton).Empty() {
		t.Fatal("idle: Send should show, running buttons hidden")
	}

	sw.busy = true
	layout(sw, 80, 24, 3)
	if !boundsOf(sw.sendButton).Empty() || boundsOf(sw.stopButton).Empty() {
		t.Fatal("busy: Send should hide, running buttons shown")
	}

	sw.busy = false
	layout(sw, 80, 24, 3)
	if boundsOf(sw.sendButton).Empty() || !boundsOf(sw.stopButton).Empty() {
		t.Fatal("back to idle: Send should show again, running buttons hidden")
	}
}

// TestRunningButtonsHiddenViaVisibleWhenIdle is the regression guard for the
// focus-trap fix (issue #201): a hidden input-row button must drop out of the
// Tab-focus cycle, which turbotv's collectFocusable achieves by skipping
// !Visible components (it does NOT skip zero-bounds ones). So hiding is done by
// clearing Component.Visible, not merely zeroing the bounds. This test pins that
// exact condition — if anyone reverts to a bounds-only hide, Visible stays true
// and an invisible button would catch Tab and swallow Enter.
func TestRunningButtonsHiddenViaVisibleWhenIdle(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	layout(sw, 80, 24, 3)
	if !sw.sendButton.Component.Visible {
		t.Error("idle: Send should be Visible (focusable)")
	}
	for name, b := range map[string]*tv.Button{
		"interject": sw.interjectButton,
		"queue":     sw.queueButton,
		"stop":      sw.stopButton,
	} {
		if b.Component.Visible {
			t.Errorf("idle: %s button should be hidden (!Visible) so it leaves the focus cycle", name)
		}
		// Focusable stays true; the focus exclusion relies on Visible=false alone
		// (collectFocusable skips !Visible), which is what we assert above.
	}

	// Busy: the inverse — Send hidden, the three running buttons shown/focusable.
	sw.busy = true
	layout(sw, 80, 24, 3)
	if sw.sendButton.Component.Visible {
		t.Error("busy: Send should be hidden (!Visible)")
	}
	for name, b := range map[string]*tv.Button{
		"interject": sw.interjectButton,
		"queue":     sw.queueButton,
		"stop":      sw.stopButton,
	} {
		if !b.Component.Visible {
			t.Errorf("busy: %s button should be Visible", name)
		}
	}

	// Returning to idle flips Visible back, so the buttons re-enter/exit the focus
	// cycle with the state.
	sw.busy = false
	layout(sw, 80, 24, 3)
	if !sw.sendButton.Component.Visible || sw.stopButton.Component.Visible {
		t.Error("idle-again: Visible flags should flip back with the busy state")
	}
}

// TestBusyToIdleRestoresInputFocus is the regression guard for the focus-recovery
// fix (issue #201): when a turn ends while a running-turn button holds keyboard
// focus, focus must move back to the prompt. turbotv only re-homes a stale focus
// on layer add/remove/raise — not when a button's Visible flag flips during layout
// — so without this restore a focused-but-hidden button would swallow keystrokes
// until the user tabbed or clicked. The test drives the real desktop focus and the
// real busy→idle event path (apply → setBusy(false) → restoreInputFocusFromButtons).
func TestBusyToIdleRestoresInputFocus(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	for _, tc := range []struct {
		name string
		btn  *tv.Button
	}{
		{"interject", sw.interjectButton},
		{"queue", sw.queueButton},
		{"stop", sw.stopButton},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A turn is running and the user has Tab-focused a running button.
			sw.busy = true
			layout(sw, 80, 24, 3)
			w.desktop.SetFocus(tc.btn)
			if !tc.btn.Component.Focused() {
				t.Fatalf("setup failed: %s button should hold focus after SetFocus", tc.name)
			}

			// The terminal event ends the turn (the real busy→idle edge).
			sw.apply(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"})

			if !sw.input.Component.Focused() {
				t.Errorf("input should regain focus on busy→idle (focus was on %s button)", tc.name)
			}
			if tc.btn.Component.Focused() {
				t.Errorf("%s button should release focus on busy→idle", tc.name)
			}
		})
	}
}

// TestBusyToIdleDoesNotStealFocusFromOtherWidgets verifies the focus restore is
// targeted (issue #201): it only fires when a running button held focus, so a user
// interacting with the model selector mid-turn is not yanked back to the prompt
// when the turn ends.
func TestBusyToIdleDoesNotStealFocusFromOtherWidgets(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	layout(sw, 80, 24, 3)

	w.desktop.SetFocus(sw.modelSelect)
	if !sw.modelSelect.Component.Focused() {
		t.Fatal("setup failed: model selector should hold focus after SetFocus")
	}

	sw.apply(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"})

	if !sw.modelSelect.Component.Focused() {
		t.Error("focus should remain on the model selector, not be moved to the input")
	}
	if sw.input.Component.Focused() {
		t.Error("input should not steal focus when a non-button widget held it")
	}
}

// TestEnterWhileBusyQueuesDoesNotInject is the core flag-removal behaviour (issue
// #201): Enter while busy always takes the drain-on-idle queue path — it stows the
// pending slot and never hands the text to OnInject, no matter what. With the old
// experimental flag the same keystroke could inject instead; that branch is gone.
func TestEnterWhileBusyQueuesDoesNotInject(t *testing.T) {
	w := newTestWorkbench(t)
	sent := recordSends(w)
	injected := recordInject(w)
	sw := w.openWindow("s", "S")

	// First message dispatches and marks the window busy.
	sw.input.SetText("first")
	sw.submitFn()
	if got := waitSend(t, sent); got != "first" {
		t.Fatalf("first send = %q, want %q", got, "first")
	}
	if !sw.busy {
		t.Fatal("window should be busy after the first submit")
	}

	// Enter while busy queues; it must NOT inject.
	sw.input.SetText("clarify later")
	sw.submitFn()
	noSend(t, sent)
	noInject(t, injected)
	if sw.pending != "clarify later" {
		t.Fatalf("pending = %q, want %q (queued via drain-on-idle)", sw.pending, "clarify later")
	}
	if got := sw.input.GetText(); strings.TrimSpace(got) != "" {
		t.Errorf("input should be cleared after queueing, got %q", got)
	}
}

// TestQueueButtonMirrorsEnter verifies the Queue button is wired to the same
// submit path as Enter (issue #201): pressing it while busy queues the current
// text exactly like pressing Enter would.
func TestQueueButtonMirrorsEnter(t *testing.T) {
	w := newTestWorkbench(t)
	sent := recordSends(w)
	injected := recordInject(w)
	sw := w.openWindow("s", "S")

	// Become busy via a real first turn.
	sw.input.SetText("first")
	sw.submitFn()
	waitSend(t, sent)

	// Pressing the Queue button while busy queues the current text.
	sw.input.SetText("via queue button")
	if sw.queueButton.OnPress == nil {
		t.Fatal("queue button has no OnPress handler")
	}
	sw.queueButton.OnPress()
	noSend(t, sent)
	noInject(t, injected)
	if sw.pending != "via queue button" {
		t.Fatalf("pending = %q, want %q", sw.pending, "via queue button")
	}
	if got := sw.input.GetText(); strings.TrimSpace(got) != "" {
		t.Errorf("input should be cleared after queueing via button, got %q", got)
	}
}

// TestQueueButtonIdleSendsLikeEnter verifies the Queue button shares submit's
// idle behaviour too: when not busy it sends (the button is only shown while
// busy, but its handler must match Enter for consistency).
func TestQueueButtonIdleSendsLikeEnter(t *testing.T) {
	w := newTestWorkbench(t)
	sent := recordSends(w)
	sw := w.openWindow("s", "S")

	sw.input.SetText("hello")
	sw.queueButton.OnPress()
	if got := waitSend(t, sent); got != "hello" {
		t.Fatalf("idle Queue button send = %q, want %q", got, "hello")
	}
}

// TestInterjectInjectsCurrentInput verifies the Interject button's action (issue
// #201): with text in the box while a turn runs, it hands the current text to
// OnInject (→ UserSession.InjectUserNote), clears the box and notes it.
func TestInterjectInjectsCurrentInput(t *testing.T) {
	w := newTestWorkbench(t)
	injected := recordInject(w)
	sent := recordSends(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	sw.input.SetText("a quick clarification")
	sw.interject()

	if got := waitInject(t, injected); got != "a quick clarification" {
		t.Fatalf("injected = %q, want %q", got, "a quick clarification")
	}
	noSend(t, sent) // interject never sends a new turn
	if got := sw.input.GetText(); strings.TrimSpace(got) != "" {
		t.Errorf("input should be cleared after interjecting, got %q", got)
	}
	if !noteContains(sw, "interjected") {
		t.Error("expected an 'interjected' transcript note")
	}
}

// TestInterjectButtonWired verifies the Interject button is wired to the
// interject action, so clicking it does the same as calling interject() directly.
func TestInterjectButtonWired(t *testing.T) {
	w := newTestWorkbench(t)
	injected := recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	if sw.interjectButton.OnPress == nil {
		t.Fatal("interject button has no OnPress handler")
	}
	sw.input.SetText("via button")
	sw.interjectButton.OnPress()
	if got := waitInject(t, injected); got != "via button" {
		t.Fatalf("injected via button = %q, want %q", got, "via button")
	}
}

// TestInterjectDisabledOnEmptyInput verifies the Interject button is inert while
// the input is empty or blank (issue #201): nothing is injected, and the enabled
// predicate reports false so the button can be greyed.
func TestInterjectDisabledOnEmptyInput(t *testing.T) {
	w := newTestWorkbench(t)
	injected := recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	for _, text := range []string{"", "   ", "\t\n "} {
		sw.input.SetText(text)
		if sw.interjectEnabled() {
			t.Errorf("interjectEnabled() = true for input %q, want false", text)
		}
		sw.interject()
		noInject(t, injected)
	}

	// Non-blank input enables it.
	sw.input.SetText("x")
	if !sw.interjectEnabled() {
		t.Error("interjectEnabled() = false for non-blank input, want true")
	}
}

// TestInterjectNoOpWhenIdle verifies interject is a no-op when no turn is running
// (issue #201): the button is only shown while busy, and its action guards on the
// busy flag so a stray activation does nothing.
func TestInterjectNoOpWhenIdle(t *testing.T) {
	w := newTestWorkbench(t)
	injected := recordInject(w)
	sw := w.openWindow("s", "S")

	sw.input.SetText("nothing in flight")
	sw.interject()
	noInject(t, injected)
	if got := sw.input.GetText(); strings.TrimSpace(got) != "nothing in flight" {
		t.Errorf("idle interject should not clear the input, got %q", got)
	}
}

// TestInterjectDoesNotQueue verifies interject is a wholly separate path from the
// queue: interjecting does not set the pending slot, so the message cannot
// double-fire on idle (issue #201).
func TestInterjectDoesNotQueue(t *testing.T) {
	w := newTestWorkbench(t)
	injected := recordInject(w)
	sent := recordSends(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	sw.input.SetText("mid-turn note")
	sw.interject()
	waitInject(t, injected)
	if sw.pending != "" {
		t.Errorf("interject should not set the pending slot, got %q", sw.pending)
	}

	// Returning to idle must not re-send the interjected text.
	sw.apply(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"})
	noSend(t, sent)
}

// TestInterjectUnavailableWhenHandlerMissing verifies the OnInject-nil path
// (issue #201): with no backend injection handler, interject reports the feature
// unavailable rather than panicking, does not dispatch, and — because the handler
// is checked before the box is cleared — leaves the typed text intact so an
// unwired backend cannot destroy the user's input.
func TestInterjectUnavailableWhenHandlerMissing(t *testing.T) {
	w := newTestWorkbench(t)
	w.handlers.OnInject = nil // unwired, as in a headless/test setup
	sw := w.openWindow("s", "S")
	sw.busy = true

	sw.input.SetText("try to interject")
	sw.interject()
	if !noteContains(sw, "interject unavailable") {
		t.Error("expected an 'interject unavailable' note when OnInject is nil")
	}
	if got := sw.input.GetText(); got != "try to interject" {
		t.Errorf("input should be preserved when interject is unavailable, got %q", got)
	}
}

// TestInterjectTrimsAndIgnoresBlank verifies interject trims the input and treats
// a blank result as empty (no injection), matching the enabled predicate.
func TestInterjectTrimsAndIgnoresBlank(t *testing.T) {
	w := newTestWorkbench(t)
	injected := recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	// Whitespace-only input is treated as empty.
	sw.input.SetText("   ")
	sw.interject()
	noInject(t, injected)

	// Surrounding whitespace is trimmed before injection.
	sw.input.SetText("  real note  ")
	sw.interject()
	if got := waitInject(t, injected); got != "real note" {
		t.Errorf("injected = %q, want trimmed %q", got, "real note")
	}
}

// TestStopButtonCancelsAndClearsQueue verifies the Stop button's action (issue
// #201): it cancels the in-flight turn (OnStop) and discards any queued message,
// mirroring /stop, so a manual stop never silently auto-fires the queue.
func TestStopButtonCancelsAndClearsQueue(t *testing.T) {
	w := newTestWorkbench(t)
	stopped := recordStop(w)
	sent := recordSends(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	// Queue a message, then stop via the button.
	sw.enqueue("queued before stop")
	if sw.pending == "" {
		t.Fatal("expected a queued message before stopping")
	}
	sw.stopButton.OnPress()

	if sw.pending != "" {
		t.Errorf("stop should discard the queue, pending = %q", sw.pending)
	}
	if id := waitStop(t, stopped); id != "s" {
		t.Errorf("OnStop got id %q, want %q", id, "s")
	}
	if !noteContains(sw, "cleared") {
		t.Error("stop should note that the queued message was cleared")
	}
	// The resulting idle transition must not auto-fire the discarded queue.
	sw.apply(agent.SessionEvent{Type: agent.SessionEventError, Err: fmt.Errorf("cancelled")})
	noSend(t, sent)
}

// TestStopButtonWiredToStopTurn verifies the Stop button routes through stopTurn
// (issue #201): with nothing running and nothing queued it surfaces the same
// "nothing to stop" note as /stop, proving it shares the handler.
func TestStopButtonWiredToStopTurn(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	if sw.stopButton.OnPress == nil {
		t.Fatal("stop button has no OnPress handler")
	}
	sw.stopButton.OnPress()
	if !noteContains(sw, "nothing to stop") {
		t.Error("idle stop button should note 'nothing to stop', proving it routes through stopTurn")
	}
}

// TestStopButtonAndSlashCommandEquivalent verifies the Stop button and /stop
// command are equivalent (issue #201): both cancel the turn and clear the queue.
func TestStopButtonAndSlashCommandEquivalent(t *testing.T) {
	for _, via := range []string{"button", "slash"} {
		t.Run(via, func(t *testing.T) {
			w := newTestWorkbench(t)
			stopped := recordStop(w)
			sw := w.openWindow("s", "S")
			sw.busy = true
			sw.enqueue("q")

			switch via {
			case "button":
				sw.stopButton.OnPress()
			case "slash":
				sw.handleSlashCommand("/stop")
			}
			if sw.pending != "" {
				t.Errorf("pending = %q, want cleared", sw.pending)
			}
			waitStop(t, stopped)
		})
	}
}

// TestEnqueueAlwaysDrainsNeverInjects verifies enqueue() directly (issue #201):
// with the experimental flag removed it always stows the pending slot for the
// drain-on-idle path and never hands the text to OnInject, regardless of handler
// wiring. The queued message then fires as the next turn on idle.
func TestEnqueueAlwaysDrainsNeverInjects(t *testing.T) {
	w := newTestWorkbench(t)
	sent := recordSends(w)
	injected := recordInject(w)
	sw := w.openWindow("s", "S")
	sw.busy = true

	sw.enqueue("held message")
	noInject(t, injected)
	if sw.pending != "held message" {
		t.Fatalf("pending = %q, want %q", sw.pending, "held message")
	}
	if !strings.Contains(sw.status.Text, "queued") {
		t.Errorf("status line %q should expose the queued message", sw.status.Text)
	}

	// The busy→idle edge drains the slot as a real turn (not an injection).
	sw.apply(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"})
	if got := waitSend(t, sent); got != "held message" {
		t.Fatalf("drained send = %q, want %q", got, "held message")
	}
	if sw.pending != "" {
		t.Errorf("pending should be empty after draining, got %q", sw.pending)
	}
}

// TestEnqueueReplaceIsLatestWins verifies the queue stays a single latest-wins
// slot (issue #201 preserves issue #170's model): a second enqueue replaces the
// first and notes the replacement.
func TestEnqueueReplaceIsLatestWins(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	sw.enqueue("first queued")
	sw.enqueue("second queued")
	if sw.pending != "second queued" {
		t.Errorf("pending = %q, want latest-wins %q", sw.pending, "second queued")
	}
	if !noteContains(sw, "replaced") {
		t.Error("expected a 'replaced' note when a queued message is overwritten")
	}
}
