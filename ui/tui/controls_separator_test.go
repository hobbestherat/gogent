package ui

import (
	"strings"
	"testing"
	"unicode/utf8"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// layoutContent drives a live session window's full content layout the way a real
// draw would, sizing the content area to wd×ht. window.Content.SetBounds invokes
// the LayoutFn closure in newSessionWindow (the code under test for issue #195),
// which positions the header, history, separator, status and input row.
func layoutContent(sw *SessionWindow, wd, ht int) {
	sw.window.Content.SetBounds(tv.Rect{X: 0, Y: 0, W: wd, H: ht})
}

// historyBounds / statusBounds / separatorBounds return a window's current widget
// bounds for layout assertions.
func historyBounds(sw *SessionWindow) tv.Rect   { return sw.history.Component.Bounds }
func statusBounds(sw *SessionWindow) tv.Rect    { return sw.status.Component.Bounds }
func separatorBounds(sw *SessionWindow) tv.Rect { return sw.separator.Component.Bounds }

// inputRowTop returns the top row of the input box (the prompt + the #201 button
// row), which is the bottom edge of the controls region.
func inputRowTop(sw *SessionWindow) int { return sw.input.Component.Bounds.Y }

// rectOverlap reports whether two non-empty rects share any row (their row ranges
// intersect). Used to assert the separator never collides with the history or
// status rows it sits between.
func rectOverlap(a, b tv.Rect) bool {
	if a.Empty() || b.Empty() {
		return false
	}
	return a.Y < b.Y+b.H && b.Y < a.Y+a.H
}

// TestSeparatorSeparatesStatusFromTranscript is the core issue #195 assertion: on a
// live window the status row is no longer flush under the transcript. A dedicated
// separator rule is laid out on its own row directly between the transcript's last
// line and the status row, so the status sits two rows below the history bottom
// (history bottom, then the separator, then the status) rather than one.
func TestSeparatorSeparatesStatusFromTranscript(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	layoutContent(sw, 80, 24)

	if sw.separator == nil {
		t.Fatal("live window should have a controls separator (issue #195)")
	}
	hist := historyBounds(sw)
	sep := separatorBounds(sw)
	stat := statusBounds(sw)

	// The separator sits on the row immediately below the transcript's last line.
	if sep.Y != hist.Bottom()+1 {
		t.Errorf("separator Y=%d should be history bottom+1=%d (history=%+v)",
			sep.Y, hist.Bottom()+1, hist)
	}
	// The status sits on the row immediately below the separator.
	if stat.Y != sep.Y+1 {
		t.Errorf("status Y=%d should be separator Y+1=%d (sep=%+v)", stat.Y, sep.Y+1, sep)
	}
	// The status is no longer flush against the transcript: there is at least the
	// separator row between them (status is >=2 rows below the history bottom).
	if stat.Y < hist.Bottom()+2 {
		t.Errorf("status Y=%d must be >= history bottom+2=%d — it should not be flush under the transcript (issue #195)",
			stat.Y, hist.Bottom()+2)
	}
}

// TestSeparatorExistsOnLiveWindowOnly verifies the separator is created on live
// windows and is nil on the read-only analysis window (which has no status/input
// chrome to fence off).
func TestSeparatorExistsOnLiveWindowOnly(t *testing.T) {
	w := newTestWorkbench(t)

	live := w.openWindow("live", "L")
	if live.separator == nil {
		t.Fatal("live window should have a controls separator (issue #195)")
	}

	// A read-only analysis window is built directly (package-internal) so it is
	// not registered with the workbench; the test only inspects its chrome.
	ro := newSessionWindow(w, "analysis-1", "Saved", tv.Rect{}, true)
	if ro.separator != nil {
		t.Error("read-only analysis window should have no separator (no controls region)")
	}
	// Driving its layout must not create one either, and the transcript keeps the
	// full content area.
	layoutContent(ro, 60, 20)
	if ro.separator != nil {
		t.Error("read-only window separator should stay nil after layout")
	}
	if hb := historyBounds(ro); hb.W != 60 || hb.H != 20 || hb.X != 0 || hb.Y != 0 {
		t.Errorf("read-only history should fill the content area, got %+v", hb)
	}
}

// TestSeparatorSpansFullInnerWidth checks the rule spans the whole inner width on
// every width so that, with the window frame's left/right borders, it boxes the
// controls region (issue #195).
func TestSeparatorSpansFullInnerWidth(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	for _, wd := range []int{4, 10, 40, 80, 120} {
		layoutContent(sw, wd, 24)
		sep := separatorBounds(sw)
		if sep.X != 0 {
			t.Errorf("wd=%d: separator X=%d, want 0 (left edge of content)", wd, sep.X)
		}
		if sep.W != wd {
			t.Errorf("wd=%d: separator W=%d, want full width %d", wd, sep.W, wd)
		}
		if sep.H != 1 {
			t.Errorf("wd=%d: separator H=%d, want a single row", wd, sep.H)
		}
	}
}

// TestSeparatorTextIsHorizontalRule verifies the rule's text is a full-width run of
// the box-drawing horizontal glyph (─), both the exact repeated string and as a
// property (only that glyph, count == width) so a half-width/wide-rune regression
// cannot pass.
func TestSeparatorTextIsHorizontalRule(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	for _, wd := range []int{4, 16, 40, 80} {
		layoutContent(sw, wd, 24)
		sep := separatorBounds(sw)
		text := sw.separator.Text
		if want := strings.Repeat(controlsSeparatorRune, sep.W); text != want {
			t.Errorf("wd=%d: separator text=%q, want %q", wd, text, want)
		}
		if got := utf8.RuneCountInString(text); got != sep.W {
			t.Errorf("wd=%d: separator rune count=%d, want width %d", wd, got, sep.W)
		}
		if strings.Trim(text, controlsSeparatorRune) != "" {
			t.Errorf("wd=%d: separator text %q contains runes other than %q", wd, text, controlsSeparatorRune)
		}
	}
}

// TestSeparatorTracksResize verifies the rule is rebuilt from the current width on
// every layout pass (issue #195): after a resize the text length and bounds width
// follow the new width rather than keeping a stale value.
func TestSeparatorTracksResize(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	layoutContent(sw, 40, 24)
	if got := utf8.RuneCountInString(sw.separator.Text); got != 40 {
		t.Fatalf("wd=40: separator runes=%d, want 40", got)
	}
	if sep := separatorBounds(sw); sep.W != 40 {
		t.Fatalf("wd=40: separator W=%d, want 40", sep.W)
	}

	// Shrink, then grow past the original — both must update.
	for _, wd := range []int{20, 40, 65, 12} {
		layoutContent(sw, wd, 24)
		if got := utf8.RuneCountInString(sw.separator.Text); got != wd {
			t.Errorf("after resize to wd=%d: separator runes=%d, want %d", wd, got, wd)
		}
		if sep := separatorBounds(sw); sep.W != wd {
			t.Errorf("after resize to wd=%d: separator W=%d, want %d", wd, sep.W, wd)
		}
	}
}

// TestSeparatorUsesChromeDividerColour checks the rule carries the chrome divider
// colour so it reads as a separator like the ones used elsewhere (the sidebar),
// rather than as transcript text (issue #195).
func TestSeparatorUsesChromeDividerColour(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	if sw.separator.FG != chromeDivider {
		t.Errorf("separator FG=%v, want chrome divider colour %v", sw.separator.FG, chromeDivider)
	}
}

// TestSeparatorMinimumHeightGuard pins the height at which the controls region
// appears. The min-height guard was bumped from ht<6 to ht<7 to free the rule row
// while keeping history at least one row tall (issue #195): at ht=7 the separator
// is laid out and history gets exactly one row; below ht=7 the layout early-returns
// as a no-op, so an already-good layout is left untouched instead of collapsing to
// degenerate geometry (at ht=6 an unguarded layout would compute history H = 0).
func TestSeparatorMinimumHeightGuard(t *testing.T) {
	t.Run("ht=7 lays out the separator with one history row", func(t *testing.T) {
		w := newTestWorkbench(t)
		sw := w.openWindow("s", "S")
		layoutContent(sw, 40, 7)

		sep := separatorBounds(sw)
		if sep.Empty() {
			t.Fatal("ht=7: separator should be laid out")
		}
		if hb := historyBounds(sw); hb.H != 1 {
			t.Errorf("ht=7: history H=%d, want exactly 1 (header + rule + status + 3-row input)", hb.H)
		}
	})

	t.Run("ht=6 is a no-op leaving a prior good layout intact", func(t *testing.T) {
		w := newTestWorkbench(t)
		sw := w.openWindow("s", "S")
		// openWindow already lays the window out once at its default size; establish a
		// known good state explicitly, then drive a sub-threshold height.
		layoutContent(sw, 40, 24)
		sep := separatorBounds(sw)
		hist := historyBounds(sw)
		stat := statusBounds(sw)

		layoutContent(sw, 40, 6)

		// The guard fires: none of the chrome is relaid-out to ht=6 geometry. In
		// particular history keeps its good-state height rather than collapsing to
		// H=0 (which an unguarded ht-inputH-3 layout would produce at ht=6).
		if got := separatorBounds(sw); got != sep {
			t.Errorf("ht=6: separator %+v changed to %+v (guard should make this a no-op)", sep, got)
		}
		if got := historyBounds(sw); got != hist {
			t.Errorf("ht=6: history %+v changed to %+v (guard should make this a no-op)", hist, got)
		}
		if got := statusBounds(sw); got != stat {
			t.Errorf("ht=6: status %+v changed to %+v (guard should make this a no-op)", stat, got)
		}
		if got := historyBounds(sw).H; got < 1 {
			t.Errorf("ht=6: history H=%d must stay positive (the guard exists to prevent this)", got)
		}
	})
}

// TestSeparatorMinimumWidthGuard pins the width guard: at wd=4 (the minimum) the
// separator is laid out; below wd=4 the layout early-returns as a no-op, leaving a
// prior good layout untouched rather than relaying-out to a too-narrow geometry.
func TestSeparatorMinimumWidthGuard(t *testing.T) {
	t.Run("wd=4 lays out the separator", func(t *testing.T) {
		w := newTestWorkbench(t)
		sw := w.openWindow("s", "S")
		layoutContent(sw, 4, 24)
		if sep := separatorBounds(sw); sep.Empty() {
			t.Fatal("wd=4: separator should be laid out")
		}
	})

	t.Run("wd=3 is a no-op leaving a prior good layout intact", func(t *testing.T) {
		w := newTestWorkbench(t)
		sw := w.openWindow("s", "S")
		layoutContent(sw, 80, 24)
		sep := separatorBounds(sw)

		layoutContent(sw, 3, 24)

		// The guard fires: the rule keeps its good-state width instead of being
		// rebuilt at W=3, proving wd=3 did not lay out.
		if got := separatorBounds(sw); got != sep {
			t.Errorf("wd=3: separator %+v changed to %+v (guard should make this a no-op)", sep, got)
		}
	})
}

// TestHistoryHeightScalesWithWindow verifies the transcript keeps a positive height
// across all usable window heights and that the separator consumes exactly one row
// of what was previously history (H == ht-inputH-3), so the rule never eats into
// the status or input rows.
func TestHistoryHeightScalesWithWindow(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	const inputH = 3

	for _, ht := range []int{7, 8, 10, 15, 24, 40} {
		layoutContent(sw, 60, ht)
		hb := historyBounds(sw)
		if hb.H < 1 {
			t.Errorf("ht=%d: history H=%d must stay positive", ht, hb.H)
		}
		if want := ht - inputH - 3; hb.H != want {
			t.Errorf("ht=%d: history H=%d, want %d (one row yielded to the separator)", ht, hb.H, want)
		}
	}
}

// TestSeparatorDoesNotOverlapNeighbours is a property check that, across a range of
// sizes, the separator row never collides with the history above it or the status
// below it — it always sits cleanly between them (issue #195).
func TestSeparatorDoesNotOverlapNeighbours(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	for _, tc := range []struct{ wd, ht int }{
		{4, 7}, {4, 24}, {40, 7}, {40, 24}, {80, 24}, {120, 40}, {10, 10},
	} {
		layoutContent(sw, tc.wd, tc.ht)
		sep := separatorBounds(sw)
		hist := historyBounds(sw)
		stat := statusBounds(sw)
		if rectOverlap(sep, hist) {
			t.Errorf("size=%dx%d: separator %+v overlaps history %+v", tc.wd, tc.ht, sep, hist)
		}
		if rectOverlap(sep, stat) {
			t.Errorf("size=%dx%d: separator %+v overlaps status %+v", tc.wd, tc.ht, sep, stat)
		}
	}
}

// TestControlsRegionContiguousAndFullWidth verifies the controls region is a
// contiguous, full-width stack: the separator, status and input row occupy three
// consecutive row bands spanning the full inner width, so together with the window
// frame they box the controls region (issue #195's preferred option).
func TestControlsRegionContiguousAndFullWidth(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	const wd, ht = 80, 24

	layoutContent(sw, wd, ht)

	sep := separatorBounds(sw)
	stat := statusBounds(sw)
	inTop := inputRowTop(sw)

	// Three consecutive rows: separator immediately above status immediately above input.
	if stat.Y != sep.Y+1 {
		t.Errorf("status Y=%d must be separator Y+1=%d", stat.Y, sep.Y+1)
	}
	if inTop != stat.Y+stat.H {
		t.Errorf("input top=%d must be status bottom+1=%d", inTop, stat.Y+stat.H)
	}
	// Both chrome rows span the full inner width (the input row's prompt does not,
	// but the separator and status do, forming the box's top edges).
	for name, r := range map[string]tv.Rect{"separator": sep, "status": stat} {
		if r.X != 0 || r.W != wd {
			t.Errorf("%s %+v should span the full inner width X=0 W=%d", name, r, wd)
		}
	}
}

// TestSeparatorStableAcrossBusyIdle verifies the #201 button-row swap leaves the
// separator and status geometry untouched: the separator sits in the same place
// whether the window is idle (Send button) or busy (Interject/Queue/Stop), because
// the swap only relayouts the input row, not the controls-region top edge
// (integration of issue #195 with issue #201).
func TestSeparatorStableAcrossBusyIdle(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	layoutContent(sw, 80, 24)
	idleSep := separatorBounds(sw)
	idleStat := statusBounds(sw)
	idleInputTop := inputRowTop(sw)

	sw.busy = true
	layoutContent(sw, 80, 24)
	if got := separatorBounds(sw); got != idleSep {
		t.Errorf("busy separator %+v should match idle %+v (the swap must not move the rule)", got, idleSep)
	}
	if got := statusBounds(sw); got != idleStat {
		t.Errorf("busy status %+v should match idle %+v", got, idleStat)
	}
	if got := inputRowTop(sw); got != idleInputTop {
		t.Errorf("busy input top=%d should match idle=%d", got, idleInputTop)
	}

	// The status row and the running-turn buttons both sit below the separator
	// (inside the controls region), so the button row shares the fenced region.
	sepY := separatorBounds(sw).Y
	if statY := statusBounds(sw).Y; statY <= sepY {
		t.Errorf("status row Y=%d must be below separator Y=%d", statY, sepY)
	}
	for name, b := range map[string]*tv.Button{
		"interject": sw.interjectButton,
		"queue":     sw.queueButton,
		"stop":      sw.stopButton,
	} {
		if by := b.Component.Bounds.Y; by <= sepY {
			t.Errorf("busy %s button Y=%d must be below separator Y=%d (shares the controls region)", name, by, sepY)
		}
	}
}

// TestStatusNotFlushAcrossHeights is a broad regression check that the "status no
// longer flush under the transcript" property (issue #195) holds for every usable
// height, not just the common 24-row case: there is always a separator row between
// the history bottom and the status row.
func TestStatusNotFlushAcrossHeights(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	for _, ht := range []int{7, 8, 12, 20, 24, 50} {
		layoutContent(sw, 70, ht)
		hist := historyBounds(sw)
		sep := separatorBounds(sw)
		stat := statusBounds(sw)
		if sep.Y != hist.Bottom()+1 {
			t.Errorf("ht=%d: separator Y=%d must be history bottom+1=%d", ht, sep.Y, hist.Bottom()+1)
		}
		if stat.Y != hist.Bottom()+2 {
			t.Errorf("ht=%d: status Y=%d must be history bottom+2=%d (a separator row in between)",
				ht, stat.Y, hist.Bottom()+2)
		}
	}
}
