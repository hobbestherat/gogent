package ui

import (
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	"gogent/internal/gogent"
)

// Tests for issue #314 (draggable-divider affordance + clickable pin glyph,
// keyboard-fallback relabel) and issue #320 (sidebar-resize performance:
// coalesced redraw + debounced layout persist). They drive the same code path
// the driver touched (setSidebarWidth/dragSidebarWidth in tui.go, the divider
// DrawFn/OnClickFn and the new pin-toggle child in sidebar.go) without starting
// the event loop, mirroring the existing sidebar_resize_test.go / sidebar_pin_test.go
// harness (newTestWorkbench + driving OnClickFn directly).

// stopLayoutTimer stops the coalesced layout-persist AfterFunc a test may have
// armed, so its goroutine cannot Post back after the test returns. The Post would
// be a harmless no-op (the run loop never drains it under `go test`), but stopping
// keeps the test self-contained.
func stopLayoutTimer(t *testing.T, w *Workbench) {
	t.Helper()
	t.Cleanup(func() {
		if w.layoutPersist != nil {
			w.layoutPersist.Stop()
		}
	})
}

// ---------------------------------------------------------------------------
// Part A — issue #320: performance (coalesced redraw + debounced persist)
// ---------------------------------------------------------------------------

// TestSidebarDragNoSyncPersist is the core #320 regression: a burst of divider
// motion reports must NOT write the layout file once per event. Before the fix
// setSidebarWidth called persistLayout() synchronously, so an N-cell drag flowed
// N SaveLayout invocations through the handler; after the fix it schedules a
// debounced persist, so the handler must see ZERO calls during the drag itself.
func TestSidebarDragNoSyncPersist(t *testing.T) {
	w := newTestWorkbench(t)
	stopLayoutTimer(t, w)
	var saves int
	w.handlers.SaveLayout = func(gogent.Layout) { saves++ }

	c := w.sidebar.divider
	// Simulate a multi-cell drag. Each X maps to width 80-X; pick a descending run
	// that stays inside [24,40] and differs from the default 32 so every event is a
	// real width change (setSidebarWidth early-returns on an unchanged width).
	for _, x := range []int{41, 42, 43, 44, 45, 46} { // widths 39..34
		c.OnClickFn(c, tui.ClickEvent{X: x, Y: 5, Down: true, Drag: x != 41})
	}
	// Release.
	c.OnClickFn(c, tui.ClickEvent{X: 46, Y: 5, Down: false})

	if saves != 0 {
		t.Fatalf("SaveLayout called %d times during a divider drag, want 0 (must be debounced, not synchronous per motion)", saves)
	}
	// The width still tracked the drag (last in-range width was 80-46 = 34).
	if got := w.sidebarWidth(); got != 34 {
		t.Errorf("after drag, width = %d, want 34", got)
	}
	// And a coalesced persist was armed for after the burst settles.
	if w.layoutPersist == nil {
		t.Error("expected a debounced layout-persist timer to be armed after the drag")
	}
}

// TestSidebarNudgeNoSyncPersist verifies the keyboard fallback (Widen/Narrow,
// nudgeSidebarWidth) also routes through the debounced persist under Option A — it
// must not write synchronously either, yet must arm the timer so the change is
// still saved shortly after (and unconditionally at shutdown).
func TestSidebarNudgeNoSyncPersist(t *testing.T) {
	w := newTestWorkbench(t)
	stopLayoutTimer(t, w)
	var saves int
	w.handlers.SaveLayout = func(gogent.Layout) { saves++ }

	w.nudgeSidebarWidth(+sidebarNudge)
	if saves != 0 {
		t.Errorf("nudge persisted synchronously (%d SaveLayout calls), want 0 — Option A debounces it", saves)
	}
	if w.layoutPersist == nil {
		t.Error("nudge did not arm the debounced layout-persist timer")
	}
}

