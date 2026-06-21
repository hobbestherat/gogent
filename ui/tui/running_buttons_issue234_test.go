package ui

import (
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/config"
)

// This file exercises issue #234 — the running-turn controls (Queue / Interject /
// Stop) are stacked in a VERTICAL column next to the prompt box while a turn runs,
// instead of laid out side by side on one row (their pre-#234 horizontal layout
// from #201, polished to uniform size in #214).
//
// Coverage is grouped:
//
//  1. COLUMN GEOMETRY — the three buttons form one right-aligned column: distinct,
//     strictly-increasing Y in the order Queue (top) → Interject (middle) → Stop
//     (bottom), sharing one X and one uniform width, each 1 row tall.
//  2. PROMPT RECONCILIATION — the column lines up with the 3-row prompt box (one
//     button per prompt row), the prompt shrinks to the room left of the column,
//     and nothing overlaps (checked in 2D, not just X).
//  3. PRESERVED #214 INVARIANTS — uniform width and the readable Interject colour
//     survive the layout change; the idle Send button is unaffected.
//  4. GLYPH DEGRADATION — the single-glyph fallback still works on a narrow window,
//     the column stays vertical in glyph mode, and the flip threshold is pinned.
//  5. EDGE / ROBUSTNESS — minimum height, resize recompute, busy↔idle swap, the
//     column-width helper, and the input-width floor.
//
// The pre-#234 horizontal assertions (button order by X, glyph flip at wd=63, all
// three buttons on the prompt's centre row) lived in running_buttons_test.go and
// running_buttons_issue214_test.go and were updated there to the vertical spec.

// ----------------------------------------------------------------------------
// Helpers.
// ----------------------------------------------------------------------------

// rectsOverlap2D reports whether two rects overlap in BOTH axes — the correct
// collision check for the vertically-stacked column, where every button shares an
// X range. An X-axis-only check would (correctly) report the stacked buttons as
// "meeting" even though they sit on different rows; layout non-overlap assertions
// here use this 2D test instead.
func rectsOverlap2D(a, b tv.Rect) bool {
	if a.Empty() || b.Empty() {
		return false
	}
	return a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H
}

// columnRects returns the three stacked running-turn buttons' current bounds in
// top→bottom display order (Queue, Interject, Stop).
func columnRects(sw *SessionWindow) (queue, interject, stop tv.Rect) {
	return boundsOf(sw.queueButton), boundsOf(sw.interjectButton), boundsOf(sw.stopButton)
}

// assertVerticalColumn is the shared invariant the whole issue turns on: the three
// running-turn buttons are stacked as one column — strictly increasing Y in the
// Queue(top) → Interject(mid) → Stop(bottom) order, sharing one X and one width,
// each exactly 1 row tall — and no two of them overlap.
func assertVerticalColumn(t *testing.T, queue, interject, stop tv.Rect) {
	t.Helper()
	// Distinct, strictly increasing Y in the required top→bottom order.
	if queue.Y >= interject.Y || interject.Y >= stop.Y {
		t.Errorf("buttons not in Queue→Interject→Stop vertical order: q.Y=%d i.Y=%d s.Y=%d",
			queue.Y, interject.Y, stop.Y)
	}
	// Each button advances exactly one row (a contiguous stack with no gaps).
	if want := queue.Y + 1; interject.Y != want {
		t.Errorf("Interject Y=%d, want %d (one row below Queue)", interject.Y, want)
	}
	if want := interject.Y + 1; stop.Y != want {
		t.Errorf("Stop Y=%d, want %d (one row below Interject)", stop.Y, want)
	}
	// Same X (a single column, not a diagonal / staircase).
	if queue.X != interject.X || interject.X != stop.X {
		t.Errorf("buttons do not share one X: q.X=%d i.X=%d s.X=%d", queue.X, interject.X, stop.X)
	}
	// Same uniform width (#214 invariant carried into the stack).
	if queue.W != interject.W || interject.W != stop.W {
		t.Errorf("button widths not uniform: q.W=%d i.W=%d s.W=%d", queue.W, interject.W, stop.W)
	}
	// Each is a single-row button.
	for name, r := range map[string]tv.Rect{"queue": queue, "interject": interject, "stop": stop} {
		if r.H != 1 {
			t.Errorf("%s button H=%d, want 1", name, r.H)
		}
	}
	// No two buttons overlap (they must be on different rows — the 2D check a
	// horizontal-only helper would miss).
	for _, pair := range [][2]tv.Rect{{queue, interject}, {interject, stop}, {queue, stop}} {
		if rectsOverlap2D(pair[0], pair[1]) {
			t.Errorf("stacked buttons overlap: %+v and %+v", pair[0], pair[1])
		}
	}
}

// ----------------------------------------------------------------------------
// Group 1 — column geometry.
// ----------------------------------------------------------------------------

