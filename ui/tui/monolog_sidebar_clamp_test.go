package ui

import (
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/config"
)

// Issue #319: the sub-agent monologue popup must honour the pinned "Sessions &
// Agents" sidebar boundary the same way a session window does — it must open
// inside the window area, clamp drag/resize at the boundary, and be pulled back
// inside when the sidebar is pinned on or widened.
//
// The test desktop is 80x25 (tui.New falls back to that off a TTY) and the
// default sidebar width is 32, so the pinned window area is {0,0,48,25} and the
// sidebar boundary (the column the monologue's right edge must stay at or left
// of) is app.Width()-defaultSidebarWidth = 48.

// newMonologWorkbench builds a test workbench whose GetTranscript handler is wired
// so showAgentMonolog actually opens a popup (it returns early when the handler is
// nil). The transcript content is irrelevant to the geometry under test.
func newMonologWorkbench(t *testing.T) *Workbench {
	t.Helper()
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.SetHandlers(Handlers{
		GetTranscript: func(sessionID, agentID string) []ChatMessage {
			return []ChatMessage{{Role: "assistant", Content: "sub-agent output"}}
		},
	})
	return w
}

// openMonolog opens a monologue popup and returns its window, failing if none was
// created.
func openMonolog(t *testing.T, w *Workbench) *tv.Window {
	t.Helper()
	w.showAgentMonolog("s1", "a1", "counter")
	if w.monolog == nil || w.monologWindow == nil {
		t.Fatal("showAgentMonolog did not open a monologue popup")
	}
	return w.monologWindow
}

// dragBareWindowTo drives a bare *tv.Window's (wrapped) click handler with a
// title-bar press/move/release that lands the origin at (toX, toY) before the
// sidebar clamp runs — the bare-window analogue of dragWindowTo in
// sidebar_pin_test.go. The press lands on the title bar's left edge so the
// recorded drag offset is zero.
func dragBareWindowTo(win *tv.Window, toX, toY int) {
	c := win.Component
	abs := c.AbsoluteBounds()
	c.OnClickFn(c, tui.ClickEvent{X: abs.X, Y: abs.Y, Down: true}) // press: zero offset
	c.OnClickFn(c, tui.ClickEvent{X: toX, Y: toY, Down: true})     // move
	c.OnClickFn(c, tui.ClickEvent{X: toX, Y: toY, Down: false})    // release
}

// winRightEdge returns the column just past a window's right border (X+W).
func winRightEdge(win *tv.Window) int {
	b := win.Component.Bounds
	return b.X + b.W
}

// TestMonologOpensInsidePinnedArea verifies the monologue opens entirely left of
// the pinned sidebar (issue #319 §2): on the 80x25 test desktop it should resolve
// to {4,2,40,21}, right edge 44 <= 48.
func TestMonologOpensInsidePinnedArea(t *testing.T) {
	w := newMonologWorkbench(t)
	if !w.IsSidebarPinned() {
		t.Fatal("precondition: sidebar should be pinned by default")
	}
	win := openMonolog(t, w)

	boundary := w.app.Width() - defaultSidebarWidth
	if got := winRightEdge(win); got > boundary {
		t.Errorf("monologue opened straddling the sidebar, right edge %d > %d", got, boundary)
	}
	want := tv.Rect{X: 4, Y: 2, W: 40, H: 21}
	if got := win.Component.Bounds; got != want {
		t.Errorf("monologue open bounds = %+v, want %+v", got, want)
	}
}

// TestMonologOpensFullScreenWhenUnpinned verifies the unpinned case is unchanged:
// the monologue centers on the full screen and may cover the sidebar columns
// (issue #319 §4).
func TestMonologOpensFullScreenWhenUnpinned(t *testing.T) {
	w := newMonologWorkbench(t)
	w.ToggleSidebarPin() // unpin
	if w.IsSidebarPinned() {
		t.Fatal("precondition: sidebar should be unpinned")
	}
	win := openMonolog(t, w)

	boundary := w.app.Width() - defaultSidebarWidth
	if got := winRightEdge(win); got <= boundary {
		t.Errorf("unpinned monologue should center on the full screen, right edge %d <= %d", got, boundary)
	}
	// Full-screen 80% width centering: {8,2,64,21}.
	want := tv.Rect{X: 8, Y: 2, W: 64, H: 21}
	if got := win.Component.Bounds; got != want {
		t.Errorf("unpinned monologue open bounds = %+v, want %+v", got, want)
	}
}

