package ui

import (
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// TestMaximizedWindowRect covers the maximize target geometry: it fills the given
// available width (the workbench's window area — already reduced by a pinned
// sidebar) below the menu bar, and floors the dimensions at 1 so the rect is never
// empty (issues #105 / #106).
func TestMaximizedWindowRect(t *testing.T) {
	for _, tc := range []struct {
		name            string
		availW, screenH int
		want            tv.Rect
	}{
		{"fills available width below menu bar", 48, 25, tv.Rect{X: 0, Y: 1, W: 48, H: 24}},
		{"narrow available width", 8, 25, tv.Rect{X: 0, Y: 1, W: 8, H: 24}},
		{"full desktop width when sidebar unpinned", 80, 25, tv.Rect{X: 0, Y: 1, W: 80, H: 24}},
		{"one-row desktop keeps a 1-row window", 48, 1, tv.Rect{X: 0, Y: 1, W: 48, H: 1}},
		{"zero-height desktop floors height at 1", 48, 0, tv.Rect{X: 0, Y: 1, W: 48, H: 1}},
		{"zero-width desktop floors width at 1", 0, 25, tv.Rect{X: 0, Y: 1, W: 1, H: 24}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := maximizedWindowRect(tc.availW, tc.screenH); got != tc.want {
				t.Errorf("maximizedWindowRect(%d, %d) = %+v, want %+v", tc.availW, tc.screenH, got, tc.want)
			}
		})
	}
}

// TestMaximizeButtonRect covers the title-bar button placement for every
// close/minimize combination: maximize sits one slot left of the rightmost
// button, never overlapping close or minimize, and tracks the window's origin.
func TestMaximizeButtonRect(t *testing.T) {
	abs := tv.Rect{X: 0, Y: 0, W: 60, H: 20} // Right() == 59
	for _, tc := range []struct {
		name                string
		showClose, minimize bool
		want                tv.Rect
	}{
		{"left of close+minimize", true, true, tv.Rect{X: 46, Y: 0, W: 3, H: 1}},
		{"left of close only", true, false, tv.Rect{X: 50, Y: 0, W: 3, H: 1}},
		{"left of minimize only", false, true, tv.Rect{X: 50, Y: 0, W: 3, H: 1}},
		{"rightmost when alone", false, false, tv.Rect{X: 54, Y: 0, W: 3, H: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := maximizeButtonRect(abs, tc.showClose, tc.minimize)
			if got != tc.want {
				t.Fatalf("maximizeButtonRect(%+v,%v,%v) = %+v, want %+v",
					abs, tc.showClose, tc.minimize, got, tc.want)
			}
			// The button never overlaps the close or minimize glyphs to its right.
			closeR := tv.Rect{X: abs.Right() - 5, Y: abs.Y, W: 3, H: 1}
			if tc.showClose && got.Right() >= closeR.X {
				t.Errorf("maximize %v overlaps close %v", got, closeR)
			}
			if tc.showClose && tc.minimize {
				minR := tv.Rect{X: abs.Right() - 9, Y: abs.Y, W: 3, H: 1}
				if got.Right() >= minR.X {
					t.Errorf("maximize %v overlaps minimize %v", got, minR)
				}
			}
		})
	}
	// The button tracks the window origin (nonzero X/Y).
	off := tv.Rect{X: 5, Y: 3, W: 60, H: 20} // Right() == 64
	if got := maximizeButtonRect(off, true, true); got != (tv.Rect{X: 51, Y: 3, W: 3, H: 1}) {
		t.Errorf("maximizeButtonRect with origin %+v = %+v, want {51,3,3,1}", off, got)
	}
}

// TestSessionWindowMaximizeToggle verifies maximize expands to the available area
// and a second toggle restores the exact prior bounds (issue #105).
func TestSessionWindowMaximizeToggle(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	sw := w.sessions["a"]
	original := tv.Rect{X: 2, Y: 2, W: 50, H: 14}
	sw.window.Component.SetBounds(original)

	sw.Maximize()
	if !sw.IsMaximized() {
		t.Fatal("expected window maximized after Maximize()")
	}
	want := maximizedWindowRect(w.windowArea().W, w.windowArea().H)
	if got := sw.window.Component.Bounds; got != want {
		t.Errorf("maximized bounds = %+v, want %+v", got, want)
	}
	// A second maximize is a no-op (stays maximized, pre-maximize bounds intact).
	sw.Maximize()
	if !sw.IsMaximized() {
		t.Error("second Maximize() un-maximized the window")
	}

	// Toggle restores the exact pre-maximize bounds.
	sw.ToggleMaximize()
	if sw.IsMaximized() {
		t.Error("expected window restored after toggling back")
	}
	if got := sw.window.Component.Bounds; got != original {
		t.Errorf("restored bounds = %+v, want original %+v", got, original)
	}
}

