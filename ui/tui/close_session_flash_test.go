package ui

import (
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/gogent"
)

// This file tests issue #209: closing the active session (Ctrl+W ->
// Workbench.CloseActive -> Workbench.CloseSession) must not paint a wrong session
// for a single frame before settling on the final one, whenever the desktop
// z-stack order has diverged from the sidebar order (w.order).
//
// The desktop emits a full frame on each toolkit mutation: AddLayer, RemoveLayer
// and an explicit Redraw all run compose -> App.Apply. During the old close path
// the closing layer was removed first, which revealed whatever layer sat directly
// beneath it in the z-stack (Desktop.TopLayer), and that wrong layer was painted
// for a frame before Focus(w.order tail) finally raised the intended session.
//
// The fix computes the post-close active session (the w.order tail) up front and
// raises it to the top of the z-stack BEFORE removing the closing layer, so every
// frame painted during the close shows only the closing window or the target. The
// tests below drive a Workbench whose z-stack has been reordered away from
// w.order and assert, via a desktop frame spy, the exact sequence of top layers.
//
// Decision documented by these tests: the session that becomes active after a
// close is the tail of w.order (the last sidebar entry) excluding the closed id —
// the same session the close has always settled on; only when/how it is shown
// changes. The general "sidebar highlight follows focus" fix is the sibling
// issue #206.

// flashSpy is a passive desktop layer that records, on every composed frame, the
// id of the session whose layer is currently on top of the z-stack. It lets a
// test observe the sequence of "what was on top" across the synchronous
// CloseSession call — i.e. detect a one-frame flash of a wrong session, which is
// invisible to plain before/after assertions.
//
// It sits at the bottom of the layer stack (below every session window) and does
// not accept input, so it never becomes the top layer (until no session remains)
// and never participates in focus/hit-testing. Its DrawFn fires once per compose
// (once per painted frame), which is exactly the granularity at which the flash
// is visible.
type flashSpy struct {
	desktop *tv.Desktop
	// layerID maps a session layer pointer to its id. It must be built once all
	// windows exist and must include the soon-to-be-closed session, because
	// CloseSession deletes it from w.sessions before its layer is removed.
	layerID map[*tv.Layer]string
	// tops is the sequence of top-session ids recorded on each frame since the
	// last reset; "" marks a frame whose top was not a session (background/sidebar
	// or the spy itself once every session is gone).
	tops []string
}

// installFlashSpy adds a recording layer to the bottom of w's desktop and returns
// the spy. Call it before opening sessions so the spy is stacked beneath them.
func installFlashSpy(w *Workbench) *flashSpy {
	sp := &flashSpy{desktop: w.desktop, layerID: map[*tv.Layer]string{}}
	comp := tv.NewComponent(tv.Rect{X: 0, Y: 0, W: w.app.Width(), H: w.app.Height()})
	comp.DrawFn = func(*tv.VisualComponent, tv.Surface) {
		// TopLayer() is read at draw time, mid-compose, while the layer slice is
		// fixed for this frame; the result is exactly this frame's top window.
		sp.tops = append(sp.tops, sp.layerID[sp.desktop.TopLayer()])
	}
	// FullScreen so compose restretches (and therefore always draws) the spy every
	// frame even if its bounds were empty; AcceptInput=false so it never claims
	// focus or the top spot while any session remains.
	w.desktop.AddLayer(tv.NewLayer("flash-spy", comp, false, true))
	return sp
}

// mapLayers populates the spy's layer->id map from the workbench's open sessions.
func (sp *flashSpy) mapLayers(w *Workbench) {
	for id, sw := range w.sessions {
		sp.layerID[sw.layer] = id
	}
}

// reset clears the recorded frame sequence so the next CloseSession is captured in
// isolation (setup Focus calls and any prior closes are ignored).
func (sp *flashSpy) reset() { sp.tops = sp.tops[:0] }

// wrongSessions returns the recorded top-session ids that are neither the closing
// session nor the intended target — i.e. the "flashed" wrong sessions. Empty when
// no wrong session was ever painted (the fix holds).
func (sp *flashSpy) wrongSessions(closing, target string) []string {
	var bad []string
	for _, top := range sp.tops {
		if top == "" || top == closing || top == target {
			continue
		}
		bad = append(bad, top)
	}
	return bad
}

// distinctTops returns the set of distinct non-empty session ids recorded.
func (sp *flashSpy) distinctTops() []string {
	seen := map[string]bool{}
	var out []string
	for _, top := range sp.tops {
		if top == "" || seen[top] {
			continue
		}
		seen[top] = true
		out = append(out, top)
	}
	return out
}

