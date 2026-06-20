package ui

import (
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/gogent"
)

// TestSidebarWidthDefault verifies a fresh workbench starts at the default
// sidebar width and the sidebar panel is repositioned to match (issue #175).
func TestSidebarWidthDefault(t *testing.T) {
	w := newTestWorkbench(t)
	if got := w.sidebarWidth(); got != defaultSidebarWidth {
		t.Fatalf("default sidebar width = %d, want %d", got, defaultSidebarWidth)
	}
	// reposition pins the panel's left edge at screenW - width.
	if got := w.sidebar.panel.Bounds.W; got != defaultSidebarWidth {
		t.Errorf("sidebar panel width = %d, want %d", got, defaultSidebarWidth)
	}
	if got := w.sidebar.panel.Bounds.X; got != w.app.Width()-defaultSidebarWidth {
		t.Errorf("sidebar panel X = %d, want %d", got, w.app.Width()-defaultSidebarWidth)
	}
}

// TestSidebarWidthClampRange verifies the width is held within [minSidebarWidth,
// app.Width()-minWorkAreaWidth] (issue #175). The test desktop is 80 cols, so the
// valid range is [24, 40].
func TestSidebarWidthClampRange(t *testing.T) {
	w := newTestWorkbench(t)
	max := w.app.Width() - minWorkAreaWidth

	w.setSidebarWidth(1000) // way over max
	if got := w.sidebarWidth(); got != max {
		t.Errorf("over-wide width clamped to %d, want max %d", got, max)
	}

	w.setSidebarWidth(1) // way under min
	if got := w.sidebarWidth(); got != minSidebarWidth {
		t.Errorf("under-min width clamped to %d, want min %d", got, minSidebarWidth)
	}

	w.setSidebarWidth(30) // in range, untouched
	if got := w.sidebarWidth(); got != 30 {
		t.Errorf("in-range width = %d, want 30", got)
	}
}

// TestSidebarWidthTinyTerminal verifies that on a terminal too narrow to honour
// both the min sidebar width and the work-area floor, the work-area floor wins so
// the sidebar shrinks rather than the work area vanishing (issue #175). With a
// 50-col app and minWorkAreaWidth 40 the max is 10, below minSidebarWidth 24, so
// the max wins.
func TestSidebarWidthTinyTerminal(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(50, 25)
	got := w.clampSidebarWidth(defaultSidebarWidth)
	want := 50 - minWorkAreaWidth // 10
	if got != want {
		t.Errorf("tiny-terminal width = %d, want %d (work-area floor wins)", got, want)
	}
}

// TestWindowAreaTracksSidebarWidth verifies the window area shrinks as the sidebar
// widens and grows as it narrows, so pinned windows track the moved boundary
// (issue #175).
func TestWindowAreaTracksSidebarWidth(t *testing.T) {
	w := newTestWorkbench(t)
	full := w.app.Width()

	w.setSidebarWidth(40)
	if got := w.windowArea().W; got != full-40 {
		t.Errorf("widened: window area W = %d, want %d", got, full-40)
	}

	w.setSidebarWidth(24)
	if got := w.windowArea().W; got != full-24 {
		t.Errorf("narrowed: window area W = %d, want %d", got, full-24)
	}
}

// TestSidebarDragChangesWidth drives the divider's click handler with a press at a
// column left of the current left edge (a leftward drag) and verifies the width
// grows to span from that column to the right edge (issue #175).
func TestSidebarDragChangesWidth(t *testing.T) {
	w := newTestWorkbench(t)
	// Dragging the divider to column X makes the width screenW - X. Pick X so the
	// resulting width (36) is inside the [24,40] range.
	dragX := w.app.Width() - 36
	c := w.sidebar.divider
	c.OnClickFn(c, tui.ClickEvent{X: dragX, Y: 5, Down: true})
	if got := w.sidebarWidth(); got != 36 {
		t.Errorf("after divider drag, width = %d, want 36", got)
	}
	// A release (Down=false) is a no-op for the width.
	c.OnClickFn(c, tui.ClickEvent{X: dragX, Y: 5, Down: false})
	if got := w.sidebarWidth(); got != 36 {
		t.Errorf("release changed width to %d, want 36", got)
	}
}

// TestSidebarDragClampsPinnedWindow verifies that widening the sidebar by drag
// pulls a pinned window that now overlaps the wider strip back into the work area
// (issue #175).
func TestSidebarDragClampsPinnedWindow(t *testing.T) {
	w := newSlimMinWorkbench(t)
	w.openWindow("a", "A")
	sw := w.sessions["a"]
	// Park the window so its right edge sits in the strip that widening will claim.
	sw.window.Component.SetBounds(tv.Rect{X: 2, Y: 2, W: w.app.Width() - 30, H: 14})
	w.setSidebarWidth(40)
	if got := rightEdge(sw); got > w.app.Width()-40 {
		t.Errorf("window covers widened sidebar, right edge %d > %d",
			got, w.app.Width()-40)
	}
}

// TestSidebarWidthNudge verifies the Widen/Narrow keyboard fallback steps the
// width by sidebarNudge columns and clamps at the bounds (issue #175).
func TestSidebarWidthNudge(t *testing.T) {
	w := newTestWorkbench(t)
	start := w.sidebarWidth()
	w.nudgeSidebarWidth(+sidebarNudge)
	if got := w.sidebarWidth(); got != start+sidebarNudge {
		t.Errorf("widen: width = %d, want %d", got, start+sidebarNudge)
	}
	w.nudgeSidebarWidth(-sidebarNudge)
	if got := w.sidebarWidth(); got != start {
		t.Errorf("narrow: width = %d, want %d", got, start)
	}
}

// TestSidebarWidthLayoutRoundTrip verifies the live width is captured into the
// layout and restored by applyLayout, and that an unset (0) width falls back to
// the default so pre-#175 layout files still load (issue #175).
func TestSidebarWidthLayoutRoundTrip(t *testing.T) {
	w := newTestWorkbench(t)
	w.setSidebarWidth(36)
	layout := w.captureLayout()
	if layout.SidebarWidth != 36 {
		t.Fatalf("captured SidebarWidth = %d, want 36", layout.SidebarWidth)
	}

	// Restore onto a fresh workbench.
	w2 := newTestWorkbench(t)
	w2.applyLayout(layout)
	if got := w2.sidebarWidth(); got != 36 {
		t.Errorf("restored width = %d, want 36", got)
	}
	if got := w2.sidebar.panel.Bounds.W; got != 36 {
		t.Errorf("restored sidebar panel width = %d, want 36", got)
	}

	// A legacy layout with no width (0) keeps the default.
	w3 := newTestWorkbench(t)
	w3.applyLayout(gogent.Layout{Entries: []gogent.LayoutEntry{{ID: "x"}}})
	if got := w3.sidebarWidth(); got != defaultSidebarWidth {
		t.Errorf("legacy layout width = %d, want default %d", got, defaultSidebarWidth)
	}
}