// TestScheduleLayoutPersistNoHandlerNoTimer verifies scheduleLayoutPersist is a
// no-op when no SaveLayout handler is wired (the guard mirrors persistLayout). A
// width change must neither arm the timer nor panic on a handler-less workbench.
func TestScheduleLayoutPersistNoHandlerNoTimer(t *testing.T) {
	w := newTestWorkbench(t)
	stopLayoutTimer(t, w)
	if w.handlers.SaveLayout != nil {
		t.Fatal("precondition: SaveLayout should be unset on a fresh test workbench")
	}
	w.setSidebarWidth(36) // real change, but no handler
	if w.layoutPersist != nil {
		t.Error("scheduleLayoutPersist armed a timer with no SaveLayout handler; want no-op")
	}
}

// TestScheduleLayoutPersistReusesTimer verifies repeated schedules during one
// drag reuse (Reset) the single timer rather than allocating a new one per event,
// so a burst collapses to one pending write (the coalescing contract).
func TestScheduleLayoutPersistReusesTimer(t *testing.T) {
	w := newTestWorkbench(t)
	stopLayoutTimer(t, w)
	w.handlers.SaveLayout = func(gogent.Layout) {}

	w.setSidebarWidth(38)
	first := w.layoutPersist
	if first == nil {
		t.Fatal("first scheduled persist did not arm a timer")
	}
	w.setSidebarWidth(36)
	w.setSidebarWidth(34)
	if w.layoutPersist != first {
		t.Error("follow-up schedules replaced the timer instead of Reset()ing the original")
	}
}

// TestSidebarPersistOnShutdownDefer verifies the correctness guarantee that backs
// the debounce: even if the timer never fires, persistLayout() (called directly,
// as Run's deferred teardown does) writes the final width exactly once. This is
// what makes dropping the per-motion writes safe.
func TestSidebarPersistOnShutdownDefer(t *testing.T) {
	w := newTestWorkbench(t)
	stopLayoutTimer(t, w)
	var saved gogent.Layout
	var saves int
	w.handlers.SaveLayout = func(l gogent.Layout) { saved = l; saves++ }

	// Drag the divider; nothing is persisted yet (debounced).
	c := w.sidebar.divider
	c.OnClickFn(c, tui.ClickEvent{X: 44, Y: 5, Down: true}) // width 36
	if saves != 0 {
		t.Fatalf("precondition: %d synchronous saves during drag, want 0", saves)
	}

	// The shutdown defer captures it.
	w.persistLayout()
	if saves != 1 {
		t.Fatalf("shutdown persistLayout produced %d saves, want exactly 1", saves)
	}
	if saved.SidebarWidth != 36 {
		t.Errorf("persisted SidebarWidth = %d, want the final dragged width 36", saved.SidebarWidth)
	}
}

// ---------------------------------------------------------------------------
// Part B — issue #314: pin/unpin glyph
// ---------------------------------------------------------------------------

// TestPinToggleClickTogglesPin drives the new header pin glyph's OnClickFn with a
// fresh press and asserts it flips the sidebar pin state (the same path as
// View → Pin/Unpin Sidebar), and toggles back on a second click.
func TestPinToggleClickTogglesPin(t *testing.T) {
	w := newTestWorkbench(t)
	if w.sidebar.pinToggle == nil {
		t.Fatal("sidebar has no pinToggle component")
	}
	if !w.IsSidebarPinned() {
		t.Fatal("precondition: sidebar pinned by default")
	}
	c := w.sidebar.pinToggle

	c.OnClickFn(c, tui.ClickEvent{X: 0, Y: 0, Down: true})
	if w.IsSidebarPinned() {
		t.Error("clicking the pin glyph did not unpin the sidebar")
	}
	c.OnClickFn(c, tui.ClickEvent{X: 0, Y: 0, Down: true})
	if !w.IsSidebarPinned() {
		t.Error("a second click did not re-pin the sidebar")
	}
}