// TestIssue234ButtonsStackVertically is the core assertion (issue #234): while a
// turn runs the three running-turn buttons are laid out as a vertical column, not
// a horizontal row. On a wide window they stack top→bottom Queue / Interject / Stop
// with one shared X and uniform width.
func TestIssue234ButtonsStackVertically(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	layout(sw, 80, 24, 3)

	queue, interject, stop := columnRects(sw)
	for name, r := range map[string]tv.Rect{"queue": queue, "interject": interject, "stop": stop} {
		if r.Empty() {
			t.Fatalf("busy column should show the %s button, got empty bounds %+v", name, r)
		}
	}
	assertVerticalColumn(t, queue, interject, stop)
}

// TestIssue234ColumnOrderQueueInterjectStop pins the required vertical order
// explicitly: Queue on top, Interject in the middle, Stop on the bottom (the order
// the issue asks for, not the old horizontal right-to-left Stop-far-right order).
func TestIssue234ColumnOrderQueueInterjectStop(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	const wd, ht, inputH = 80, 24, 3
	layout(sw, wd, ht, inputH)

	top := ht - inputH
	queue, interject, stop := columnRects(sw)
	if queue.Y != top {
		t.Errorf("Queue Y=%d, want the top row %d (Queue is the top of the stack)", queue.Y, top)
	}
	if interject.Y != top+1 {
		t.Errorf("Interject Y=%d, want the middle row %d", interject.Y, top+1)
	}
	if stop.Y != top+2 {
		t.Errorf("Stop Y=%d, want the bottom row %d", stop.Y, top+2)
	}
}

// TestIssue234ColumnSharesXAndWidth asserts every button in the column shares one X
// and one width — the defining property of "a column" rather than three independent
// placements — and that the shared width is the #214 uniform width, not each
// button's own (shorter) label width.
func TestIssue234ColumnSharesXAndWidth(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	layout(sw, 80, 24, 3)

	queue, interject, stop := columnRects(sw)
	wantW := uniformButtonWidth(interjectLabel, queueLabel, stopLabel)
	if queue.W != wantW {
		t.Errorf("Queue W=%d, want the uniform width %d (not its own %d)",
			queue.W, wantW, buttonWidth(queueLabel))
	}
	if queue.X != interject.X || interject.X != stop.X {
		t.Errorf("column buttons differ in X: q.X=%d i.X=%d s.X=%d", queue.X, interject.X, stop.X)
	}
	if queue.W != interject.W || interject.W != stop.W {
		t.Errorf("column buttons differ in W: q.W=%d i.W=%d s.W=%d", queue.W, interject.W, stop.W)
	}
}

// TestIssue234ColumnFlushWithRightMargin asserts the whole column is right-aligned:
// every button's right edge sits flush against the input-row right margin (one cell
// shy of the window's right edge), as the horizontal layout did.
func TestIssue234ColumnFlushWithRightMargin(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	const wd = 80
	layout(sw, wd, 24, 3)

	wantRight := wd - inputRowMargin
	for name, r := range map[string]tv.Rect{
		"queue": boundsOf(sw.queueButton), "interject": boundsOf(sw.interjectButton), "stop": boundsOf(sw.stopButton),
	} {
		if got := r.X + r.W; got != wantRight {
			t.Errorf("%s right edge = %d, want %d (flush with the right margin)", name, got, wantRight)
		}
	}
}

// ----------------------------------------------------------------------------
// Group 2 — prompt reconciliation.
// ----------------------------------------------------------------------------

// TestIssue234ColumnLinesUpWithPromptRows verifies the input-area height was
// reconciled so the stacked column lines up with the prompt box: the prompt is
// inputH (3) rows tall and each button is 1 row, so the three buttons occupy
// exactly the prompt's three rows — no button spills below the input area, and the
// column spans the full input height.
func TestIssue234ColumnLinesUpWithPromptRows(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	const wd, ht, inputH = 80, 24, 3
	layout(sw, wd, ht, inputH)

	in := boundsOfInput(sw)
	queue, interject, stop := columnRects(sw)
	// Each button sits on a distinct prompt row (top, middle, bottom).
	if queue.Y != in.Y {
		t.Errorf("Queue Y=%d, want the prompt's top row %d", queue.Y, in.Y)
	}
	if interject.Y != in.Y+1 {
		t.Errorf("Interject Y=%d, want the prompt's middle row %d", interject.Y, in.Y+1)
	}
	if stop.Y != in.Y+2 {
		t.Errorf("Stop Y=%d, want the prompt's bottom row %d", stop.Y, in.Y+2)
	}
	// The bottom button's row stays within the prompt box (rows [in.Y, in.Y+in.H)).
	if stop.Y >= in.Y+in.H {
		t.Errorf("Stop Y=%d spills below the input area [Y=%d,H=%d)", stop.Y, in.Y, in.H)
	}
	// The column spans the prompt's full height: Queue at the top row, Stop at the
	// last row of the input area.
	if queue.Y != in.Y {
		t.Errorf("column should start at the prompt top %d, got Queue Y=%d", in.Y, queue.Y)
	}
	if stop.Y != in.Y+in.H-1 {
		t.Errorf("column should end at the prompt bottom row %d, got Stop Y=%d", in.Y+in.H-1, stop.Y)
	}
}

