package ui

// Tests for the View-menu window tiling / auto-arrange wiring (issue #241).
//
// The pure tiling geometry lives in turbotv (TileRects/TileWindows); gogent owns
// which windows to arrange (sidebar order, live + read-only), the work area, the
// maximized bookkeeping and the menu/command wiring. These tests exercise the
// gogent orchestration layer (arrange/maximizeAll) and the wiring
// (Workbench.commands + viewItems), asserting against tv.TileRects as the source
// of truth rather than re-deriving the tiling math.
//
// Runs without -race on a Pi5 (per repo convention); newTestWorkbench builds a
// real Workbench (desktop + sidebar) without starting the event loop.

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// --- rect helpers -----------------------------------------------------------

// rectWithin reports whether r lies entirely inside area (inclusive edges).
func rectWithin(r, area tv.Rect) bool {
	return r.X >= area.X && r.Y >= area.Y &&
		r.X+r.W <= area.X+area.W && r.Y+r.H <= area.Y+area.H
}

// rectsOverlap reports whether two rects share any interior cell. Touching edges
// (a partition's shared boundary) do NOT count as overlap.
func rectsOverlap(a, b tv.Rect) bool {
	return a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H
}

// stripMnemonic removes '&' menu accelerators from a label.
func stripMnemonic(s string) string {
	return strings.ReplaceAll(s, "&", "")
}

// --- workbench helpers ------------------------------------------------------

// openLive opens n live session windows named s1..sN and returns their ids in
// creation order.
func openLive(w *Workbench, n int) []string {
	ids := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		id := "s" + itoa(i)
		w.openWindow(id, id)
		ids = append(ids, id)
	}
	return ids
}

// itoa is a tiny local int->string to avoid pulling in strconv everywhere.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// orderedBounds returns the bounds of every open window in sidebar order.
func orderedBounds(w *Workbench) []tv.Rect {
	sws, _ := w.openWindows()
	out := make([]tv.Rect, len(sws))
	for i, sw := range sws {
		out[i] = sw.window.Component.Bounds
	}
	return out
}

// anyMaximized reports whether any open window still has gogent's maximized
// flag set.
func anyMaximized(w *Workbench) bool {
	sws, _ := w.openWindows()
	for _, sw := range sws {
		if sw.maximized {
			return true
		}
	}
	return false
}

// anyMinimized reports whether any open window is still toolkit-minimized.
func anyMinimized(w *Workbench) bool {
	sws, _ := w.openWindows()
	for _, sw := range sws {
		if sw.window.IsMinimized() {
			return true
		}
	}
	return false
}

// assertPartitionsArea checks every rect is inside area and no two overlap.
func assertPartitionsArea(t *testing.T, rects []tv.Rect, area tv.Rect) {
	t.Helper()
	for i, r := range rects {
		if !rectWithin(r, area) {
			t.Errorf("rect %d %+v not within area %+v", i, r, area)
		}
		for j := i + 1; j < len(rects); j++ {
			if rectsOverlap(r, rects[j]) {
				t.Errorf("rects %d %+v and %d %+v overlap", i, r, j, rects[j])
			}
		}
	}
}

// --- arrange: core layouts --------------------------------------------------

