package ui

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/config"
	"gogent/internal/gogent"
)

// newTestWorkbench builds a real Workbench (desktop + sidebar) without starting
// the event loop. tui.New() falls back to an 80x25 buffer off a TTY, and App.Apply
// is a no-op until Run(), so this is safe and silent under `go test`.
func newTestWorkbench(t *testing.T) *Workbench {
	t.Helper()
	return NewWorkbench([]*config.ModelConfig{{Name: "test", Model: "test"}})
}

// orderIDs snapshots the session sidebar order under the workbench lock.
func (w *Workbench) orderIDs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.order...)
}

// sidebarIDs returns the session ids of the sidebar tree roots in display order.
func sidebarIDs(s *sidebar) []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.tree.Roots))
	for _, n := range s.tree.Roots {
		if ref, ok := n.Data.(nodeRef); ok {
			out = append(out, ref.sessionID)
		}
	}
	return out
}

func equalOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSessionOrder verifies windows register in creation order and the active
// (top-most) session is the most recently opened one.
func TestSessionOrder(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B")
	w.openWindow("c", "C")

	if order := w.orderIDs(); !equalOrder(order, []string{"a", "b", "c"}) {
		t.Fatalf("order = %v, want [a b c]", order)
	}
	if got := w.ActiveID(); got != "c" {
		t.Fatalf("ActiveID = %q, want c (last opened)", got)
	}
	// Sidebar mirrors the workbench order.
	if got := sidebarIDs(w.sidebar); !equalOrder(got, []string{"a", "b", "c"}) {
		t.Fatalf("sidebar order = %v, want [a b c]", got)
	}
}

// TestRenameSession verifies a rename updates the window title and sidebar label
// and is captured into the persisted layout (title is decoupled from the id).
func TestRenameSession(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")

	w.SetSessionTitle("a", "My Refactor")

	sw := w.sessions["a"]
	if sw.title != "My Refactor" || sw.window.Title != "My Refactor" {
		t.Errorf("title not applied: session=%q window=%q", sw.title, sw.window.Title)
	}
	if !strings.Contains(w.sidebar.sessions["a"].Label, "My Refactor") {
		t.Errorf("sidebar label not updated: %q", w.sidebar.sessions["a"].Label)
	}
	captured := w.captureLayout()
	if e := captured.Entry("a"); e == nil || e.Title != "My Refactor" {
		t.Errorf("captured title = %+v, want My Refactor", e)
	}
	// Empty/whitespace titles are ignored (can't wipe a title).
	w.SetSessionTitle("a", "   ")
	if w.sessions["a"].title != "My Refactor" {
		t.Errorf("blank rename wiped title: %q", w.sessions["a"].title)
	}
}

// TestTogglePin verifies pinning marks the session, floats it to the top of the
// sidebar, and unpinning clears the mark while leaving it in place.
func TestTogglePin(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B")
	w.openWindow("c", "C")

	w.TogglePin("b")

	if !w.pinned["b"] {
		t.Fatal("b not marked pinned")
	}
	if order := w.orderIDs(); !equalOrder(order, []string{"b", "a", "c"}) {
		t.Fatalf("order after pin = %v, want [b a c]", order)
	}
	if got := sidebarIDs(w.sidebar); !equalOrder(got, []string{"b", "a", "c"}) {
		t.Fatalf("sidebar order after pin = %v, want [b b c]", got)
	}
	if !strings.Contains(w.sidebar.sessions["b"].Label, "★") {
		t.Errorf("pinned session missing ★ marker: %q", w.sidebar.sessions["b"].Label)
	}
	if strings.Contains(w.sidebar.sessions["a"].Label, "★") {
		t.Errorf("unpinned session has ★ marker: %q", w.sidebar.sessions["a"].Label)
	}
	if e := w.captureLayout().Entry("b"); e == nil || !e.Pinned {
		t.Errorf("captured pin state wrong: %+v", e)
	}

	// Unpin clears the marker but leaves the session where it is.
	w.TogglePin("b")
	if w.pinned["b"] {
		t.Fatal("b still pinned after unpin")
	}
	if order := w.orderIDs(); !equalOrder(order, []string{"b", "a", "c"}) {
		t.Fatalf("order changed on unpin = %v, want [b a c]", order)
	}
	if strings.Contains(w.sidebar.sessions["b"].Label, "★") {
		t.Errorf("unpinned session still has ★ marker: %q", w.sidebar.sessions["b"].Label)
	}
}