// TestMonologDragClampedPinned verifies a title-bar drag that would push the
// monologue over the pinned sidebar is clamped back to the boundary, and that the
// width is unchanged (a slide, not a resize) — issue #319 §1.
func TestMonologDragClampedPinned(t *testing.T) {
	w := newMonologWorkbench(t)
	win := openMonolog(t, w)
	widthBefore := win.Component.Bounds.W

	dragBareWindowTo(win, 30, 2) // origin far right; right edge would be 30+40=70
	b := win.Component.Bounds
	boundary := w.app.Width() - defaultSidebarWidth
	if got := winRightEdge(win); got > boundary {
		t.Errorf("pinned drag covered the sidebar, right edge %d > %d", got, boundary)
	}
	if b.W != widthBefore {
		t.Errorf("pinned drag changed width to %d, want %d (drag must not resize)", b.W, widthBefore)
	}
}

// TestMonologDragFreeWhenUnpinned verifies the same drag is unconstrained when the
// sidebar is unpinned: the monologue can be dragged over the sidebar columns
// (issue #319 §4).
func TestMonologDragFreeWhenUnpinned(t *testing.T) {
	w := newMonologWorkbench(t)
	w.ToggleSidebarPin() // unpin
	win := openMonolog(t, w)

	dragBareWindowTo(win, 30, 2)
	boundary := w.app.Width() - defaultSidebarWidth
	if got := winRightEdge(win); got <= boundary {
		t.Errorf("unpinned drag should cover the sidebar, right edge %d <= %d", got, boundary)
	}
	if got := win.Component.Bounds.X; got != 30 {
		t.Errorf("unpinned drag origin = %d, want 30 (free move)", got)
	}
}

// TestMonologDragWithinAreaUnconstrained verifies the clamp is a no-op for a drag
// that stays inside the pinned area, so normal dragging is not disturbed.
func TestMonologDragWithinAreaUnconstrained(t *testing.T) {
	w := newMonologWorkbench(t)
	win := openMonolog(t, w)
	// Opened at {4,2,40,21}; a small leftward drag keeps the whole window in-area.
	dragBareWindowTo(win, 1, 1)
	want := tv.Rect{X: 1, Y: 1, W: 40, H: 21}
	if got := win.Component.Bounds; got != want {
		t.Errorf("in-area drag should be unconstrained, got %+v, want %+v", got, want)
	}
}

// TestMonologPinOnPullsItBack verifies that pinning the sidebar on while a
// monologue is open over the panel pulls it back inside the boundary, the same as
// session windows (issue #319 §3).
func TestMonologPinOnPullsItBack(t *testing.T) {
	w := newMonologWorkbench(t)
	w.ToggleSidebarPin() // unpin first so the monologue opens over the sidebar
	win := openMonolog(t, w)
	boundary := w.app.Width() - defaultSidebarWidth
	if winRightEdge(win) <= boundary {
		t.Fatalf("precondition: unpinned monologue should cover the sidebar, right edge %d", winRightEdge(win))
	}

	w.ToggleSidebarPin() // pin again -> re-clamp the open monologue
	if !w.IsSidebarPinned() {
		t.Fatal("expected sidebar pinned after toggle")
	}
	if got := winRightEdge(win); got > boundary {
		t.Errorf("monologue still covers the sidebar after pinning, right edge %d > %d", got, boundary)
	}
}

// TestMonologWidthChangePullsItBack verifies widening the pinned sidebar pulls an
// already-open monologue back inside the narrowed window area (issue #319 §3).
func TestMonologWidthChangePullsItBack(t *testing.T) {
	w := newMonologWorkbench(t)
	win := openMonolog(t, w) // pinned: opens at {4,2,40,21}, right edge 44

	// Widen the sidebar to its maximum (40 on an 80-col desktop -> work area 40),
	// which moves the boundary left of the open monologue's right edge (44).
	w.setSidebarWidth(40)
	boundary := w.app.Width() - w.sidebarWidth()
	if boundary != 40 {
		t.Fatalf("precondition: boundary after widening = %d, want 40", boundary)
	}
	if got := winRightEdge(win); got > boundary {
		t.Errorf("widening the sidebar did not pull the monologue back, right edge %d > %d", got, boundary)
	}
}

// TestMonologWidthChangeIgnoredWhenUnpinned verifies a width change does not move
// the monologue while the sidebar is unpinned (the re-clamp is gated on pinned).
func TestMonologWidthChangeIgnoredWhenUnpinned(t *testing.T) {
	w := newMonologWorkbench(t)
	w.ToggleSidebarPin() // unpin
	win := openMonolog(t, w)
	before := win.Component.Bounds

	w.setSidebarWidth(40)
	if got := win.Component.Bounds; got != before {
		t.Errorf("unpinned width change moved the monologue: %+v -> %+v", before, got)
	}
}