// TestMaximizeLeavesSidebarUncovered verifies a maximized window never covers the
// reserved "Sessions & Agents" sidebar or the menu bar (issue #105).
func TestMaximizeLeavesSidebarUncovered(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	sw := w.sessions["a"]
	sw.Maximize()
	b := sw.window.Component.Bounds
	screenW := w.app.Width()

	if b.X+b.W > screenW-defaultSidebarWidth {
		t.Errorf("maximized window cols [%d,%d) overlap sidebar at col %d",
			b.X, b.X+b.W, screenW-defaultSidebarWidth)
	}
	if b.Y < menuBarHeight {
		t.Errorf("maximized window starts at Y=%d, would cover the menu bar", b.Y)
	}
	// Sanity: on the 80x25 test desktop the window fills the work area.
	if b != (tv.Rect{X: 0, Y: 1, W: 48, H: 24}) {
		t.Errorf("maximized bounds = %+v, want {0,1,48,24}", b)
	}
}

// TestMaximizeButtonClickToggles drives the title-bar click path: a press on the
// button toggles maximize, then toggles it back (recomputing the button each
// press, since maximize moves the window).
func TestMaximizeButtonClickToggles(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	sw := w.sessions["a"]
	sw.window.Component.SetBounds(tv.Rect{X: 2, Y: 2, W: 60, H: 20})

	clickButton := func() bool {
		abs := sw.window.Component.AbsoluteBounds()
		btn := maximizeButtonRect(abs, sw.window.ShowClose, sw.window.Minimizable)
		return sw.handleMaximizeClick(tui.ClickEvent{X: btn.X, Y: abs.Y, Down: true})
	}

	if !clickButton() {
		t.Fatal("expected press on maximize button to be claimed")
	}
	if !sw.IsMaximized() {
		t.Error("expected maximized after pressing the maximize button")
	}
	if !clickButton() {
		t.Error("expected second press to be claimed")
	}
	if sw.IsMaximized() {
		t.Error("expected restored after second press")
	}
}

// TestMaximizeButtonHitRangeAndFallthrough verifies every glyph cell is a hit
// (on a fresh window per cell so a prior toggle does not move the button) and that
// neighbouring title-bar cells, releases and off-row presses fall through.
func TestMaximizeButtonHitRangeAndFallthrough(t *testing.T) {
	for _, dx := range []int{0, 1, 2} {
		w := newTestWorkbench(t)
		w.openWindow("b", "B")
		sw := w.sessions["b"]
		sw.window.Component.SetBounds(tv.Rect{X: 2, Y: 2, W: 60, H: 20})
		abs := sw.window.Component.AbsoluteBounds()
		btn := maximizeButtonRect(abs, sw.window.ShowClose, sw.window.Minimizable)
		if !sw.handleMaximizeClick(tui.ClickEvent{X: btn.X + dx, Y: abs.Y, Down: true}) {
			t.Errorf("glyph cell +%d not claimed", dx)
		}
	}

	w := newTestWorkbench(t)
	w.openWindow("c", "C")
	sw := w.sessions["c"]
	sw.window.Component.SetBounds(tv.Rect{X: 2, Y: 2, W: 60, H: 20})
	abs := sw.window.Component.AbsoluteBounds()
	btn := maximizeButtonRect(abs, sw.window.ShowClose, sw.window.Minimizable)
	// A press on the title-bar just left of the button falls through (drag).
	if sw.handleMaximizeClick(tui.ClickEvent{X: btn.X - 1, Y: abs.Y, Down: true}) {
		t.Error("a non-button title-bar press should not be claimed by maximize")
	}
	// A release is never claimed.
	if sw.handleMaximizeClick(tui.ClickEvent{X: btn.X, Y: abs.Y, Down: false}) {
		t.Error("a release should not be claimed by maximize")
	}
	// A press off the title-bar row is not claimed.
	if sw.handleMaximizeClick(tui.ClickEvent{X: btn.X, Y: abs.Y + 1, Down: true}) {
		t.Error("a press below the title bar should not be claimed by maximize")
	}
}

// TestMaximizeDisabled verifies that when WindowConfig.Maximizable is false the
// window has no maximize affordance: the flag is off and Maximize is a no-op.
func TestMaximizeDisabled(t *testing.T) {
	w := newTestWorkbench(t)
	w.windowConfig.Maximizable = false
	w.openWindow("a", "A")
	sw := w.sessions["a"]

	if sw.maximizable {
		t.Fatal("expected maximize to be disabled for this window")
	}
	original := sw.window.Component.Bounds
	sw.Maximize()
	if sw.IsMaximized() {
		t.Error("Maximize() should be a no-op when maximize is disabled")
	}
	if sw.window.Component.Bounds != original {
		t.Errorf("bounds changed despite disabled maximize: %+v -> %+v",
			original, sw.window.Component.Bounds)
	}
	// A click on where the button would be is not claimed.
	abs := sw.window.Component.AbsoluteBounds()
	btn := maximizeButtonRect(abs, sw.window.ShowClose, sw.window.Minimizable)
	if sw.handleMaximizeClick(tui.ClickEvent{X: btn.X, Y: abs.Y, Down: true}) {
		t.Error("maximize-disabled window should not claim title-bar clicks")
	}
}