// TestIssue234NoOverlapBetweenPromptAndColumn asserts the prompt box and the button
// column never overlap in 2D: the prompt shrinks to the room left of the column
// with the one-cell gap between them, so a button never paints over the prompt.
func TestIssue234NoOverlapBetweenPromptAndColumn(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	const wd = 80
	layout(sw, wd, 24, 3)

	in := boundsOfInput(sw)
	queue, interject, stop := columnRects(sw)
	for name, r := range map[string]tv.Rect{"queue": queue, "interject": interject, "stop": stop} {
		if rectsOverlap2D(in, r) {
			t.Errorf("prompt %+v overlaps the %s button %+v", in, name, r)
		}
	}
	// The prompt's right edge is exactly one gap cell left of the column.
	wantInputRight := queue.X - inputRowGap
	if in.X+in.W != wantInputRight {
		t.Errorf("prompt right edge = %d, want %d (one gap left of the column at X=%d)",
			in.X+in.W, wantInputRight, queue.X)
	}
}

// TestIssue234PromptShrinksBesideColumn asserts the busy prompt is narrower than the
// idle prompt: the column claims room the idle Send button does not, so the prompt
// gives up width to sit beside the stack.
func TestIssue234PromptShrinksBesideColumn(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	const wd, ht, inputH = 80, 24, 3

	layout(sw, wd, ht, inputH) // idle
	idleW := boundsOfInput(sw).W

	sw.busy = true
	layout(sw, wd, ht, inputH) // busy
	busyW := boundsOfInput(sw).W

	if busyW >= idleW {
		t.Errorf("busy prompt width %d should be < idle width %d (room yielded to the column)", busyW, idleW)
	}
	if busyW < 1 {
		t.Errorf("busy prompt width %d must be >= 1", busyW)
	}
}

// ----------------------------------------------------------------------------
// Group 3 — preserved #214 invariants.
// ----------------------------------------------------------------------------

// TestIssue234UniformWidthInVerticalStack verifies the #214 uniform-width polish
// survives the vertical change: all three stacked buttons share one width (the
// widest label's) in full-label mode, not their individual label widths.
func TestIssue234UniformWidthInVerticalStack(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	layout(sw, 80, 24, 3) // full labels

	queue, interject, stop := columnRects(sw)
	want := uniformButtonWidth(interjectLabel, queueLabel, stopLabel)
	if queue.W != want || interject.W != want || stop.W != want {
		t.Errorf("column widths not uniform: q.W=%d i.W=%d s.W=%d, want all %d",
			queue.W, interject.W, stop.W, want)
	}
	// Queue (11) and Stop (10) must have been widened to the shared 13, not left at
	// their own widths — guards against a revert to per-label sizing.
	if queue.W == buttonWidth(queueLabel) {
		t.Errorf("Queue kept its per-label width %d instead of the uniform %d", queue.W, want)
	}
	if stop.W == buttonWidth(stopLabel) {
		t.Errorf("Stop kept its per-label width %d instead of the uniform %d", stop.W, want)
	}
}

// TestIssue234InterjectColourStillReadable keeps the #214 readable-Interject
// invariant in view of the layout change: the Interject foreground in both states
// clears the large-text contrast floor on the default theme (a regression here
// would mean the colour work was lost alongside the layout move).
func TestIssue234InterjectColourStillReadable(t *testing.T) {
	withThemeRestore(t)
	ApplyTheme(ResolveTheme(config.ThemeConfig{}, truecolorEnv, false))
	bg := tv.ActiveTheme().ButtonBG
	for _, state := range []struct {
		name    string
		enabled bool
	}{
		{"enabled", true}, {"disabled", false},
	} {
		fg := interjectButtonFG(state.enabled)
		if contrastRatio(fg, bg) < minContrastLarge {
			t.Errorf("%s Interject contrast %.3f < minContrastLarge %.1f — #214 readability lost",
				state.name, contrastRatio(fg, bg), minContrastLarge)
		}
	}
	// Stop stays error-red (its identity, distinct from Interject).
	sw := newTestWorkbench(t).openWindow("s", "S")
	if sw.stopButton.FG != colorError {
		t.Errorf("Stop FG = %v, want the error colour %v", sw.stopButton.FG, colorError)
	}
}