// TestArrangeTileRows verifies vertical tiling: each window is full-width, the
// bounds match turbotv's TileRects in sidebar order, they partition windowArea
// without overlap, and gogent's maximized flag is cleared.
func TestArrangeTileRows(t *testing.T) {
	w := newTestWorkbench(t)
	openLive(w, 3)

	w.arrange(tv.TileRows)

	area := w.tileArea()
	rects := orderedBounds(w)
	if len(rects) != 3 {
		t.Fatalf("got %d tiled windows, want 3", len(rects))
	}
	// Bounds are exactly the rects turbotv computes, in sidebar order.
	want, _, rows := tv.TileRects(tv.TileRows, area, 3)
	if rows != 3 {
		t.Errorf("TileRows rows = %d, want 3", rows)
	}
	for i := range rects {
		if rects[i] != want[i] {
			t.Errorf("window %d bounds = %+v, want %+v", i, rects[i], want[i])
		}
	}
	// TileRows = full width each, stacked top-to-bottom: same X/W, increasing Y.
	for i := 1; i < len(rects); i++ {
		if rects[i].X != rects[0].X || rects[i].W != rects[0].W {
			t.Errorf("row %d not full-width: X/W = %d/%d, want %d/%d",
				i, rects[i].X, rects[i].W, rects[0].X, rects[0].W)
		}
		if rects[i].Y <= rects[i-1].Y {
			t.Errorf("rows not top-to-bottom: Y[%d]=%d <= Y[%d]=%d",
				i, rects[i].Y, i-1, rects[i-1].Y)
		}
	}
	assertPartitionsArea(t, rects, w.windowArea())
	if anyMaximized(w) {
		t.Error("arrange left a window maximized")
	}
}

// TestArrangeTileColumns verifies horizontal tiling: full-height columns,
// left-to-right, matching TileRects, non-overlapping, maximized cleared.
func TestArrangeTileColumns(t *testing.T) {
	w := newTestWorkbench(t)
	openLive(w, 3)

	w.arrange(tv.TileColumns)

	area := w.tileArea()
	rects := orderedBounds(w)
	if len(rects) != 3 {
		t.Fatalf("got %d tiled windows, want 3", len(rects))
	}
	want, cols, _ := tv.TileRects(tv.TileColumns, area, 3)
	if cols != 3 {
		t.Errorf("TileColumns cols = %d, want 3", cols)
	}
	for i := range rects {
		if rects[i] != want[i] {
			t.Errorf("window %d bounds = %+v, want %+v", i, rects[i], want[i])
		}
	}
	for i := 1; i < len(rects); i++ {
		if rects[i].Y != rects[0].Y || rects[i].H != rects[0].H {
			t.Errorf("col %d not full-height: Y/H = %d/%d, want %d/%d",
				i, rects[i].Y, rects[i].H, rects[0].Y, rects[0].H)
		}
		if rects[i].X <= rects[i-1].X {
			t.Errorf("cols not left-to-right: X[%d]=%d <= X[%d]=%d",
				i, rects[i].X, i-1, rects[i-1].X)
		}
	}
	assertPartitionsArea(t, rects, w.windowArea())
	if anyMaximized(w) {
		t.Error("arrange left a window maximized")
	}
}

// TestArrangeTileGrid verifies grid tiling against TileRects, including a
// partial final row (5 windows → 3 cols × 2 rows), non-overlapping, maximized
// cleared.
func TestArrangeTileGrid(t *testing.T) {
	w := newTestWorkbench(t)
	openLive(w, 5)

	w.arrange(tv.TileGrid)

	area := w.tileArea()
	rects := orderedBounds(w)
	if len(rects) != 5 {
		t.Fatalf("got %d tiled windows, want 5", len(rects))
	}
	want, cols, rows := tv.TileRects(tv.TileGrid, area, 5)
	if cols != 3 || rows != 2 {
		t.Errorf("TileGrid dims = %dx%d, want 3x2", cols, rows)
	}
	for i := range rects {
		if rects[i] != want[i] {
			t.Errorf("window %d bounds = %+v, want %+v", i, rects[i], want[i])
		}
	}
	assertPartitionsArea(t, rects, w.windowArea())
	if anyMaximized(w) {
		t.Error("arrange left a window maximized")
	}
}

