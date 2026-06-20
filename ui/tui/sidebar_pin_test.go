package ui

import (
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/gogent"
)

// dragWindowTo simulates a title-bar drag that moves the window's origin to
// (toX, toY) by driving the window's (wrapped) click handler with a press, move
// and release. The press lands on the title bar's left edge so the recorded drag
// offset is zero, making the move land the origin exactly at (toX, toY) before the
// sidebar clamp runs.
func dragWindowTo(sw *SessionWindow, toX, toY int) {
	c := sw.window.Component
	abs := c.AbsoluteBounds()
	c.OnClickFn(c, tui.ClickEvent{X: abs.X, Y: abs.Y, Down: true}) // press: zero offset
	c.OnClickFn(c, tui.ClickEvent{X: toX, Y: toY, Down: true})     // move
	c.OnClickFn(c, tui.ClickEvent{X: toX, Y: toY, Down: false})    // release
}

// resizeWindowTo simulates dragging the bottom-right resize grip to (toX, toY).
func resizeWindowTo(sw *SessionWindow, toX, toY int) {
	c := sw.window.Component
	abs := c.AbsoluteBounds()
	c.OnClickFn(c, tui.ClickEvent{X: abs.Right(), Y: abs.Bottom(), Down: true}) // press grip
	c.OnClickFn(c, tui.ClickEvent{X: toX, Y: toY, Down: true})                  // drag grip
	c.OnClickFn(c, tui.ClickEvent{X: toX, Y: toY, Down: false})                 // release
}

// rightEdge returns the column just past a window's right border (X+W), the value
// the sidebar clamp holds at or below screenW-sidebarWidth when pinned.
func rightEdge(sw *SessionWindow) int {
	b := sw.window.Component.Bounds
	return b.X + b.W
}

// newSlimMinWorkbench returns a test workbench whose minimum window size is small
// enough to fit inside the 80x25 test desktop's pinned area (width 48). The
// default MinWidth of 50 is wider than that area, which would make the area/min
// constraints fight; real (wider) terminals do not hit this, but the test desktop
// does, so the geometry tests opt into a slimmer minimum.
func newSlimMinWorkbench(t *testing.T) *Workbench {
	t.Helper()
	w := newTestWorkbench(t)
	w.windowConfig.MinWidth = 10
	w.windowConfig.MinHeight = 4
	return w
}

// newDraggableWindow opens a window with no title-bar buttons (close/minimize/
// maximize all off) and a slim minimum size, so a simulated title-bar drag or
// corner resize is never intercepted by a button click. It returns the workbench
// and the window.
func newDraggableWindow(t *testing.T) (*Workbench, *SessionWindow) {
	t.Helper()
	w := newSlimMinWorkbench(t)
	w.windowConfig.Maximizable = false // no maximize button to intercept drags
	w.openWindow("a", "A")
	sw := w.sessions["a"]
	sw.window.ShowClose = false
	sw.window.Minimizable = false
	return w, sw
}

// TestSidebarPinDefault verifies the sidebar is pinned by default so the window
// area excludes the sidebar strip (issue #106).
func TestSidebarPinDefault(t *testing.T) {
	w := newTestWorkbench(t)
	if !w.IsSidebarPinned() {
		t.Fatal("sidebar should be pinned by default")
	}
	want := tv.Rect{X: 0, Y: 0, W: w.app.Width() - sidebarWidth, H: w.app.Height()}
	if got := w.windowArea(); got != want {
		t.Errorf("pinned windowArea = %+v, want %+v", got, want)
	}
}

// TestWindowAreaPinnedVsUnpinned verifies the window area is the desktop minus the
// sidebar when pinned and the full desktop when unpinned (issue #106).
func TestWindowAreaPinnedVsUnpinned(t *testing.T) {
	w := newTestWorkbench(t)
	full := tv.Rect{X: 0, Y: 0, W: w.app.Width(), H: w.app.Height()}
	reduced := tv.Rect{X: 0, Y: 0, W: w.app.Width() - sidebarWidth, H: w.app.Height()}

	if got := w.windowArea(); got != reduced {
		t.Errorf("pinned windowArea = %+v, want %+v", got, reduced)
	}

	w.ToggleSidebarPin()
	if w.IsSidebarPinned() {
		t.Fatal("expected sidebar unpinned after toggle")
	}
	if got := w.windowArea(); got != full {
		t.Errorf("unpinned windowArea = %+v, want %+v", got, full)
	}
}