// lastTop returns the most recently recorded top-session id ("" if none or the
// last frame had no session on top).
func (sp *flashSpy) lastTop() string {
	if len(sp.tops) == 0 {
		return ""
	}
	return sp.tops[len(sp.tops)-1]
}

// divergeZStack reorders the desktop z-stack so the sessions end with the last id
// of bottomToTop as the top-most window, using Focus (which only reorders the
// z-stack, never w.order). w.order is left untouched, so the two now disagree —
// the precondition for the flash bug.
func divergeZStack(w *Workbench, bottomToTop []string) {
	// Focus bottom-up; the last focused session ends on top.
	for _, id := range bottomToTop {
		w.Focus(id)
	}
}

// expectedTarget returns the session that should become active after closing id:
// the tail of order excluding id (the documented decision for #209), or "" when id
// is the only session.
func expectedTarget(order []string, id string) string {
	for i := len(order) - 1; i >= 0; i-- {
		if order[i] != id {
			return order[i]
		}
	}
	return ""
}

// TestCloseSessionNoFlashWhenZStackDiverged is the core regression test for #209.
// With the z-stack reordered to [3,1,2] (session 2 on top, session 1 directly
// beneath it) while w.order stays [1,2,3], closing the active session 2 must:
//   - never paint session 1 (the z-stack neighbour) for any frame, and
//   - settle directly on session 3 (the w.order tail).
//
// Under the old code, RemoveLayer(session2) would paint session 1 for one frame
// before Focus(session3) snapped to the target.
func TestCloseSessionNoFlashWhenZStackDiverged(t *testing.T) {
	w := newTestWorkbench(t)
	sp := installFlashSpy(w)
	w.openWindow("1", "One")
	w.openWindow("2", "Two")
	w.openWindow("3", "Three")
	sp.mapLayers(w)

	// w.order stays [1,2,3]; reorder only the z-stack to [3,1,2] (2 on top, 1
	// directly beneath it).
	divergeZStack(w, []string{"3", "1", "2"})
	if got := w.ActiveID(); got != "2" {
		t.Fatalf("setup: ActiveID = %q, want 2 (top of diverged z-stack)", got)
	}
	if order := w.orderIDs(); !equalOrder(order, []string{"1", "2", "3"}) {
		t.Fatalf("setup: w.order must be unchanged by Focus = %v, want [1 2 3]", order)
	}
	sp.reset()

	w.CloseSession("2") // close the active (top) session

	const target = "3" // tail of w.order after pruning "2"
	if bad := sp.wrongSessions("2", target); len(bad) != 0 {
		t.Errorf("CloseSession flashed wrong session(s) %v; only %q (closing) and %q (target) may ever be painted\nfull trace: %v",
			bad, "2", target, sp.tops)
	}
	if got := sp.lastTop(); got != target {
		t.Errorf("final painted top = %q, want target %q\nfull trace: %v", got, target, sp.tops)
	}
	if got := w.ActiveID(); got != target {
		t.Errorf("post-close ActiveID = %q, want %q", got, target)
	}
}

// TestCloseActiveNoFlash exercises the real Ctrl+W entry point (CloseActive closes
// the top-most session) with a diverged z-stack, asserting the same no-flash /
// correct-target contract as the direct CloseSession path.
func TestCloseActiveNoFlash(t *testing.T) {
	w := newTestWorkbench(t)
	sp := installFlashSpy(w)
	w.openWindow("a", "A")
	w.openWindow("b", "B")
	w.openWindow("c", "C")
	sp.mapLayers(w)
	divergeZStack(w, []string{"c", "a", "b"}) // b on top; a directly beneath
	if w.ActiveID() != "b" {
		t.Fatalf("setup: ActiveID = %q, want b", w.ActiveID())
	}
	sp.reset()

	w.CloseActive() // the Ctrl+W path: CloseActive -> CloseSession(top)

	const target = "c"
	if bad := sp.wrongSessions("b", target); len(bad) != 0 {
		t.Errorf("CloseActive flashed wrong session(s) %v\nfull trace: %v", bad, sp.tops)
	}
	if sp.lastTop() != target {
		t.Errorf("final painted top = %q, want %q\nfull trace: %v", sp.lastTop(), target, sp.tops)
	}
	if w.ActiveID() != target {
		t.Errorf("post-close ActiveID = %q, want %q", w.ActiveID(), target)
	}
}