// TestArrangeMatchesTileRectsForManyCounts is a table-driven check that arrange
// produces exactly turbotv's rects across a range of window counts for all three
// layouts, so remainder distribution and grid sizing are exercised broadly.
func TestArrangeMatchesTileRectsForManyCounts(t *testing.T) {
	layouts := []tv.TileLayout{tv.TileRows, tv.TileColumns, tv.TileGrid}
	for _, layout := range layouts {
		for n := 1; n <= 9; n++ {
			w := newTestWorkbench(t)
			openLive(w, n)
			w.arrange(layout)
			area := w.tileArea()
			rects := orderedBounds(w)
			want, _, _ := tv.TileRects(layout, area, n)
			if len(rects) != n {
				t.Errorf("layout %v n=%d: got %d rects", layout, n, len(rects))
				continue
			}
			for i := range rects {
				if rects[i] != want[i] {
					t.Errorf("layout %v n=%d window %d: got %+v want %+v",
						layout, n, i, rects[i], want[i])
				}
			}
			assertPartitionsArea(t, rects, w.windowArea())
			if anyMaximized(w) {
				t.Errorf("layout %v n=%d: a window stayed maximized", layout, n)
			}
		}
	}
}

// TestArrangeTileCascade verifies the Cascade arrangement (issue #271) lays
// windows out exactly as the toolkit's TileCascade geometry, in sidebar order,
// and clears the maximized flag. Cascade overlaps by design, so it asserts the
// per-window bounds match TileRects rather than partitioning the area.
func TestArrangeTileCascade(t *testing.T) {
	for n := 1; n <= 9; n++ {
		w := newTestWorkbench(t)
		openLive(w, n)
		w.arrange(tv.TileCascade)
		area := w.tileArea()
		rects := orderedBounds(w)
		want, _, _ := tv.TileRects(tv.TileCascade, area, n)
		if len(rects) != n {
			t.Errorf("n=%d: got %d rects", n, len(rects))
			continue
		}
		for i := range rects {
			if rects[i] != want[i] {
				t.Errorf("cascade n=%d window %d: got %+v want %+v", n, i, rects[i], want[i])
			}
		}
		if anyMaximized(w) {
			t.Errorf("cascade n=%d: a window stayed maximized", n)
		}
	}
}

// --- arrange: state transitions & edge cases --------------------------------

// TestArrangeClearsMaximized verifies that arranging windows which were
// previously maximized clears gogent's maximized flag and gives them tiled
// (non-maximized) bounds.
func TestArrangeClearsMaximized(t *testing.T) {
	w := newTestWorkbench(t)
	openLive(w, 3)
	// Maximize two of them up front via the per-window path.
	w.sessions["s1"].Maximize()
	w.sessions["s3"].Maximize()
	if !w.sessions["s1"].IsMaximized() || !w.sessions["s3"].IsMaximized() {
		t.Fatal("precondition: maximize did not take effect")
	}

	w.arrange(tv.TileGrid)

	for _, id := range []string{"s1", "s2", "s3"} {
		if w.sessions[id].IsMaximized() {
			t.Errorf("%s still maximized after arrange", id)
		}
	}
	area := w.tileArea()
	for _, id := range []string{"s1", "s2", "s3"} {
		b := w.sessions[id].window.Component.Bounds
		// A tiled window in a 3-up grid is strictly smaller than the whole area
		// (cols>=2 or rows>=2), so it must not equal the maximized area.
		if b == area {
			t.Errorf("%s bounds == full area after tiling, want a tile", id)
		}
	}
}

// TestArrangeUnminimizes verifies arrange un-minimizes minimized windows
// (tv.TileWindows restores them so they participate in the layout). This is the
// behavior maximizeAll is expected to mirror (see TestMaximizeAllUnminimizes).
func TestArrangeUnminimizes(t *testing.T) {
	w := newTestWorkbench(t)
	openLive(w, 3)
	w.sessions["s2"].window.Minimize()
	if !w.sessions["s2"].window.IsMinimized() {
		t.Fatal("precondition: s2 not minimized")
	}

	w.arrange(tv.TileRows)

	if w.sessions["s2"].window.IsMinimized() {
		t.Error("s2 still minimized after arrange; tiling should restore it")
	}
	if anyMinimized(w) {
		t.Error("a window remained minimized after arrange")
	}
}