// TestMonologResizeHookKeepsItInArea verifies the layer's OnResize hook re-resolves
// the monologue against the (pinned) window area so a terminal resize keeps it clear
// of the sidebar (issue #319 §2). Driving OnResize after nudging the window proves it
// re-centers inside the area rather than the full screen.
func TestMonologResizeHookKeepsItInArea(t *testing.T) {
	w := newMonologWorkbench(t)
	win := openMonolog(t, w)
	if w.monolog.OnResize == nil {
		t.Fatal("monologue layer has no OnResize hook")
	}

	// Park it over the sidebar, then fire the resize hook.
	win.Component.SetBounds(tv.Rect{X: 30, Y: 5, W: 64, H: 21})
	w.monolog.OnResize(tv.Rect{})

	boundary := w.app.Width() - defaultSidebarWidth
	if got := winRightEdge(win); got > boundary {
		t.Errorf("OnResize left the monologue over the sidebar, right edge %d > %d", got, boundary)
	}
	want := tv.Rect{X: 4, Y: 2, W: 40, H: 21}
	if got := win.Component.Bounds; got != want {
		t.Errorf("OnResize bounds = %+v, want %+v", got, want)
	}
}

// TestMonologResizeHookGrowsWithTerminal verifies the resize hook re-resolves the
// monologue against the *current* terminal size and still respects the pinned
// boundary after the terminal grows (issue #319 §2).
func TestMonologResizeHookGrowsWithTerminal(t *testing.T) {
	w := newMonologWorkbench(t)
	win := openMonolog(t, w)

	w.app.Resize(120, 40)
	w.monolog.OnResize(tv.Rect{})

	boundary := w.app.Width() - w.sidebarWidth()
	if got := winRightEdge(win); got > boundary {
		t.Errorf("after terminal grew, monologue covers the sidebar, right edge %d > %d", got, boundary)
	}
	// It should have grown beyond its 80x25 size (40x21) toward ~80%x85% of the
	// 88-wide work area.
	if b := win.Component.Bounds; b.W <= 40 {
		t.Errorf("monologue did not grow with the terminal, width %d", b.W)
	}
}

// TestMonologReplacedKeepsSingleWindow verifies opening a second monologue replaces
// the first and re-points monologWindow at the new window (issue #302 / #319 §3).
func TestMonologReplacedKeepsSingleWindow(t *testing.T) {
	w := newMonologWorkbench(t)
	first := openMonolog(t, w)

	w.showAgentMonolog("s1", "a2", "second")
	second := w.monologWindow
	if second == nil {
		t.Fatal("second monologue did not open")
	}
	if second == first {
		t.Error("opening a second monologue should create a new window")
	}
	if w.monolog == nil {
		t.Error("monolog layer should be set after reopen")
	}
}

// TestMonologCloseClearsWindow verifies closing the monologue clears both the layer
// and the stored window so the re-clamp paths see no stale window (issue #319 §3).
func TestMonologCloseClearsWindow(t *testing.T) {
	w := newMonologWorkbench(t)
	win := openMonolog(t, w)

	win.OnClose(win)
	if w.monolog != nil {
		t.Error("monolog layer should be nil after close")
	}
	if w.monologWindow != nil {
		t.Error("monologWindow should be nil after close")
	}

	// The re-clamp paths must tolerate a closed monologue (nil window).
	w.ToggleSidebarPin()
	w.ToggleSidebarPin()
	w.setSidebarWidth(36)
}

// TestMonologNotOpenedWithoutHandler verifies showAgentMonolog is a no-op (no
// window, no stored pointer) when the GetTranscript handler is unset.
func TestMonologNotOpenedWithoutHandler(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.showAgentMonolog("s1", "a1", "counter")
	if w.monolog != nil || w.monologWindow != nil {
		t.Error("monologue should not open without a GetTranscript handler")
	}
}

// TestPinTogglesNoMonologNoPanic verifies the sidebar re-clamp paths are safe when
// no monologue is open (the monologWindow guard).
func TestPinTogglesNoMonologNoPanic(t *testing.T) {
	w := newTestWorkbench(t)
	if w.monologWindow != nil {
		t.Fatal("precondition: no monologue should be open")
	}
	w.ToggleSidebarPin()
	w.ToggleSidebarPin()
	w.setSidebarWidth(36)
	w.setSidebarWidth(28)
}