// TestPinToggleIgnoresDragAndRelease guards the double-toggle defence: only a
// fresh press (Down && !Drag) may toggle. The terminal keeps Down=true through
// drag-motion reports and sends Down=false on release; neither must flip the pin,
// so a click that jitters across a cell boundary toggles exactly once.
func TestPinToggleIgnoresDragAndRelease(t *testing.T) {
	w := newTestWorkbench(t)
	c := w.sidebar.pinToggle
	start := w.IsSidebarPinned() // true

	c.OnClickFn(c, tui.ClickEvent{Down: true, Drag: false}) // fresh press -> toggle
	c.OnClickFn(c, tui.ClickEvent{Down: true, Drag: true})  // jitter motion -> ignore
	c.OnClickFn(c, tui.ClickEvent{Down: true, Drag: true})  // more jitter -> ignore
	c.OnClickFn(c, tui.ClickEvent{Down: false})             // release -> ignore

	if w.IsSidebarPinned() == start {
		t.Fatal("net pin state unchanged; the fresh press should have toggled it once")
	}
	// One more fresh press returns to start: confirms a single net toggle, not three.
	c.OnClickFn(c, tui.ClickEvent{Down: true, Drag: false})
	if w.IsSidebarPinned() != start {
		t.Error("drag/release events leaked extra toggles (pin double-toggled)")
	}
}

// TestPinToggleClickReturnsHandled verifies the glyph claims the click (returns
// true) so it is not also routed to the tree behind it.
func TestPinToggleClickReturnsHandled(t *testing.T) {
	w := newTestWorkbench(t)
	c := w.sidebar.pinToggle
	if !c.OnClickFn(c, tui.ClickEvent{Down: true}) {
		t.Error("pin glyph OnClickFn returned false; click would fall through to the tree")
	}
	if !c.OnClickFn(c, tui.ClickEvent{Down: false}) {
		t.Error("pin glyph OnClickFn returned false on release")
	}
}

// TestPinToggleIsLastPanelChild verifies the glyph is the LAST child of the
// sidebar panel, so HitTestDeep (last child first) routes a click on its header
// cell to it rather than to the tree underneath.
func TestPinToggleIsLastPanelChild(t *testing.T) {
	w := newTestWorkbench(t)
	kids := w.sidebar.panel.Children()
	if len(kids) == 0 {
		t.Fatal("sidebar panel has no children")
	}
	if last := kids[len(kids)-1]; last != w.sidebar.pinToggle {
		t.Errorf("last panel child = %p, want pinToggle %p (must be last for HitTestDeep routing)", last, w.sidebar.pinToggle)
	}
}

// TestPinTogglePositionedRightEdge verifies LayoutFn parks the glyph one cell in
// from the panel's right edge on the header row, clear of the title and the
// divider column.
func TestPinTogglePositionedRightEdge(t *testing.T) {
	w := newTestWorkbench(t)
	w.desktop.Redraw() // run the panel LayoutFn so child bounds are set
	b := w.sidebar.pinToggle.Bounds
	wantX := w.sidebar.panel.Bounds.W - 2
	if b.X != wantX || b.Y != 0 || b.W != 1 || b.H != 1 {
		t.Errorf("pinToggle bounds = %+v, want {X:%d Y:0 W:1 H:1}", b, wantX)
	}
	// Absolute column is two cells in from the screen's right edge.
	abs := w.sidebar.pinToggle.AbsoluteBounds()
	if abs.X != w.app.Width()-2 {
		t.Errorf("pinToggle absolute X = %d, want %d (screen right edge - 2)", abs.X, w.app.Width()-2)
	}
}