// TestArrangeSingleWindow: with one open window every layout gives it the whole
// tile area (TileRects returns area for n==1) and clears maximized.
func TestArrangeSingleWindow(t *testing.T) {
	for _, layout := range []tv.TileLayout{tv.TileRows, tv.TileColumns, tv.TileGrid} {
		w := newTestWorkbench(t)
		w.openWindow("only", "only")
		w.sessions["only"].Maximize()

		w.arrange(layout)

		got := w.sessions["only"].window.Component.Bounds
		if got != w.tileArea() {
			t.Errorf("layout %v single window bounds = %+v, want tileArea %+v",
				layout, got, w.tileArea())
		}
		if w.sessions["only"].IsMaximized() {
			t.Errorf("layout %v left the single window maximized", layout)
		}
	}
}

// TestArrangeEmptyIsNoOp verifies arrange on an empty desktop does not panic and
// changes nothing.
func TestArrangeEmptyIsNoOp(t *testing.T) {
	w := newTestWorkbench(t)
	for _, layout := range []tv.TileLayout{tv.TileRows, tv.TileColumns, tv.TileGrid} {
		// Must not panic with no windows.
		w.arrange(layout)
		if got := w.orderIDs(); len(got) != 0 {
			t.Errorf("arrange on empty desktop created sessions: %v", got)
		}
	}
}

// TestArrangeIsRerunnable verifies arranging twice yields identical bounds
// (the issue calls the action "re-runnable"): there is no drift between runs.
func TestArrangeIsRerunnable(t *testing.T) {
	w := newTestWorkbench(t)
	openLive(w, 4)

	w.arrange(tv.TileGrid)
	first := orderedBounds(w)
	w.arrange(tv.TileGrid)
	second := orderedBounds(w)

	for i := range first {
		if first[i] != second[i] {
			t.Errorf("rerun drifted: window %d %+v -> %+v", i, first[i], second[i])
		}
	}
}

// TestArrangeFollowsSidebarOrder verifies that after reordering the sidebar
// (MoveSession), arrange lays windows out in the new sidebar order: the
// leftmost column belongs to the session now first in w.order.
func TestArrangeFollowsSidebarOrder(t *testing.T) {
	w := newTestWorkbench(t)
	openLive(w, 3) // order: s1, s2, s3
	// Move s3 to the front → sidebar order s3, s1, s2.
	w.MoveSession("s3", -2)
	if got := w.orderIDs(); got[0] != "s3" {
		t.Fatalf("precondition: order = %v, want s3 first", got)
	}

	w.arrange(tv.TileColumns)

	rects := orderedBounds(w)
	// Columns are left-to-right in sidebar order, so X must strictly increase.
	for i := 1; i < len(rects); i++ {
		if rects[i].X <= rects[i-1].X {
			t.Errorf("columns not ordered left-to-right after reorder: "+
				"X[%d]=%d <= X[%d]=%d", i, rects[i].X, i-1, rects[i-1].X)
		}
	}
	// The first-in-order session (s3) holds the leftmost column.
	if w.sessions["s3"].window.Component.Bounds.X != rects[0].X {
		t.Error("s3 (first in sidebar order) is not in the leftmost column")
	}
}

// TestArrangeIncludesReadOnlyWindows verifies that read-only analysis windows
// (issue #58) are tiled alongside live sessions — openWindows must gather them
// too. Arranging all windows without covering the sidebar applies to them as
// well.
func TestArrangeIncludesReadOnlyWindows(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("live1", "live1")
	w.openWindowAny("analysis-1", "Saved A", true) // read-only
	w.openWindow("live2", "live2")

	w.arrange(tv.TileGrid)

	sws, _ := w.openWindows()
	if len(sws) != 3 {
		t.Fatalf("openWindows returned %d windows, want 3 (live+analysis+live)", len(sws))
	}
	rects := orderedBounds(w)
	assertPartitionsArea(t, rects, w.windowArea())
	// Every open window — including the analysis one — received a tile.
	if w.sessions["analysis-1"].window.Component.Bounds == (tv.Rect{}) {
		t.Error("read-only analysis window was not tiled")
	}
	if anyMaximized(w) {
		t.Error("a window stayed maximized")
	}
}