// TestFlashSpyDetectsZNeighbourOnRemoveTopLayer validates that the spy is actually
// sensitive to the flash: removing the top session layer directly (the root-cause
// operation) must be recorded as the z-stack neighbour beneath it — not the
// intended target. If this test fails, the spy cannot observe the bug and the
// no-flash assertions above are vacuous. It also pins the root cause: a plain
// RemoveLayer of the top window reveals the arbitrary z-neighbour.
func TestFlashSpyDetectsZNeighbourOnRemoveTopLayer(t *testing.T) {
	w := newTestWorkbench(t)
	sp := installFlashSpy(w)
	w.openWindow("1", "One")
	w.openWindow("2", "Two")
	w.openWindow("3", "Three")
	sp.mapLayers(w)
	divergeZStack(w, []string{"3", "1", "2"}) // z-stack [3,1,2]; 2 on top; 1 beneath
	sp.reset()

	// Reproduce the root cause in isolation: remove the top (active) layer. The
	// desktop repaints whatever now sits on top — session 1, the z-neighbour.
	w.desktop.RemoveLayer(w.sessions["2"].layer)

	if bad := sp.wrongSessions("2", "3"); len(bad) == 0 {
		t.Fatalf("spy failed to detect the z-neighbour flash; tops = %v", sp.tops)
	}
	if sp.lastTop() != "1" {
		t.Errorf("after removing top layer, painted top = %q, want z-neighbour 1\ntops = %v", sp.lastTop(), sp.tops)
	}
}

// TestCloseSessionRemovesFromAllStores verifies the closed session is gone from
// every place that tracks it: the sessions map, w.order, the pinned set and the
// sidebar tree.
func TestCloseSessionRemovesFromAllStores(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B")
	w.openWindow("c", "C")
	w.CloseSession("b")

	if w.sessions["b"] != nil {
		t.Error("closed session still present in sessions map")
	}
	if w.pinned["b"] {
		t.Error("closed session still in pinned set")
	}
	if order := w.orderIDs(); !equalOrder(order, []string{"a", "c"}) {
		t.Errorf("w.order = %v, want [a c]", order)
	}
	if got := sidebarIDs(w.sidebar); !equalOrder(got, []string{"a", "c"}) {
		t.Errorf("sidebar order = %v, want [a c]", got)
	}
}

// TestCloseSessionSidebarFocusFollowsTarget verifies the fix's Focus(target) also
// routes the sidebar's Overall/TODO focus onto the target (via refreshOverall ->
// sidebar.focusSession), so the sidebar highlight lands on the raised window in
// the same step — the close-path piece of the #206 highlight-follows-focus story.
func TestCloseSessionSidebarFocusFollowsTarget(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B")
	w.openWindow("c", "C")
	divergeZStack(w, []string{"c", "a", "b"}) // b active
	w.CloseSession("b")

	if w.sidebar.focused != "c" {
		t.Errorf("sidebar.focused = %q, want c (the raised target)", w.sidebar.focused)
	}
}

// TestCloseSessionActiveIsOrderTail covers closing the active session when it IS
// the w.order tail: the target must be the new tail, raised with no flash. The
// z-stack neighbour beneath the closed tail differs from the new tail, so a flash
// would reveal a wrong session under the old code.
func TestCloseSessionActiveIsOrderTail(t *testing.T) {
	w := newTestWorkbench(t)
	sp := installFlashSpy(w)
	w.openWindow("a", "A")
	w.openWindow("b", "B")
	w.openWindow("c", "C")
	sp.mapLayers(w)
	// c (the tail) is the active/top window, but a sits directly beneath it (not b).
	divergeZStack(w, []string{"b", "a", "c"}) // z-stack [b,a,c]; c on top; a beneath
	if w.ActiveID() != "c" {
		t.Fatalf("setup: ActiveID = %q, want c", w.ActiveID())
	}
	sp.reset()

	w.CloseSession("c")

	const target = "b" // new tail after pruning c
	if bad := sp.wrongSessions("c", target); len(bad) != 0 {
		t.Errorf("flashed wrong session(s) %v; a (z-neighbour) must never show\ntops = %v", bad, sp.tops)
	}
	if w.ActiveID() != target {
		t.Errorf("post-close ActiveID = %q, want %q", w.ActiveID(), target)
	}
}