// TestIssue234IdleSendUnaffected asserts the idle layout is untouched by the
// vertical change: a single Send button, centred on the prompt box, with the three
// running buttons hidden — the column only exists while busy.
func TestIssue234IdleSendUnaffected(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	const wd, ht, inputH = 80, 24, 3
	layout(sw, wd, ht, inputH) // idle

	send := boundsOf(sw.sendButton)
	if send.Empty() {
		t.Fatal("idle should show the Send button")
	}
	in := boundsOfInput(sw)
	centerY := in.Y + (in.H-1)/2
	if send.Y != centerY {
		t.Errorf("idle Send Y=%d, want the prompt centre %d (idle placement unchanged)", send.Y, centerY)
	}
	if send.H != 1 {
		t.Errorf("idle Send H=%d, want 1", send.H)
	}
	for name, b := range map[string]*tv.Button{
		"interject": sw.interjectButton, "queue": sw.queueButton, "stop": sw.stopButton,
	} {
		if r := boundsOf(b); !r.Empty() {
			t.Errorf("idle should hide the %s button, got %+v", name, r)
		}
		if b.Component.Visible {
			t.Errorf("idle %s should be !Visible (out of the focus cycle)", name)
		}
	}
}

// ----------------------------------------------------------------------------
// Group 4 — glyph degradation.
// ----------------------------------------------------------------------------

// TestIssue234DegradesToGlyphsOnNarrowWindow verifies the degraded single-glyph
// mode still works (issue #234 keeps it): on a window too narrow for the full
// labels beside a usable prompt, the labels collapse to single glyphs.
func TestIssue234DegradesToGlyphsOnNarrowWindow(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	// Narrow window (below the flip): glyphs.
	layout(sw, 30, 24, 3)
	if sw.interjectButton.Label != interjectGlyph ||
		sw.queueButton.Label != queueGlyph ||
		sw.stopButton.Label != stopGlyph {
		t.Errorf("narrow labels = %q/%q/%q, want glyphs",
			sw.interjectButton.Label, sw.queueButton.Label, sw.stopButton.Label)
	}
	// Wide window (above the flip): full labels.
	layout(sw, 80, 24, 3)
	if sw.interjectButton.Label != interjectLabel ||
		sw.queueButton.Label != queueLabel ||
		sw.stopButton.Label != stopLabel {
		t.Errorf("wide labels = %q/%q/%q, want full",
			sw.interjectButton.Label, sw.queueButton.Label, sw.stopButton.Label)
	}
}

// TestIssue234GlyphModeStillVerticalColumn asserts that in degraded glyph mode the
// buttons are STILL a vertical column (distinct increasing Y, same X and width) —
// degradation changes the labels, not the stacking.
func TestIssue234GlyphModeStillVerticalColumn(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	layout(sw, 30, 24, 3) // glyph mode

	if sw.interjectButton.Label != interjectGlyph {
		t.Fatalf("expected glyph mode at wd=30, got %q", sw.interjectButton.Label)
	}
	queue, interject, stop := columnRects(sw)
	assertVerticalColumn(t, queue, interject, stop)
	// In glyph mode the shared width is the glyph width.
	want := uniformButtonWidth(interjectGlyph, queueGlyph, stopGlyph)
	if queue.W != want {
		t.Errorf("glyph column width = %d, want %d", queue.W, want)
	}
}

// TestIssue234DegradationFlipThreshold pins the exact width at which the labels flip
// between glyph and full form under the new column footprint. The column claims only
// one button width (not three), so the footprint shrank from 42 to 15 and the flip
// moved down from wd=63 (horizontal) to wd=35 (vertical). At the flip the prompt
// gets exactly minInputWidth cells.
func TestIssue234DegradationFlipThreshold(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	// The column footprint is one uniform frame + gap + margin.
	footprint := runningButtonsColumnWidth(interjectLabel, queueLabel, stopLabel)
	if footprint != 15 {
		t.Fatalf("column footprint = %d, want 15 (uniform 13 + gap 1 + margin 1)", footprint)
	}

	// One below the flip: still glyphs.
	flip := footprint + minInputWidth // 15 + 20 = 35
	layout(sw, flip-1, 24, 3)
	if sw.interjectButton.Label != interjectGlyph {
		t.Errorf("wd=%d should use glyphs, got %q", flip-1, sw.interjectButton.Label)
	}
	// At the flip: full labels and the prompt gets exactly minInputWidth.
	layout(sw, flip, 24, 3)
	if sw.interjectButton.Label != interjectLabel {
		t.Errorf("wd=%d should use full labels, got %q", flip, sw.interjectButton.Label)
	}
	if got := boundsOfInput(sw).W; got != minInputWidth {
		t.Errorf("flip-point prompt width = %d, want exactly minInputWidth %d", got, minInputWidth)
	}
	// And the prompt only grows from the flip upward.
	layout(sw, flip+20, 24, 3)
	if got := boundsOfInput(sw).W; got <= minInputWidth {
		t.Errorf("prompt width above the flip = %d, want > minInputWidth %d", got, minInputWidth)
	}
}

