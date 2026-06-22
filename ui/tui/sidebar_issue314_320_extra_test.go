package ui

import (
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/gogent"
)

// Round-2 adversarial tests for issues #314/#320. They probe edges the first pass
// left implicit: the no-op-width persist guard, the full glyph→ToggleSidebarPin→
// clamp wiring, pin-toggle width invariance, and divider over-drag clamping after
// the affordance repaint. Same harness as sidebar_issue314_320_test.go.

// TestSidebarUnchangedWidthDoesNotArmPersist verifies setSidebarWidth's early
// return (width == current) skips scheduleLayoutPersist entirely, so a divider
// drag that hits a clamp and re-reports the same width does not keep re-arming the
// timer (no needless write is queued for a no-op).
func TestSidebarUnchangedWidthDoesNotArmPersist(t *testing.T) {
	w := newTestWorkbench(t)
	stopLayoutTimer(t, w)
	w.handlers.SaveLayout = func(gogent.Layout) {}

	w.setSidebarWidth(w.sidebarWidth()) // no-op: same as current
	if w.layoutPersist != nil {
		t.Error("a no-op width change armed the layout-persist timer; want no schedule")
	}
}

// TestPinToggleGlyphClampsWindows drives the new header glyph end-to-end: clicking
// it to re-pin must run the same clamp ToggleSidebarPin does, pulling a window that
// was parked over the sidebar back inside the reserved strip. This proves the glyph
// is wired to the real pin behaviour, not just the boolean.
func TestPinToggleGlyphClampsWindows(t *testing.T) {
	w := newTestWorkbench(t)
	c := w.sidebar.pinToggle

	// Unpin via the glyph so a window can be placed over the sidebar.
	c.OnClickFn(c, tui.ClickEvent{Down: true})
	if w.IsSidebarPinned() {
		t.Fatal("precondition: glyph click should have unpinned")
	}
	w.openWindow("a", "A")
	sw := w.sessions["a"]
	sw.window.Component.SetBounds(tv.Rect{X: 30, Y: 2, W: 50, H: 14})
	if rightEdge(sw) <= w.app.Width()-defaultSidebarWidth {
		t.Fatalf("precondition: window should cover the sidebar, right edge %d", rightEdge(sw))
	}

	// Re-pin via the glyph -> must clamp the overlapping window in.
	c.OnClickFn(c, tui.ClickEvent{Down: true})
	if !w.IsSidebarPinned() {
		t.Fatal("glyph click did not re-pin")
	}
	if got := rightEdge(sw); got > w.app.Width()-defaultSidebarWidth {
		t.Errorf("glyph re-pin did not clamp window: right edge %d > %d",
			got, w.app.Width()-defaultSidebarWidth)
	}
}

// TestPinToggleDoesNotChangeWidth guards that pinning/unpinning via the glyph never
// perturbs the sidebar width — the two controls are orthogonal and must not bleed
// into each other.
func TestPinToggleDoesNotChangeWidth(t *testing.T) {
	w := newTestWorkbench(t)
	w.setSidebarWidth(36)
	start := w.sidebarWidth()
	c := w.sidebar.pinToggle

	c.OnClickFn(c, tui.ClickEvent{Down: true}) // unpin
	c.OnClickFn(c, tui.ClickEvent{Down: true}) // re-pin
	if got := w.sidebarWidth(); got != start {
		t.Errorf("pin toggle changed sidebar width to %d, want %d", got, start)
	}
}

// TestDividerOverDragClampsWidth verifies the drag mechanics still clamp correctly
// after the #314 paint changes: dragging the divider past the max pins at the
// work-area floor, and past the min pins at minSidebarWidth. On the 80-col test
// desktop the range is [24, 40].
func TestDividerOverDragClampsWidth(t *testing.T) {
	w := newTestWorkbench(t)
	stopLayoutTimer(t, w)
	c := w.sidebar.divider
	max := w.app.Width() - minWorkAreaWidth // 40

	// Drag far left (small X) -> width would be huge -> clamp to max.
	c.OnClickFn(c, tui.ClickEvent{X: w.app.Width() - 60, Y: 5, Down: true}) // req width 60
	if got := w.sidebarWidth(); got != max {
		t.Errorf("over-wide drag width = %d, want clamped max %d", got, max)
	}
	// Drag far right (large X) -> width would be tiny -> clamp to min.
	c.OnClickFn(c, tui.ClickEvent{X: w.app.Width() - 10, Y: 5, Down: true, Drag: true}) // req width 10
	if got := w.sidebarWidth(); got != minSidebarWidth {
		t.Errorf("under-min drag width = %d, want clamped min %d", got, minSidebarWidth)
	}
	c.OnClickFn(c, tui.ClickEvent{X: w.app.Width() - 10, Y: 5, Down: false}) // release
}

// TestPinGlyphDistinctFromFavoriteMarker is an explicit guard for the #314
// constraint: the pin glyphs must be the filled/outline square pair and must never
// collide with ★, the session-favorite marker, in either state.
func TestPinGlyphDistinctFromFavoriteMarker(t *testing.T) {
	w := newTestWorkbench(t)
	read := func() rune {
		w.desktop.Redraw()
		abs := w.sidebar.pinToggle.AbsoluteBounds()
		return w.app.ReadCell(abs.X, abs.Y).Ch
	}
	pinned := read()
	w.ToggleSidebarPin()
	unpinned := read()

	for _, g := range []rune{pinned, unpinned} {
		if g == '★' {
			t.Errorf("pin glyph %q collides with the session-favorite marker ★", g)
		}
	}
	if pinned != '▣' || unpinned != '□' {
		t.Errorf("pin glyphs = (pinned %q, unpinned %q), want (▣, □)", pinned, unpinned)
	}
}