// --- maximizeAll ------------------------------------------------------------

// TestMaximizeAllExpandsEveryWindow verifies every open window is sized to the
// tile area and marked maximized.
func TestMaximizeAllExpandsEveryWindow(t *testing.T) {
	w := newTestWorkbench(t)
	openLive(w, 4)

	w.maximizeAll()

	area := w.tileArea()
	sws, _ := w.openWindows()
	for _, sw := range sws {
		if sw.window.Component.Bounds != area {
			t.Errorf("window %q bounds = %+v, want tileArea %+v",
				sw.id, sw.window.Component.Bounds, area)
		}
		if !sw.maximized {
			t.Errorf("window %q not marked maximized", sw.id)
		}
	}
}

// TestMaximizeAllRecordsRestorePoint verifies preMaximizeBounds is captured for
// windows that were not already maximized, so a later per-window restore returns
// them. Windows that were already maximized keep their existing restore point.
func TestMaximizeAllRecordsRestorePoint(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "a")
	w.openWindow("b", "b")

	// Give 'a' a known pre-maximize position, then maximize it by hand.
	restA := tv.Rect{X: 5, Y: 6, W: 20, H: 7}
	w.sessions["a"].window.Component.SetBounds(restA)
	w.sessions["a"].Maximize()
	if !w.sessions["a"].IsMaximized() {
		t.Fatal("precondition: a not maximized")
	}
	// 'b' is at some normal bounds.
	restB := w.sessions["b"].window.Component.Bounds

	w.maximizeAll()

	// 'a' was already maximized: its restore point must be untouched.
	if w.sessions["a"].preMaximizeBounds != restA {
		t.Errorf("a restore point clobbered: got %+v, want %+v",
			w.sessions["a"].preMaximizeBounds, restA)
	}
	// 'b' was newly maximized: its restore point is its prior bounds.
	if w.sessions["b"].preMaximizeBounds != restB {
		t.Errorf("b restore point = %+v, want %+v",
			w.sessions["b"].preMaximizeBounds, restB)
	}
	// And restoring 'a' actually returns it to restA.
	w.sessions["a"].unmaximize()
	if w.sessions["a"].window.Component.Bounds != restA {
		t.Errorf("a restore landed at %+v, want %+v",
			w.sessions["a"].window.Component.Bounds, restA)
	}
}

// TestMaximizeAllEmptyIsNoOp verifies maximizeAll on an empty desktop is safe.
func TestMaximizeAllEmptyIsNoOp(t *testing.T) {
	w := newTestWorkbench(t)
	w.maximizeAll() // must not panic
	if got := w.orderIDs(); len(got) != 0 {
		t.Errorf("maximizeAll on empty desktop created sessions: %v", got)
	}
}

// TestMaximizeAllThenArrange verifies the two actions compose: after maximizing
// all, a subsequent tile clears maximized and partitions the area again.
func TestMaximizeAllThenArrange(t *testing.T) {
	w := newTestWorkbench(t)
	openLive(w, 3)

	w.maximizeAll()
	if !w.sessions["s1"].IsMaximized() {
		t.Fatal("precondition: maximizeAll did not maximize s1")
	}

	w.arrange(tv.TileRows)

	if anyMaximized(w) {
		t.Error("arrange after maximizeAll left a window maximized")
	}
	rects := orderedBounds(w)
	assertPartitionsArea(t, rects, w.windowArea())
	// No window still fills the whole area.
	for i, r := range rects {
		if r == w.tileArea() {
			t.Errorf("window %d still fills the whole area after tiling", i)
		}
	}
}