// ----------------------------------------------------------------------------
// Group 5 — edge cases / robustness.
// ----------------------------------------------------------------------------

// TestIssue234ColumnAcrossWidths is the property check: across realistic window
// widths spanning both glyph and full-label modes, the buttons always form a proper
// vertical column, sit flush with the right margin, never overlap the prompt, and
// the prompt keeps at least one cell. Widths start above the window minimum.
func TestIssue234ColumnAcrossWidths(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	for _, wd := range []int{30, 34, 35, 36, 40, 48, 50, 63, 80, 100, 120} {
		layout(sw, wd, 24, 3)
		in := boundsOfInput(sw)
		queue, interject, stop := columnRects(sw)
		// Vertical column invariant at every width.
		assertVerticalColumn(t, queue, interject, stop)
		// Flush with the right margin.
		if got := stop.X + stop.W; got != wd-inputRowMargin {
			t.Errorf("wd=%d: Stop right edge %d, want %d", wd, got, wd-inputRowMargin)
		}
		// Prompt never overlaps any button and keeps at least one cell.
		if in.W < 1 {
			t.Errorf("wd=%d: prompt width %d must be >= 1", wd, in.W)
		}
		for name, r := range map[string]tv.Rect{"queue": queue, "interject": interject, "stop": stop} {
			if rectsOverlap2D(in, r) {
				t.Errorf("wd=%d: prompt overlaps %s button: %+v and %+v", wd, name, in, r)
			}
		}
		// Buttons stay on screen for realistic widths.
		for name, r := range map[string]tv.Rect{"queue": queue, "interject": interject, "stop": stop} {
			if r.X < 0 {
				t.Errorf("wd=%d: %s button off-screen left at X=%d", wd, name, r.X)
			}
		}
	}
}

// TestIssue234ColumnFitsAtMinimumHeight verifies the three stacked rows always fit
// at the layout guard's minimum height (ht=7): with inputH=3 the column occupies the
// three bottom-most rows and nothing spills outside the window. Height never forces
// glyph degradation (only width does), per the #234 design.
func TestIssue234ColumnFitsAtMinimumHeight(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	const wd, ht, inputH = 80, 7, 3
	layout(sw, wd, ht, inputH)

	queue, interject, stop := columnRects(sw)
	// Full labels even at minimum height: height does not degrade the labels.
	if sw.interjectButton.Label != interjectLabel {
		t.Errorf("minimum-height window should still use full labels, got %q", sw.interjectButton.Label)
	}
	for name, r := range map[string]tv.Rect{"queue": queue, "interject": interject, "stop": stop} {
		if r.Y < 0 || r.Y+r.H > ht {
			t.Errorf("%s button %+v does not fit in a %d-row window", name, r, ht)
		}
	}
	// Stop sits on the window's last row.
	if stop.Y != ht-1 {
		t.Errorf("Stop Y=%d, want the window's last row %d", stop.Y, ht-1)
	}
}

// TestIssue234LayoutRecomputedOnResize verifies the label choice and geometry are
// recomputed on every layout pass, so a resize from narrow to wide (and back) never
// leaves a stale glyph/full state or stale bounds.
func TestIssue234LayoutRecomputedOnResize(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	// Wide → narrow → wide: labels follow each pass, no stale state.
	layout(sw, 80, 24, 3)
	if sw.interjectButton.Label != interjectLabel {
		t.Fatalf("wide: label = %q, want full", sw.interjectButton.Label)
	}
	wideX := boundsOf(sw.queueButton).X

	layout(sw, 30, 24, 3)
	if sw.interjectButton.Label != interjectGlyph {
		t.Fatalf("narrow: label = %q, want glyph", sw.interjectButton.Label)
	}
	narrowX := boundsOf(sw.queueButton).X

	layout(sw, 80, 24, 3)
	if sw.interjectButton.Label != interjectLabel {
		t.Fatalf("wide again: label = %q, want full (no stale glyph state)", sw.interjectButton.Label)
	}
	// The wide geometry is restored exactly (not a stale narrow position).
	if got := boundsOf(sw.queueButton).X; got != wideX {
		t.Errorf("wide-again Queue X=%d, want %d (restored, not stale %d)", got, wideX, narrowX)
	}
}