// TestPinToggleGlyphReflectsState renders the workbench and reads the pin glyph
// cell back: ▣ (filled) when pinned, □ (outline) when unpinned, painted in the
// accent colour. This is the filled=active convention and the re-render-on-toggle
// contract (the DrawFn reads IsSidebarPinned each frame).
func TestPinToggleGlyphReflectsState(t *testing.T) {
	w := newTestWorkbench(t)

	read := func() tui.Cell {
		w.desktop.Redraw()
		abs := w.sidebar.pinToggle.AbsoluteBounds()
		return w.app.ReadCell(abs.X, abs.Y)
	}

	pinnedCell := read()
	if pinnedCell.Ch != '▣' {
		t.Errorf("pinned glyph = %q, want ▣", pinnedCell.Ch)
	}
	if pinnedCell.FG != chromeAccent {
		t.Errorf("pinned glyph FG = %v, want accent %v", pinnedCell.FG, chromeAccent)
	}

	w.ToggleSidebarPin()
	unpinnedCell := read()
	if unpinnedCell.Ch != '□' {
		t.Errorf("unpinned glyph = %q, want □", unpinnedCell.Ch)
	}
	// Must not regress to the session-favorite marker.
	if unpinnedCell.Ch == '★' || pinnedCell.Ch == '★' {
		t.Error("pin glyph uses ★, which is reserved for the session-favorite marker")
	}
}

// ---------------------------------------------------------------------------
// Part B — issue #314: divider grip affordance + drag highlight
// ---------------------------------------------------------------------------

// TestDividerDrawsGripGlyph renders the divider and checks the header row carries
// the ↔ grip in the accent colour, while the column body keeps the plain │ border
// glyph.
func TestDividerDrawsGripGlyph(t *testing.T) {
	w := newTestWorkbench(t)
	w.desktop.Redraw()
	abs := w.sidebar.divider.AbsoluteBounds()
	if abs.H < 2 {
		t.Fatalf("divider too short to test (H=%d)", abs.H)
	}

	head := w.app.ReadCell(abs.X, abs.Y)
	if head.Ch != '↔' {
		t.Errorf("divider header glyph = %q, want ↔ grip", head.Ch)
	}
	if head.FG != chromeAccent {
		t.Errorf("grip FG = %v, want accent %v", head.FG, chromeAccent)
	}

	body := w.app.ReadCell(abs.X, abs.Y+1)
	if body.Ch != '│' {
		t.Errorf("divider body glyph = %q, want plain │", body.Ch)
	}
}

// TestDividerActiveHighlightOnDrag verifies the press/release transitions in the
// divider OnClickFn toggle dividerActive, and the DrawFn brightens the column body
// to the accent colour (bold) while a drag is live, reverting after release.
func TestDividerActiveHighlightOnDrag(t *testing.T) {
	w := newTestWorkbench(t)
	c := w.sidebar.divider

	bodyCell := func() tui.Cell {
		w.desktop.Redraw()
		abs := c.AbsoluteBounds()
		return w.app.ReadCell(abs.X, abs.Y+1) // a body row, below the ↔ header
	}

	// Idle: plain border colour, not bold.
	if w.sidebar.dividerActive {
		t.Fatal("dividerActive should start false")
	}
	if idle := bodyCell(); idle.FG != chromeDivider || idle.Bold {
		t.Errorf("idle divider body = {FG:%v Bold:%v}, want {divider, false}", idle.FG, idle.Bold)
	}

	// Press (drag in progress): brightened + bold.
	c.OnClickFn(c, tui.ClickEvent{X: 44, Y: 5, Down: true}) // width 36
	if !w.sidebar.dividerActive {
		t.Fatal("press did not set dividerActive")
	}
	if live := bodyCell(); live.FG != chromeAccent || !live.Bold {
		t.Errorf("live divider body = {FG:%v Bold:%v}, want {accent, true}", live.FG, live.Bold)
	}

	// Release: back to idle styling.
	c.OnClickFn(c, tui.ClickEvent{X: 44, Y: 5, Down: false})
	if w.sidebar.dividerActive {
		t.Fatal("release did not clear dividerActive")
	}
	if back := bodyCell(); back.FG != chromeDivider || back.Bold {
		t.Errorf("post-release divider body = {FG:%v Bold:%v}, want {divider, false}", back.FG, back.Bold)
	}
}