// TestArrangeThenMaximizeAll verifies the reverse composition.
func TestArrangeThenMaximizeAll(t *testing.T) {
	w := newTestWorkbench(t)
	openLive(w, 4)

	w.arrange(tv.TileGrid)
	w.maximizeAll()

	area := w.tileArea()
	sws, _ := w.openWindows()
	for _, sw := range sws {
		if sw.window.Component.Bounds != area {
			t.Errorf("window %q = %+v, want tileArea after maximizeAll", sw.id, sw.window.Component.Bounds)
		}
		if !sw.maximized {
			t.Errorf("window %q not maximized after maximizeAll", sw.id)
		}
	}
}

// TestMaximizeAllUnminimizes is a regression guard: "Maximize All" must expand
// minimized windows too — the bulk counterpart of tiling, which restores them. A
// minimized turbotv window renders only its title bar regardless of its bounds,
// so an earlier revision that set bounds without un-minimizing silently skipped
// such windows. This mirrors arrange()'s un-minimize behavior
// (see TestArrangeUnminimizes).
func TestMaximizeAllUnminimizes(t *testing.T) {
	w := newTestWorkbench(t)
	openLive(w, 3)
	w.sessions["s2"].window.Minimize()
	if !w.sessions["s2"].window.IsMinimized() {
		t.Fatal("precondition: s2 not minimized")
	}

	w.maximizeAll()

	if w.sessions["s2"].window.IsMinimized() {
		t.Error("s2 still minimized after Maximize All; the action should " +
			"expand every window, including minimized ones (as arrange does)")
	}
	if anyMinimized(w) {
		t.Error("a window remained minimized after Maximize All")
	}
}

// TestMaximizeAllMinimizedRestoresRealBounds is a regression guard for the
// restore-point corruption that accompanied the minimize bug. Minimizing a
// window collapses Component.Bounds to a 1-row title bar (turbotv/window.go),
// so capturing preMaximizeBounds from a still-minimized window remembered the
// collapsed rect — a later unmaximize restored the window to a 1-row sliver.
// After the fix, maximizeAll restores the window first, so the captured
// preMaximizeBounds is the real pre-collapse size and unmaximize round-trips.
func TestMaximizeAllMinimizedRestoresRealBounds(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "a")
	orig := w.sessions["a"].window.Component.Bounds
	if orig.H <= 1 {
		t.Fatalf("precondition: window opened with degenerate height %d", orig.H)
	}
	w.sessions["a"].window.Minimize()
	// Sanity: minimizing collapses the bounds to a 1-row bar.
	if got := w.sessions["a"].window.Component.Bounds; got.H != 1 {
		t.Fatalf("precondition: minimized height = %d, want 1", got.H)
	}

	w.maximizeAll()

	sw := w.sessions["a"]
	if sw.IsMaximized() {
		// expected: marked maximized and sized to the area
		if sw.window.Component.Bounds != w.tileArea() {
			t.Errorf("minimized-then-maximized bounds = %+v, want tileArea", sw.window.Component.Bounds)
		}
	} else {
		t.Error("window not marked maximized after Maximize All")
	}
	// The restore point must be the real pre-collapse size, not the 1-row bar.
	if sw.preMaximizeBounds.H == 1 {
		t.Errorf("preMaximizeBounds captured collapsed bar (H=1); "+
			"want real pre-minimize size: %+v", sw.preMaximizeBounds)
	}
	if sw.preMaximizeBounds != orig {
		t.Errorf("preMaximizeBounds = %+v, want original bounds %+v",
			sw.preMaximizeBounds, orig)
	}
	// And unmaximize actually returns it to those real bounds.
	sw.unmaximize()
	if got := sw.window.Component.Bounds; got != orig {
		t.Errorf("after unmaximize bounds = %+v, want original %+v", got, orig)
	}
}

// --- command palette / cheatsheet wiring (Workbench.commands) ---------------