// TestIssue234BusyIdleSwapSwapsColumnAndSend verifies the column follows the busy
// flag across transitions: busy shows the stacked column and hides Send; returning
// to idle hides the column and restores the single Send.
func TestIssue234BusyIdleSwapSwapsColumnAndSend(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	// Idle: Send shown, column hidden.
	layout(sw, 80, 24, 3)
	if boundsOf(sw.sendButton).Empty() {
		t.Fatal("idle: Send should show")
	}
	for _, b := range []*tv.Button{sw.queueButton, sw.interjectButton, sw.stopButton} {
		if !boundsOf(b).Empty() {
			t.Fatal("idle: column buttons should be hidden")
		}
	}

	// Busy: Send hidden, column shown.
	sw.busy = true
	layout(sw, 80, 24, 3)
	if !boundsOf(sw.sendButton).Empty() {
		t.Fatal("busy: Send should hide")
	}
	for name, b := range map[string]*tv.Button{"queue": sw.queueButton, "interject": sw.interjectButton, "stop": sw.stopButton} {
		if boundsOf(b).Empty() {
			t.Fatalf("busy: %s should show", name)
		}
	}

	// Back to idle: Send restored, column hidden again.
	sw.busy = false
	layout(sw, 80, 24, 3)
	if boundsOf(sw.sendButton).Empty() {
		t.Fatal("idle again: Send should be restored")
	}
	for _, b := range []*tv.Button{sw.queueButton, sw.interjectButton, sw.stopButton} {
		if !boundsOf(b).Empty() {
			t.Fatal("idle again: column buttons should be hidden")
		}
	}
}

// TestIssue234ColumnVisibleFlagsFollowBusy asserts the focus-cycle integration
// (issue #201) survives the vertical change: while busy the three column buttons are
// Visible (so Tab can reach them), and while idle they are !Visible (so they leave
// the focus cycle) — not merely zero-bounds.
func TestIssue234ColumnVisibleFlagsFollowBusy(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	sw.busy = true
	layout(sw, 80, 24, 3)
	for name, b := range map[string]*tv.Button{
		"queue": sw.queueButton, "interject": sw.interjectButton, "stop": sw.stopButton,
	} {
		if !b.Component.Visible {
			t.Errorf("busy: %s should be Visible (focusable)", name)
		}
	}
	if sw.sendButton.Component.Visible {
		t.Error("busy: Send should be !Visible")
	}

	sw.busy = false
	layout(sw, 80, 24, 3)
	for name, b := range map[string]*tv.Button{
		"queue": sw.queueButton, "interject": sw.interjectButton, "stop": sw.stopButton,
	} {
		if b.Component.Visible {
			t.Errorf("idle: %s should be !Visible (out of the focus cycle)", name)
		}
	}
	if !sw.sendButton.Component.Visible {
		t.Error("idle: Send should be Visible")
	}
}

// TestIssue234ColumnWidthHelper pins runningButtonsColumnWidth: the horizontal room
// the vertical column claims is ONE uniform button frame plus the gap to the prompt
// and the right margin — not three frames summed side by side. It drives the glyph-
// degradation budget.
func TestIssue234ColumnWidthHelper(t *testing.T) {
	// Full labels: uniform 13 + gap 1 + margin 1 = 15.
	if got := runningButtonsColumnWidth(interjectLabel, queueLabel, stopLabel); got != 15 {
		t.Errorf("column width(full) = %d, want 15", got)
	}
	// Glyphs: uniform 5 + gap 1 + margin 1 = 7.
	if got := runningButtonsColumnWidth(interjectGlyph, queueGlyph, stopGlyph); got != 7 {
		t.Errorf("column width(glyph) = %d, want 7", got)
	}
	// It is exactly uniformButtonWidth + gap + margin for any label set (the
	// definition), including one where the widest label is not the first.
	check := func(i, q, s string) {
		t.Helper()
		want := uniformButtonWidth(i, q, s) + inputRowGap + inputRowMargin
		if got := runningButtonsColumnWidth(i, q, s); got != want {
			t.Errorf("column width(%q,%q,%q) = %d, want %d", i, q, s, got, want)
		}
	}
	check("a", "bb", "cccccccccc") // widest is third
	check("x", "y", "z")
}

// TestIssue234InputWidthFlooredAtOne exercises the input-width clamp: on a window
// so narrow the column leaves almost no room, the prompt width is floored at 1
// rather than going zero/negative (the explicit `if inputW < 1` guard).
func TestIssue234InputWidthFlooredAtOne(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true

	// Width where the glyph column leaves the prompt under a cell before clamping.
	// Glyph btnW=5 → inputW = wd - btnW - 2; at wd=8 that is 1 (already the floor).
	layout(sw, 8, 24, 3)
	in := boundsOfInput(sw)
	if in.W < 1 {
		t.Errorf("clamped prompt width = %d, must be >= 1", in.W)
	}
	// Even narrower: the clamp holds (never zero/negative).
	layout(sw, 6, 24, 3)
	if got := boundsOfInput(sw).W; got < 1 {
		t.Errorf("clamped prompt width at wd=6 = %d, must be >= 1", got)
	}
}