// TestCloseSessionTwoSessionsNoFlash covers the minimal multi-session case: with
// two sessions, closing the active one must raise the other directly with no
// flash and leave it active.
func TestCloseSessionTwoSessionsNoFlash(t *testing.T) {
	w := newTestWorkbench(t)
	sp := installFlashSpy(w)
	w.openWindow("a", "A")
	w.openWindow("b", "B")
	sp.mapLayers(w)
	if w.ActiveID() != "b" {
		t.Fatalf("setup: ActiveID = %q, want b (last opened)", w.ActiveID())
	}
	sp.reset()

	w.CloseSession("b")

	if bad := sp.wrongSessions("b", "a"); len(bad) != 0 {
		t.Errorf("flashed wrong session(s) %v\ntops = %v", bad, sp.tops)
	}
	if w.ActiveID() != "a" {
		t.Errorf("post-close ActiveID = %q, want a", w.ActiveID())
	}
	if order := w.orderIDs(); !equalOrder(order, []string{"a"}) {
		t.Errorf("w.order = %v, want [a]", order)
	}
}

// TestCloseSessionLastRemaining covers the empty case: closing the only session
// leaves an empty desktop. target is "" so Focus is skipped; there must be no
// panic, no flash of any session, and ActiveID must become "".
func TestCloseSessionLastRemaining(t *testing.T) {
	w := newTestWorkbench(t)
	sp := installFlashSpy(w)
	w.openWindow("solo", "Solo")
	sp.mapLayers(w)
	sp.reset()

	w.CloseSession("solo")

	if order := w.orderIDs(); len(order) != 0 {
		t.Errorf("w.order = %v, want empty", order)
	}
	if w.ActiveID() != "" {
		t.Errorf("post-close ActiveID = %q, want empty", w.ActiveID())
	}
	// With no target there is nothing to flash; the only session ever on top was
	// the one being closed, and after removal the desktop shows a non-session.
	if got := sp.distinctTops(); len(got) != 0 {
		t.Errorf("recorded session tops = %v, want none (no target, no neighbour)", got)
	}
	if sp.lastTop() != "" {
		t.Errorf("final painted top = %q, want non-session (desktop cleared)", sp.lastTop())
	}
}

// TestCloseSessionUnknownIDIsNoOp verifies closing an unknown id, an empty id, and
// an already-closed id are all safe no-ops that leave real sessions intact.
func TestCloseSessionUnknownIDIsNoOp(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B")
	before := w.orderIDs()

	w.CloseSession("nope") // unknown
	w.CloseSession("")     // empty id
	if order := w.orderIDs(); !equalOrder(order, before) {
		t.Errorf("no-op closes changed order: %v, want %v", order, before)
	}
	if w.sessions["a"] == nil || w.sessions["b"] == nil {
		t.Error("a real session was removed by a no-op close")
	}

	// Closing a real id then closing it again (now absent) must not panic.
	w.CloseSession("a")
	w.CloseSession("a")
	if order := w.orderIDs(); !equalOrder(order, []string{"b"}) {
		t.Errorf("order after closing a twice = %v, want [b]", order)
	}
}

// TestCloseSessionReadOnlyAnalysisSkipsBackend verifies the read-only analysis
// branch of CloseSession (issue #58): closing an analysis window raises the
// remaining session with no flash and skips the backend teardown (OnClose) and
// layout persistence that only live sessions own.
func TestCloseSessionReadOnlyAnalysisSkipsBackend(t *testing.T) {
	w := newTestWorkbench(t)
	sp := installFlashSpy(w)
	var onClose string
	w.handlers.OnClose = func(id string) { onClose = id }
	savedLayout := false
	w.handlers.SaveLayout = func(gogent.Layout) { savedLayout = true }

	w.openWindow("live", "Live")
	w.OpenAnalysisSession(RestoredSession{ID: "saved", Title: "Saved"})
	sp.mapLayers(w)

	analysisID := ""
	for id, sw := range w.sessions {
		if sw.readOnly {
			analysisID = id
		}
	}
	if analysisID == "" {
		t.Fatal("no read-only analysis window was opened")
	}
	// The analysis window was opened last, so it is on top.
	if w.ActiveID() != analysisID {
		t.Fatalf("setup: ActiveID = %q, want analysis %q", w.ActiveID(), analysisID)
	}
	savedLayout = false
	sp.reset()

	w.CloseSession(analysisID)

	if onClose != "" {
		t.Errorf("OnClose invoked for read-only window: %q (should be skipped)", onClose)
	}
	if savedLayout {
		t.Error("SaveLayout invoked for read-only close (should be skipped)")
	}
	if w.sessions[analysisID] != nil {
		t.Error("read-only analysis window not removed")
	}
	// The remaining live session is the w.order tail and thus the target.
	if bad := sp.wrongSessions(analysisID, "live"); len(bad) != 0 {
		t.Errorf("read-only close flashed wrong session(s) %v\ntops = %v", bad, sp.tops)
	}
	if w.ActiveID() != "live" {
		t.Errorf("post-close ActiveID = %q, want live", w.ActiveID())
	}
}