// TestToggleSidebarPinClampsExistingWindows verifies that turning the pin on
// pulls every window that was covering the sidebar back into the reserved area
// (issue #106).
func TestToggleSidebarPinClampsExistingWindows(t *testing.T) {
	w := newTestWorkbench(t)
	w.ToggleSidebarPin() // unpin first so we can place a window over the sidebar
	if w.IsSidebarPinned() {
		t.Fatal("precondition: sidebar should be unpinned")
	}
	w.openWindow("a", "A")
	sw := w.sessions["a"]
	// Park the window so its right edge is well past the sidebar boundary.
	sw.window.Component.SetBounds(tv.Rect{X: 30, Y: 2, W: 50, H: 14})
	if rightEdge(sw) <= w.app.Width()-sidebarWidth {
		t.Fatalf("precondition: window should cover the sidebar, right edge %d", rightEdge(sw))
	}

	w.ToggleSidebarPin() // pin again -> clamp
	if !w.IsSidebarPinned() {
		t.Fatal("expected sidebar pinned after toggle")
	}
	if got := rightEdge(sw); got > w.app.Width()-sidebarWidth {
		t.Errorf("window still covers the sidebar after pinning, right edge %d > %d",
			got, w.app.Width()-sidebarWidth)
	}
}

// TestSidebarPinConstrainsDrag verifies a pinned sidebar stops a window being
// dragged over it (the window slides along the boundary), while unpinning restores
// the free drag that lets the window cover it (issue #106).
func TestSidebarPinConstrainsDrag(t *testing.T) {
	w, sw := newDraggableWindow(t)
	sw.window.Component.SetBounds(tv.Rect{X: 2, Y: 2, W: 40, H: 14})

	// Drag the origin far to the right; pinned, the right edge must stop at the
	// sidebar boundary and the window width is unchanged (a slide, not a resize).
	dragWindowTo(sw, 30, 2)
	b := sw.window.Component.Bounds
	if rightEdge(sw) > w.app.Width()-sidebarWidth {
		t.Errorf("pinned drag covered the sidebar, right edge %d > %d",
			rightEdge(sw), w.app.Width()-sidebarWidth)
	}
	if b.W != 40 {
		t.Errorf("pinned drag changed width to %d, want 40 (drag must not resize)", b.W)
	}

	// Unpinning restores the free drag: the same move now covers the sidebar.
	w.ToggleSidebarPin()
	sw.window.Component.SetBounds(tv.Rect{X: 2, Y: 2, W: 40, H: 14})
	dragWindowTo(sw, 30, 2)
	if rightEdge(sw) <= w.app.Width()-sidebarWidth {
		t.Errorf("unpinned drag should cover the sidebar, right edge %d <= %d",
			rightEdge(sw), w.app.Width()-sidebarWidth)
	}
}

// TestSidebarPinConstrainsResize verifies a pinned sidebar caps a resize at the
// boundary while keeping the anchored origin (no leftward jump), and that
// unpinning restores free resizing over the sidebar (issue #106).
func TestSidebarPinConstrainsResize(t *testing.T) {
	w, sw := newDraggableWindow(t)
	sw.window.Component.SetBounds(tv.Rect{X: 2, Y: 2, W: 40, H: 14})

	// Resize the grip far past the sidebar; pinned, the right edge stops at the
	// boundary and the left origin (X=2) is preserved.
	resizeWindowTo(sw, 70, 15)
	b := sw.window.Component.Bounds
	if rightEdge(sw) > w.app.Width()-sidebarWidth {
		t.Errorf("pinned resize covered the sidebar, right edge %d > %d",
			rightEdge(sw), w.app.Width()-sidebarWidth)
	}
	if b.X != 2 {
		t.Errorf("pinned resize moved origin to X=%d, want 2 (anchored edge should stay)", b.X)
	}

	// Unpinning restores free resizing over the sidebar.
	w.ToggleSidebarPin()
	sw.window.Component.SetBounds(tv.Rect{X: 2, Y: 2, W: 40, H: 14})
	resizeWindowTo(sw, 70, 15)
	if rightEdge(sw) <= w.app.Width()-sidebarWidth {
		t.Errorf("unpinned resize should cover the sidebar, right edge %d <= %d",
			rightEdge(sw), w.app.Width()-sidebarWidth)
	}
}

// TestSidebarPinConstrainsMaximize verifies maximize respects the pinned boundary
// and fills the full desktop when unpinned (issue #106).
func TestSidebarPinConstrainsMaximize(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	sw := w.sessions["a"]

	sw.Maximize()
	if got := rightEdge(sw); got > w.app.Width()-sidebarWidth {
		t.Errorf("pinned maximize covered the sidebar, right edge %d > %d",
			got, w.app.Width()-sidebarWidth)
	}
	sw.ToggleMaximize() // restore before changing the pin state

	// Unpinned: maximize fills the whole desktop, including the sidebar columns.
	w.ToggleSidebarPin()
	sw.Maximize()
	if got := sw.window.Component.Bounds; got.W != w.app.Width() {
		t.Errorf("unpinned maximize width = %d, want full %d", got.W, w.app.Width())
	}
}