// TestIssue234FullLayoutColumnDoesNotCollideWithChrome drives the REAL per-window
// Content.LayoutFn (not layoutInputRow in isolation), which positions the
// history / separator / status AND the input row + button column together. It
// asserts the stacked column never collides with the chrome above the input area —
// the integration check that would catch an inputH mismatch between layoutInputRow
// and the surrounding layout closure (e.g. the column's 3 hardcoded rows drifting
// below the input area into the status line).
func TestIssue234FullLayoutColumnDoesNotCollideWithChrome(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	const wd, ht = 80, 24
	// SetBounds on Content runs the captured LayoutFn with these content bounds.
	sw.window.Content.SetBounds(tv.Rect{X: 0, Y: 0, W: wd, H: ht})

	in := boundsOfInput(sw)
	queue, interject, stop := columnRects(sw)
	for name, r := range map[string]tv.Rect{"queue": queue, "interject": interject, "stop": stop} {
		if r.Empty() {
			t.Fatalf("full layout should show the %s button, got empty bounds", name)
		}
	}
	// The column invariant holds under the full layout, not just in isolation.
	assertVerticalColumn(t, queue, interject, stop)

	status := sw.status.Component.Bounds
	sep := sw.separator.Component.Bounds
	hist := sw.history.Component.Bounds
	// The column sits strictly below the status line and separator (the chrome
	// directly above the input area); it never overlaps them or the transcript.
	for name, r := range map[string]tv.Rect{"queue": queue, "interject": interject, "stop": stop} {
		if rectsOverlap2D(r, status) {
			t.Errorf("%s button %+v overlaps the status line %+v", name, r, status)
		}
		if rectsOverlap2D(r, sep) {
			t.Errorf("%s button %+v overlaps the separator %+v", name, r, sep)
		}
		if rectsOverlap2D(r, hist) {
			t.Errorf("%s button %+v overlaps the history/transcript %+v", name, r, hist)
		}
	}
	// The whole input area (and thus the column) starts below the status line.
	if in.Y <= status.Y {
		t.Errorf("input area Y=%d must sit below the status line Y=%d", in.Y, status.Y)
	}
	// The prompt and the column still do not overlap under the full layout.
	for name, r := range map[string]tv.Rect{"queue": queue, "interject": interject, "stop": stop} {
		if rectsOverlap2D(in, r) {
			t.Errorf("prompt %+v overlaps the %s button %+v under the full layout", in, name, r)
		}
	}
}

// ----------------------------------------------------------------------------
// Group 6 — runningButtonStackRows (the inputH-derived stack rows, fix round 1).
// ----------------------------------------------------------------------------

// TestIssue234RunningButtonStackRows pins the helper that derives the column's Y
// rows from the input-area height (fix round 1): for the real inputH == 3 it returns
// three distinct, consecutive rows in Queue→Interject→Stop order; and its safety-net
// contract holds for every height — no row ever spills below the input area
// (row <= top+inputH-1), the property that lets the column sit cleanly above the
// status line even if a future caller shrank inputH.
func TestIssue234RunningButtonStackRows(t *testing.T) {
	// The production case: a 3-row input area yields three distinct consecutive
	// rows in the required top→bottom order.
	q, i, s := runningButtonStackRows(21, 3)
	if q != 21 || i != 22 || s != 23 {
		t.Errorf("inputH=3 rows = (%d,%d,%d), want (21,22,23) (Queue/Interject/Stop)", q, i, s)
	}
	if q >= i || i >= s {
		t.Errorf("inputH=3 rows not strictly increasing: %d,%d,%d", q, i, s)
	}
	if i != q+1 || s != q+2 {
		t.Errorf("inputH=3 rows not consecutive: got %d,%d,%d want %d,%d,%d", q, i, s, q, q+1, q+2)
	}

	// The safety-net contract: for ANY inputH >= 1, no row spills below the input
	// area (every row is within [top, top+inputH-1]). This is what keeps the
	// column off the status line beneath it.
	for _, inputH := range []int{1, 2, 3, 4, 5, 10} {
		const top = 21
		last := top + inputH - 1
		q, i, s := runningButtonStackRows(top, inputH)
		for name, y := range map[string]int{"queue": q, "interject": i, "stop": s} {
			if y < top || y > last {
				t.Errorf("inputH=%d: %s row %d spills outside the input area [%d,%d]",
					inputH, name, y, top, last)
			}
		}
	}

	// For inputH >= 3 the three rows are always distinct and ordered.
	for _, inputH := range []int{3, 4, 5, 10} {
		q, i, s := runningButtonStackRows(0, inputH)
		if q >= i || i >= s {
			t.Errorf("inputH=%d: expected distinct ordered rows, got %d,%d,%d", inputH, q, i, s)
		}
	}
}