// findCommandByKeys returns the first command whose keys field matches, or nil.
func findCommandByKeys(cmds []command, keys string) *command {
	for i := range cmds {
		if cmds[i].keys == keys {
			return &cmds[i]
		}
	}
	return nil
}

// TestCommandsRegisterArrangement verifies the four arrangement actions are in
// the central command table under a "Window" category with the Ctrl+Shift+V/H/
// G/M accelerators, so the command palette and the '?' cheatsheet pick them up.
func TestCommandsRegisterArrangement(t *testing.T) {
	cmds := (&Workbench{}).commands()

	want := []struct {
		keys, name, category string
	}{
		{"Ctrl+Shift+V", "Tile vertically", "Window"},
		{"Ctrl+Shift+H", "Tile horizontally", "Window"},
		{"Ctrl+Shift+G", "Tile grid", "Window"},
		{"Ctrl+Shift+M", "Maximize all windows", "Window"},
	}
	for _, wnt := range want {
		cmd := findCommandByKeys(cmds, wnt.keys)
		if cmd == nil {
			t.Errorf("no command bound to %q", wnt.keys)
			continue
		}
		if cmd.category != wnt.category {
			t.Errorf("%q category = %q, want %q", wnt.keys, cmd.category, wnt.category)
		}
		if cmd.name != wnt.name {
			t.Errorf("%q name = %q, want %q", wnt.keys, cmd.name, wnt.name)
		}
		if cmd.run == nil {
			t.Errorf("%q has no run handler", wnt.keys)
		}
	}
}

// TestCommandsArrangementKeysAreUnique verifies no other command accidentally
// reuses the Ctrl+Shift+V/H/G/M chords (a clash would make a binding unreachable).
func TestCommandsArrangementKeysAreUnique(t *testing.T) {
	cmds := (&Workbench{}).commands()
	chords := map[string]int{
		"Ctrl+Shift+V": 0, "Ctrl+Shift+H": 0, "Ctrl+Shift+G": 0, "Ctrl+Shift+M": 0,
	}
	for _, c := range cmds {
		if _, ok := chords[c.keys]; ok {
			chords[c.keys]++
		}
	}
	for chord, n := range chords {
		if n != 1 {
			t.Errorf("chord %q bound to %d commands, want exactly 1", chord, n)
		}
	}
}

// TestCommandsArrangementRunInvokesActions verifies the command-table closures
// are actually wired to arrange/maximizeAll (not just labelled): invoking the
// bound run handler produces the expected effect.
func TestCommandsArrangementRunInvokesActions(t *testing.T) {
	// Tile via the command bound to Ctrl+Shift+G.
	wTile := newTestWorkbench(t)
	openLive(wTile, 5)
	cmdsTile := wTile.commands()
	if cmd := findCommandByKeys(cmdsTile, "Ctrl+Shift+G"); cmd == nil || cmd.run == nil {
		t.Fatal("Tile grid command missing or has no run")
	} else {
		cmd.run()
	}
	want, _, _ := tv.TileRects(tv.TileGrid, wTile.tileArea(), 5)
	got := orderedBounds(wTile)
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Ctrl+Shift+G run: window %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Maximize via the command bound to Ctrl+Shift+M.
	wMax := newTestWorkbench(t)
	openLive(wMax, 3)
	cmdsMax := wMax.commands()
	if cmd := findCommandByKeys(cmdsMax, "Ctrl+Shift+M"); cmd == nil || cmd.run == nil {
		t.Fatal("Maximize all command missing or has no run")
	} else {
		cmd.run()
	}
	area := wMax.tileArea()
	for _, id := range []string{"s1", "s2", "s3"} {
		if b := wMax.sessions[id].window.Component.Bounds; b != area {
			t.Errorf("Ctrl+Shift+M run: %s = %+v, want %+v", id, b, area)
		}
	}
}

// --- View-menu wiring (viewItems) -------------------------------------------