// TestSidebarPinConstrainsRestoredLayout verifies a window restored from a saved
// layout (which may have been saved overlapping the sidebar) is clamped into the
// pinned area (issue #106).
func TestSidebarPinConstrainsRestoredLayout(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	sw := w.sessions["a"]

	// A layout saved with the window parked over the sidebar.
	w.applyLayout(gogent.Layout{Entries: []gogent.LayoutEntry{
		{ID: "a", Title: "A", X: 30, Y: 2, W: 50, H: 14},
	}})
	if rightEdge(sw) > w.app.Width()-sidebarWidth {
		t.Errorf("restored window covers the sidebar, right edge %d > %d",
			rightEdge(sw), w.app.Width()-sidebarWidth)
	}
}

// TestSidebarPinLeavesInBoundsWindow verifies the clamp is a no-op for a window
// already inside the pinned area, so normal dragging within the area is not
// disturbed (issue #106).
func TestSidebarPinLeavesInBoundsWindow(t *testing.T) {
	_, sw := newDraggableWindow(t)
	inBounds := tv.Rect{X: 2, Y: 2, W: 40, H: 14} // right edge 42, well left of 48
	sw.window.Component.SetBounds(inBounds)

	dragWindowTo(sw, 5, 3) // a small drag that stays inside the area
	got := sw.window.Component.Bounds
	if got.X != 5 || got.Y != 3 || got.W != 40 || got.H != 14 {
		t.Errorf("in-bounds drag should be unconstrained, got %+v", got)
	}
}

// TestSidebarPinSkipsMinimized verifies the clamp leaves a minimized window's
// single-row title bar at height 1 instead of enlarging it back to MinHeight
// (issue #106).
func TestSidebarPinSkipsMinimized(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	sw := w.sessions["a"]
	sw.window.Component.SetBounds(tv.Rect{X: 30, Y: 2, W: 50, H: 14})
	sw.window.Minimize()
	if h := sw.window.Component.Bounds.H; h != 1 {
		t.Fatalf("precondition: minimized height = %d, want 1", h)
	}

	// Driving a click (and pinning) must not enlarge the minimized bar.
	w.ToggleSidebarPin() // unpin
	w.ToggleSidebarPin() // pin again -> would clamp, but minimized is skipped
	if h := sw.window.Component.Bounds.H; h != 1 {
		t.Errorf("minimized window enlarged to height %d, want 1", h)
	}
}

// TestClampWindowSize exercises the resize clamp: it caps the dragged edge at the
// area while keeping the origin, enforces the minimum, and leaves a window whose
// origin is already outside the area for the drag clamp to handle.
func TestClampWindowSize(t *testing.T) {
	area := tv.Rect{X: 0, Y: 0, W: 48, H: 25}
	for _, tc := range []struct {
		name       string
		in         tv.Rect
		minW, minH int
		want       tv.Rect
	}{
		{"fits unchanged", tv.Rect{X: 2, Y: 2, W: 40, H: 14}, 10, 5, tv.Rect{X: 2, Y: 2, W: 40, H: 14}},
		{"width capped at area right, origin kept", tv.Rect{X: 2, Y: 2, W: 60, H: 14}, 10, 5, tv.Rect{X: 2, Y: 2, W: 46, H: 14}},
		{"width capped from a mid-area origin", tv.Rect{X: 10, Y: 2, W: 60, H: 14}, 10, 5, tv.Rect{X: 10, Y: 2, W: 38, H: 14}},
		{"height capped at area bottom", tv.Rect{X: 2, Y: 2, W: 40, H: 30}, 10, 5, tv.Rect{X: 2, Y: 2, W: 40, H: 23}},
		{"both width and height capped", tv.Rect{X: 2, Y: 2, W: 60, H: 30}, 10, 5, tv.Rect{X: 2, Y: 2, W: 46, H: 23}},
		{"below minimum bumped up", tv.Rect{X: 2, Y: 2, W: 5, H: 3}, 10, 5, tv.Rect{X: 2, Y: 2, W: 10, H: 5}},
		{"origin past area width left as-is", tv.Rect{X: 60, Y: 2, W: 40, H: 14}, 10, 5, tv.Rect{X: 60, Y: 2, W: 40, H: 14}},
		{"minimum wins when origin too close to edge", tv.Rect{X: 40, Y: 2, W: 60, H: 14}, 45, 5, tv.Rect{X: 40, Y: 2, W: 45, H: 14}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampWindowSize(tc.in, area, tc.minW, tc.minH); got != tc.want {
				t.Errorf("clampWindowSize(%+v, %+v, %d, %d) = %+v, want %+v",
					tc.in, area, tc.minW, tc.minH, got, tc.want)
			}
		})
	}
}