// TestIssue234StackRowsCollapseBelowThreeIsCharacterization pins the documented
// trade-off of the clamp (fix round 1): a future inputH < 3 has fewer rows than
// buttons, so the later buttons collapse onto the input area's last row rather
// than spill below it. That means interject/stop then SHARE a row (a visual
// collision) — accepted because no current caller produces inputH < 3 (the sole
// caller passes 3 and the layout guard requires the window be tall enough). This
// characterizes the collapse so a change to the clamping strategy is conscious.
func TestIssue234StackRowsCollapseBelowThreeIsCharacterization(t *testing.T) {
	// inputH == 2: two rows for three buttons → Stop clamps onto Interject's row.
	q, i, s := runningButtonStackRows(21, 2)
	if q != 21 || i != 22 || s != 22 {
		t.Errorf("inputH=2 rows = (%d,%d,%d), want (21,22,22) (Stop clamped to the last row)", q, i, s)
	}
	if i != s {
		t.Errorf("inputH=2: Interject and Stop should share the last row (documented collapse), got %d vs %d", i, s)
	}

	// inputH == 1: one row for three buttons → all three collapse onto it.
	q, i, s = runningButtonStackRows(21, 1)
	if q != 21 || i != 21 || s != 21 {
		t.Errorf("inputH=1 rows = (%d,%d,%d), want (21,21,21) (all clamped to the single row)", q, i, s)
	}
}

// TestIssue234LayoutUsesStackRowsHelper is the wiring guard: the busy column's Y
// rows must equal runningButtonStackRows(top, inputH) — i.e. layoutInputRow routes
// through the helper instead of re-hardcoding top/top+1/top+2. A regression that
// inlined the rows again (and so dropped the no-spill clamp) would fail here.
func TestIssue234LayoutUsesStackRowsHelper(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	const wd, ht, inputH = 80, 24, 3
	layout(sw, wd, ht, inputH)

	top := ht - inputH
	wantQ, wantI, wantS := runningButtonStackRows(top, inputH)
	if got := boundsOf(sw.queueButton).Y; got != wantQ {
		t.Errorf("Queue Y=%d, want runningButtonStackRows()=%d", got, wantQ)
	}
	if got := boundsOf(sw.interjectButton).Y; got != wantI {
		t.Errorf("Interject Y=%d, want runningButtonStackRows()=%d", got, wantI)
	}
	if got := boundsOf(sw.stopButton).Y; got != wantS {
		t.Errorf("Stop Y=%d, want runningButtonStackRows()=%d", got, wantS)
	}
	// Sanity: the helper agrees with the documented geometry for the real case.
	if wantQ != top || wantI != top+1 || wantS != top+2 {
		t.Errorf("helper rows for inputH=3 = (%d,%d,%d), want (%d,%d,%d)",
			wantQ, wantI, wantS, top, top+1, top+2)
	}
}

// TestIssue234TooShortWindowSkipsColumnLayout pins the "short window" edge case the
// #234 design relies on: the per-window Content.LayoutFn guards on ht >= 7, so a
// window too short for the 3-row input area does not lay out the column at all (the
// buttons keep zero bounds) rather than rendering a clipped/broken stack. This is why
// no height-based glyph degradation is needed — a too-short window degrades to "no
// column", not to a cropped one.
func TestIssue234TooShortWindowSkipsColumnLayout(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.busy = true
	// ht=6 is below the layout guard (ht < 7). Drive the real LayoutFn, not
	// layoutInputRow in isolation (which has no guard of its own).
	sw.window.Content.SetBounds(tv.Rect{X: 0, Y: 0, W: 80, H: 6})

	for name, b := range map[string]*tv.Button{
		"queue": sw.queueButton, "interject": sw.interjectButton, "stop": sw.stopButton,
	} {
		if r := boundsOf(b); !r.Empty() {
			t.Errorf("a too-short window (ht=6) should not lay out the %s button, got %+v", name, r)
		}
	}
	// Just above the guard (ht=7) the column DOES lay out and fits — the boundary
	// between "skip" and "full column" is exactly the guard.
	sw.window.Content.SetBounds(tv.Rect{X: 0, Y: 0, W: 80, H: 7})
	for name, b := range map[string]*tv.Button{
		"queue": sw.queueButton, "interject": sw.interjectButton, "stop": sw.stopButton,
	} {
		if r := boundsOf(b); r.Empty() {
			t.Errorf("ht=7 should lay out the %s button, got empty bounds", name)
		}
	}
	stop := boundsOf(sw.stopButton)
	if stop.Y+stop.H > 7 {
		t.Errorf("at ht=7 the column must fit within the window: Stop bottom = %d", stop.Y+stop.H)
	}
}