// TestMoveSessionClamping exercises reordering and the list-bound clamping.
func TestMoveSessionClamping(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		delta int
		want  []string
	}{
		{"move down", "a", 1, []string{"b", "a", "c"}},
		{"move up", "c", -1, []string{"a", "c", "b"}},
		{"clamp at top", "a", -1, []string{"a", "b", "c"}},
		{"clamp at bottom", "c", 1, []string{"a", "b", "c"}},
		{"zero delta is a no-op", "b", 0, []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			w.openWindow("a", "A")
			w.openWindow("b", "B")
			w.openWindow("c", "C")

			w.MoveSession(tc.id, tc.delta)
			if order := w.orderIDs(); !equalOrder(order, tc.want) {
				t.Fatalf("order = %v, want %v", order, tc.want)
			}
			if got := sidebarIDs(w.sidebar); !equalOrder(got, tc.want) {
				t.Fatalf("sidebar order = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMoveSessionUnknown verifies moving an unknown id is a no-op.
func TestMoveSessionUnknown(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.MoveSession("nope", 1)
	if order := w.orderIDs(); !equalOrder(order, []string{"a"}) {
		t.Fatalf("order = %v, want [a]", order)
	}
}

// TestCloseOthers verifies only the kept session remains.
func TestCloseOthers(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B")
	w.openWindow("c", "C")

	w.CloseOthers("b")
	if order := w.orderIDs(); !equalOrder(order, []string{"b"}) {
		t.Fatalf("order = %v, want [b]", order)
	}
	if w.sessions["a"] != nil || w.sessions["c"] != nil {
		t.Fatal("other sessions not removed")
	}
	if got := sidebarIDs(w.sidebar); !equalOrder(got, []string{"b"}) {
		t.Fatalf("sidebar = %v, want [b]", got)
	}
}

// TestCloseAll verifies every window closes and a single fresh one reopens.
func TestCloseAll(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B")

	w.CloseAll()
	order := w.orderIDs()
	if len(order) != 1 {
		t.Fatalf("expected exactly 1 session after CloseAll, got %v", order)
	}
	if order[0] == "a" || order[0] == "b" {
		t.Fatalf("CloseAll should open a fresh session, got %q", order[0])
	}
}

// TestLayoutCaptureApplyRoundTrip verifies captureLayout and applyLayout are
// inverses: a title, pin state, window bounds and minimized flag set on one
// desktop reappear on a fresh desktop after applying the captured layout.
func TestLayoutCaptureApplyRoundTrip(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B")

	w.SetSessionTitle("a", "Renamed A")
	w.TogglePin("b")
	rect := tv.Rect{X: 3, Y: 4, W: 55, H: 13}
	w.sessions["a"].window.Component.SetBounds(rect)
	w.sessions["b"].window.Minimize()

	captured := w.captureLayout()
	if len(captured.Entries) != 2 {
		t.Fatalf("captureLayout: expected 2 entries, got %d", len(captured.Entries))
	}
	if e := captured.Entry("a"); e == nil || e.Title != "Renamed A" {
		t.Errorf("captured a = %+v, want title Renamed A", e)
	}
	if e := captured.Entry("b"); e == nil || !e.Pinned || !e.Minimized {
		t.Errorf("captured b = %+v, want pinned+minimized", e)
	}

	// Fresh desktop with default windows, then re-apply the captured layout.
	w2 := newTestWorkbench(t)
	w2.openWindow("a", "A")
	w2.openWindow("b", "B")
	if w2.sessions["a"].title != "A" {
		t.Fatal("precondition: fresh window should have default title")
	}
	w2.applyLayout(captured)

	a := w2.sessions["a"]
	if a.title != "Renamed A" || a.window.Title != "Renamed A" {
		t.Errorf("title not restored: session=%q window=%q", a.title, a.window.Title)
	}
	if !w2.pinned["b"] {
		t.Error("pin state not restored")
	}
	wantRect := clampWindowRect(rect, w2.app.Width(), w2.app.Height(), a.window.MinWidth, a.window.MinHeight)
	if got := a.window.Component.Bounds; got != wantRect {
		t.Errorf("bounds = %+v, want %+v", got, wantRect)
	}
	if !w2.sessions["b"].window.IsMinimized() {
		t.Error("minimized state not restored")
	}
}

// TestApplyLayoutIgnoresUnknown verifies entries for sessions that no longer
// exist are skipped (the layout is self-healing on the next save).
func TestApplyLayoutIgnoresUnknown(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.applyLayout(gogent.Layout{Entries: []gogent.LayoutEntry{
		{ID: "ghost", Title: "Boo"},
		{ID: "a", Title: "Real"},
	}})
	if w.sessions["a"].title != "Real" {
		t.Errorf("known session not updated: %q", w.sessions["a"].title)
	}
}

// TestOrderByLayout verifies restored sessions are re-sequenced to match the
// persisted layout, with unknown sessions appended in their original order.
func TestOrderByLayout(t *testing.T) {
	restored := []RestoredSession{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	cases := []struct {
		name   string
		layout gogent.Layout
		want   []string
	}{
		{
			"layout reorders, unknowns appended",
			gogent.Layout{Entries: []gogent.LayoutEntry{{ID: "c"}, {ID: "a"}}},
			[]string{"c", "a", "b"},
		},
		{"empty layout keeps order", gogent.Layout{}, []string{"a", "b", "c"}},
		{"layout with only unknown ids keeps order",
			gogent.Layout{Entries: []gogent.LayoutEntry{{ID: "x"}, {ID: "y"}}}, []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orderByLayout(restored, tc.layout)
			ids := make([]string, len(got))
			for i, rs := range got {
				ids[i] = rs.ID
			}
			if !equalOrder(ids, tc.want) {
				t.Fatalf("orderByLayout = %v, want %v", ids, tc.want)
			}
		})
	}
	// A single restored session is returned as-is.
	if got := orderByLayout([]RestoredSession{{ID: "solo"}}, gogent.Layout{Entries: []gogent.LayoutEntry{{ID: "solo"}}}); len(got) != 1 || got[0].ID != "solo" {
		t.Fatalf("single-session case failed: %+v", got)
	}
}

// TestClampWindowRect exercises the on-screen / minimum-size clamping so a
// layout saved on a larger terminal can't strand a window off-screen.
func TestClampWindowRect(t *testing.T) {
	cases := []struct {
		name             string
		in               tv.Rect
		screenW, screenH int
		minW, minH       int
		want             tv.Rect
	}{
		{"in bounds unchanged", tv.Rect{X: 2, Y: 2, W: 50, H: 12}, 80, 25, 50, 12, tv.Rect{X: 2, Y: 2, W: 50, H: 12}},
		{"negative origin clamped to 0", tv.Rect{X: -5, Y: -3, W: 50, H: 12}, 80, 25, 50, 12, tv.Rect{X: 0, Y: 0, W: 50, H: 12}},
		{"too wide clamped to screen", tv.Rect{X: 0, Y: 0, W: 200, H: 12}, 80, 25, 50, 12, tv.Rect{X: 0, Y: 0, W: 80, H: 12}},
		{"below min bumped up", tv.Rect{X: 0, Y: 0, W: 5, H: 2}, 80, 25, 50, 12, tv.Rect{X: 0, Y: 0, W: 50, H: 12}},
		{"off bottom-right shifted on-screen", tv.Rect{X: 70, Y: 20, W: 50, H: 12}, 80, 25, 50, 12, tv.Rect{X: 30, Y: 13, W: 50, H: 12}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampWindowRect(tc.in, tc.screenW, tc.screenH, tc.minW, tc.minH)
			if got != tc.want {
				t.Fatalf("clampWindowRect(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestPersistLayoutHandler verifies structural changes flow through the
// SaveLayout handler so they survive a restart.
func TestPersistLayoutHandler(t *testing.T) {
	w := newTestWorkbench(t)
	var saved gogent.Layout
	w.handlers.SaveLayout = func(l gogent.Layout) { saved = l }

	w.openWindow("a", "A")
	w.SetSessionTitle("a", "Pinned Refactor")
	w.TogglePin("a")

	e := saved.Entry("a")
	if e == nil {
		t.Fatal("SaveLayout handler never received session a")
	}
	if e.Title != "Pinned Refactor" || !e.Pinned {
		t.Errorf("saved entry a = %+v, want title+pin", e)
	}
}