// TestDividerActiveTracksDownFlag is a white-box check that dividerActive simply
// mirrors event.Down across the press → drag-motion → release sequence, so the
// highlight follows the whole gesture (not just the initial press).
func TestDividerActiveTracksDownFlag(t *testing.T) {
	w := newTestWorkbench(t)
	stopLayoutTimer(t, w)
	w.handlers.SaveLayout = func(gogent.Layout) {}
	c := w.sidebar.divider

	for _, step := range []struct {
		name string
		ev   tui.ClickEvent
		want bool
	}{
		{"press", tui.ClickEvent{X: 44, Down: true}, true},
		{"drag", tui.ClickEvent{X: 43, Down: true, Drag: true}, true},
		{"release", tui.ClickEvent{X: 43, Down: false}, false},
	} {
		c.OnClickFn(c, step.ev)
		if w.sidebar.dividerActive != step.want {
			t.Errorf("after %s: dividerActive = %v, want %v", step.name, w.sidebar.dividerActive, step.want)
		}
	}
}

// TestDividerDragStillResizesAfterAffordance guards that the #314 paint changes did
// not break the #175/#320 drag mechanics: a press still maps X to a new width.
func TestDividerDragStillResizesAfterAffordance(t *testing.T) {
	w := newTestWorkbench(t)
	stopLayoutTimer(t, w)
	c := w.sidebar.divider
	c.OnClickFn(c, tui.ClickEvent{X: w.app.Width() - 38, Y: 5, Down: true})
	if got := w.sidebarWidth(); got != 38 {
		t.Errorf("divider drag width = %d, want 38", got)
	}
}

// ---------------------------------------------------------------------------
// Part B — issue #314: View-menu keyboard-fallback relabel
// ---------------------------------------------------------------------------

// TestViewMenuKeyboardFallbackRelabel verifies the Widen/Narrow entries are kept
// (the keyboard fallback is NOT deleted) but relabelled and grouped as a clearly
// secondary section: each label is marked "(keyboard)" and a separator precedes
// the group.
func TestViewMenuKeyboardFallbackRelabel(t *testing.T) {
	w := newTestWorkbench(t)
	items := w.viewItems()

	idxWiden, idxNarrow := -1, -1
	for i, it := range items {
		if it == nil {
			continue
		}
		// Strip the &accelerator marker, which can sit mid-word (e.g. "Narro&w").
		label := strings.ReplaceAll(it.Label, "&", "")
		if strings.Contains(label, "Widen Sidebar") {
			idxWiden = i
			if !strings.Contains(label, "keyboard") {
				t.Errorf("Widen entry %q not marked as a keyboard fallback", it.Label)
			}
		}
		if strings.Contains(label, "Narrow Sidebar") {
			idxNarrow = i
			if !strings.Contains(label, "keyboard") {
				t.Errorf("Narrow entry %q not marked as a keyboard fallback", it.Label)
			}
		}
	}
	if idxWiden < 0 || idxNarrow < 0 {
		t.Fatal("Widen/Narrow keyboard fallback entries were deleted; they must be retained")
	}
	// A separator must set the fallback group apart from the Pin entry above it.
	if idxWiden == 0 || !items[idxWiden-1].Separator {
		t.Error("expected a separator immediately before the Widen (keyboard) fallback entry")
	}
}

// TestViewMenuWidenNarrowStillNudge verifies the relabelled fallback entries are
// still wired to nudgeSidebarWidth so the keyboard path keeps resizing.
func TestViewMenuWidenNarrowStillNudge(t *testing.T) {
	w := newTestWorkbench(t)
	stopLayoutTimer(t, w)
	start := w.sidebarWidth()

	invoke := func(needle string) {
		t.Helper()
		for _, it := range w.viewItems() {
			if it != nil && it.OnSelect != nil && strings.Contains(strings.ReplaceAll(it.Label, "&", ""), needle) {
				it.OnSelect()
				return
			}
		}
		t.Fatalf("no actionable menu item matching %q", needle)
	}

	invoke("Widen Sidebar")
	if got := w.sidebarWidth(); got != start+sidebarNudge {
		t.Errorf("Widen menu item: width = %d, want %d", got, start+sidebarNudge)
	}
	invoke("Narrow Sidebar")
	if got := w.sidebarWidth(); got != start {
		t.Errorf("Narrow menu item: width = %d, want %d", got, start)
	}
}