// TestCloseOthersDivergedZStack verifies CloseOthers (which calls CloseSession in
// a loop) does not panic and leaves exactly the kept session when the z-stack is
// diverged. Each underlying close raises the current w.order tail first, so the
// desktop never settles on a z-neighbour between iterations.
func TestCloseOthersDivergedZStack(t *testing.T) {
	w := newTestWorkbench(t)
	sp := installFlashSpy(w)
	w.openWindow("a", "A")
	w.openWindow("b", "B")
	w.openWindow("c", "C")
	sp.mapLayers(w)
	divergeZStack(w, []string{"c", "a", "b"}) // b on top
	sp.reset()

	w.CloseOthers("a") // keep a, close b and c

	if order := w.orderIDs(); !equalOrder(order, []string{"a"}) {
		t.Errorf("w.order = %v, want [a]", order)
	}
	if w.sessions["a"] == nil {
		t.Error("kept session a was removed")
	}
	if w.sessions["b"] != nil || w.sessions["c"] != nil {
		t.Error("other sessions not removed")
	}
	// No session other than a should remain on top after the loop settles.
	if w.ActiveID() != "a" {
		t.Errorf("post-close ActiveID = %q, want a", w.ActiveID())
	}
}

// TestCloseSessionDivergenceMatrix sweeps several z-stack divergences (including
// closing the head, middle and tail of w.order) and asserts the invariant for
// each: only the closing session and the w.order-tail target are ever painted,
// and the target ends up active.
func TestCloseSessionDivergenceMatrix(t *testing.T) {
	cases := []struct {
		name   string   // creation order == initial w.order
		open   []string // bottom->top divergence; the last element is the active session to close
		zstack []string
	}{
		{"3 sessions, close middle", []string{"1", "2", "3"}, []string{"3", "1", "2"}},
		{"3 sessions, close tail", []string{"a", "b", "c"}, []string{"b", "a", "c"}},
		{"4 sessions, close head", []string{"1", "2", "3", "4"}, []string{"4", "2", "3", "1"}},
		{"4 sessions, close middle", []string{"1", "2", "3", "4"}, []string{"1", "4", "3", "2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			sp := installFlashSpy(w)
			for _, id := range tc.open {
				w.openWindow(id, id)
			}
			sp.mapLayers(w)
			divergeZStack(w, tc.zstack)

			closing := tc.zstack[len(tc.zstack)-1]
			if w.ActiveID() != closing {
				t.Fatalf("setup: ActiveID = %q, want closing %q", w.ActiveID(), closing)
			}
			// w.order == creation order (Focus never changes it).
			target := expectedTarget(tc.open, closing)
			sp.reset()

			w.CloseSession(closing)

			if bad := sp.wrongSessions(closing, target); len(bad) != 0 {
				t.Errorf("flashed wrong session(s) %v; only %q/%q allowed\ntops = %v", bad, closing, target, sp.tops)
			}
			if sp.lastTop() != target {
				t.Errorf("final painted top = %q, want target %q\ntops = %v", sp.lastTop(), target, sp.tops)
			}
			if w.ActiveID() != target {
				t.Errorf("post-close ActiveID = %q, want %q", w.ActiveID(), target)
			}
		})
	}
}

// TestCloseSessionNonActiveDocumentsExistingContract documents that closing a
// non-active session (e.g. via CloseOthers, not the Ctrl+W path) still ends with
// the w.order tail raised — the same final session the active-close path settles
// on. This is pre-existing behaviour (unchanged by #209, which only fixes the
// flash); it is locked in here so a future refactor cannot silently alter which
// session wins.
func TestCloseSessionNonActiveDocumentsExistingContract(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B")
	w.openWindow("c", "C")
	if w.ActiveID() != "c" {
		t.Fatalf("setup: ActiveID = %q, want c (last opened)", w.ActiveID())
	}

	// Close a non-active session (a). The w.order tail (c) is the target and ends
	// up raised — the user's current window is preserved.
	w.CloseSession("a")
	if w.ActiveID() != "c" {
		t.Errorf("post-close ActiveID = %q, want c (w.order tail)", w.ActiveID())
	}
	if order := w.orderIDs(); !equalOrder(order, []string{"b", "c"}) {
		t.Errorf("w.order = %v, want [b c]", order)
	}
}