// findMenuItemByLabel returns the first non-separator menu item whose label
// (mnemonic-stripped, case-insensitive) contains substr, or nil.
func findMenuItemByLabel(items []*tv.MenuItem, substr string) *tv.MenuItem {
	needle := strings.ToLower(stripMnemonic(substr))
	for _, it := range items {
		if it.Separator {
			continue
		}
		if strings.Contains(strings.ToLower(stripMnemonic(it.Label)), needle) {
			return it
		}
	}
	return nil
}

// TestViewMenuHasArrangementItems verifies the View menu exposes the four
// arrangement actions with the correct Ctrl+Shift accelerator bindings.
func TestViewMenuHasArrangementItems(t *testing.T) {
	w := newTestWorkbench(t)
	items := w.viewItems()

	want := []struct {
		label, display, rune string
	}{
		{"Tile Vertically", "Ctrl+Shift+V", "v"},
		{"Tile Horizontally", "Ctrl+Shift+H", "h"},
		{"Tile Grid", "Ctrl+Shift+G", "g"},
		{"Maximize All", "Ctrl+Shift+M", "m"},
	}
	for _, wnt := range want {
		it := findMenuItemByLabel(items, wnt.label)
		if it == nil {
			t.Errorf("View menu missing %q item", wnt.label)
			continue
		}
		if it.OnSelect == nil {
			t.Errorf("%q menu item has no OnSelect", wnt.label)
		}
		if it.Shortcut == nil {
			t.Errorf("%q menu item has no shortcut", wnt.label)
			continue
		}
		if it.Shortcut.Display != wnt.display {
			t.Errorf("%q shortcut display = %q, want %q",
				wnt.label, it.Shortcut.Display, wnt.display)
		}
		if strings.ToLower(string(it.Shortcut.Rune)) != wnt.rune {
			t.Errorf("%q shortcut rune = %q, want %q",
				wnt.label, string(it.Shortcut.Rune), wnt.rune)
		}
		if !it.Shortcut.Ctrl || !it.Shortcut.Shift {
			t.Errorf("%q shortcut must be Ctrl+Shift (Ctrl=%v Shift=%v Alt=%v)",
				wnt.label, it.Shortcut.Ctrl, it.Shortcut.Shift, it.Shortcut.Alt)
		}
		if it.Shortcut.Alt {
			t.Errorf("%q shortcut must not include Alt", wnt.label)
		}
	}
}

// TestViewMenuOnSelectTiles verifies a View-menu item's OnSelect really performs
// the arrangement (the menu wiring is live, not just labelled).
func TestViewMenuOnSelectTiles(t *testing.T) {
	w := newTestWorkbench(t)
	openLive(w, 4)

	it := findMenuItemByLabel(w.viewItems(), "Tile Grid")
	if it == nil || it.OnSelect == nil {
		t.Fatal("Tile Grid menu item missing or has no OnSelect")
	}
	it.OnSelect()

	want, _, _ := tv.TileRects(tv.TileGrid, w.tileArea(), 4)
	got := orderedBounds(w)
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("menu OnSelect window %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if anyMaximized(w) {
		t.Error("menu OnSelect left a window maximized")
	}
}

// TestViewMenuOnSelectMaximizes verifies the Maximize All menu item's OnSelect.
func TestViewMenuOnSelectMaximizes(t *testing.T) {
	w := newTestWorkbench(t)
	openLive(w, 3)

	it := findMenuItemByLabel(w.viewItems(), "Maximize All")
	if it == nil || it.OnSelect == nil {
		t.Fatal("Maximize All menu item missing or has no OnSelect")
	}
	it.OnSelect()

	area := w.tileArea()
	for _, id := range []string{"s1", "s2", "s3"} {
		if b := w.sessions[id].window.Component.Bounds; b != area {
			t.Errorf("%s = %+v after Maximize All OnSelect, want %+v", id, b, area)
		}
		if !w.sessions[id].IsMaximized() {
			t.Errorf("%s not maximized after Maximize All OnSelect", id)
		}
	}
}
