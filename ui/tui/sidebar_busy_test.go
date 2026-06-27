package ui

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/agent"
)

// This file tests the per-session idle/active sidebar marker (issue #236) and the
// removal of the phantom glyph-less global "needs attention" header counter (issue
// #230). The two are one change: every session row carries its own ○/● marker (and
// its own ⏳/❓ badge when it needs attention), and the "Sessions & Agents" header
// renders no aggregate count at all.
//
// Glyph contract from #236 (locked in):
//   - ○ idle (no turn in flight), ● active (a turn in flight);
//   - distinct from the sub-agent lifecycle set used by statusIcon (▶ ⏸ ✓ ✗ •), so a
//     session row — even one coordinating a working sub-agent — never reads as a
//     sub-agent.

// idleMark / activeMark are the per-session markers (#236). They are kept as named
// constants so a future glyph change only has to update one place, and so the
// regression guards below cannot accidentally drift to a different character.
const (
	idleMark   = "○"
	activeMark = "●"
)

// subAgentGlyphs is statusIcon's lifecycle set. A session row must never show any of
// these — the triangle ▶ in particular belongs to sub-agents only.
var subAgentGlyphs = []string{"▶", "⏸", "✓", "✗", "•"}

// --- glyph + label unit tests ----------------------------------------------

// TestSessionStatusIcon pins the two markers and, critically, that they are distinct
// from each other and from every sub-agent lifecycle glyph (the #236 disambiguation
// requirement). A session row that reused ▶ would be ambiguous, so this guards the
// dedicated session pair.
func TestSessionStatusIcon(t *testing.T) {
	if got := sessionStatusIcon(false); got != idleMark {
		t.Fatalf("sessionStatusIcon(false) = %q, want %q", got, idleMark)
	}
	if got := sessionStatusIcon(true); got != activeMark {
		t.Fatalf("sessionStatusIcon(true) = %q, want %q", got, activeMark)
	}
	if idleMark == activeMark {
		t.Fatalf("idle and active markers must differ; both are %q", idleMark)
	}
	for _, g := range subAgentGlyphs {
		if idleMark == g {
			t.Fatalf("idle marker %q collides with sub-agent glyph %q", idleMark, g)
		}
		if activeMark == g {
			t.Fatalf("active marker %q collides with sub-agent glyph %q", activeMark, g)
		}
	}
}

// TestSessionLabelMarker covers the rendered session row's LEADING glyph across the
// busy/pin/approval/clarify combinations. The marker is ○ when idle, ● when busy,
// and it always leads the label — no badge or the ★ pin marker may displace it.
func TestSessionLabelMarker(t *testing.T) {
	for _, tc := range []struct {
		name       string
		busy       bool
		pinned     bool
		pending    bool
		clarify    bool
		wantMark   string
		wantStar   bool
		wantAppr   bool
		wantClar   bool
		wantSuffix string // the glyph that must trail the whole label, "" if none
	}{
		{name: "idle plain", wantMark: idleMark},
		{name: "active plain", busy: true, wantMark: activeMark},
		{name: "idle pinned", pinned: true, wantMark: idleMark, wantStar: true},
		{name: "active pinned", busy: true, pinned: true, wantMark: activeMark, wantStar: true},
		{name: "active+approval", busy: true, pending: true, wantMark: activeMark, wantAppr: true, wantSuffix: approvalBadge},
		{name: "active+clarify", busy: true, clarify: true, wantMark: activeMark, wantClar: true, wantSuffix: clarifyBadge},
		{name: "active+approval+clarify", busy: true, pending: true, clarify: true, wantMark: activeMark, wantAppr: true, wantClar: true, wantSuffix: clarifyBadge},
		{name: "idle+approval+clarify", pending: true, clarify: true, wantMark: idleMark, wantAppr: true, wantClar: true, wantSuffix: clarifyBadge},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			label := sessionLabel("Session 1", tc.busy, tc.pinned, tc.pending, tc.clarify)

			// The marker leads the label.
			if !strings.HasPrefix(label, tc.wantMark) {
				t.Fatalf("leading marker = %q, want %q (label %q)", label[:len([]rune(label))], tc.wantMark, label)
			}
			// The other marker never appears on this row.
			other := activeMark
			if tc.wantMark == activeMark {
				other = idleMark
			}
			if strings.Contains(label, other) {
				t.Fatalf("row for %s leaked the %q marker: %q", tc.name, other, label)
			}
			if (strings.Contains(label, "★") != tc.wantStar) && tc.wantStar {
				t.Fatalf("expected ★ in %q", label)
			}
			if strings.Contains(label, approvalBadge) != tc.wantAppr {
				t.Fatalf("approval badge presence = %v, want %v (%q)", strings.Contains(label, approvalBadge), tc.wantAppr, label)
			}
			if strings.Contains(label, clarifyBadge) != tc.wantClar {
				t.Fatalf("clarify badge presence = %v, want %v (%q)", strings.Contains(label, clarifyBadge), tc.wantClar, label)
			}
			if tc.wantSuffix != "" && !strings.HasSuffix(label, tc.wantSuffix) {
				t.Fatalf("expected label to trail with %q, got %q", tc.wantSuffix, label)
			}
		})
	}
}

// TestSessionLabelNeverShowsSubAgentGlyph is the #236 regression guard: no session
// row — for ANY combination of flags — may render a sub-agent lifecycle glyph. This
// catches a regression where sessionLabel is accidentally wired to statusIcon.
func TestSessionLabelNeverShowsSubAgentGlyph(t *testing.T) {
	for _, busy := range []bool{false, true} {
		for _, pinned := range []bool{false, true} {
			for _, pending := range []bool{false, true} {
				for _, clarify := range []bool{false, true} {
					label := sessionLabel("Session 1", busy, pinned, pending, clarify)
					for _, g := range subAgentGlyphs {
						if strings.Contains(label, g) {
							t.Fatalf("session label leaked sub-agent glyph %q (busy=%v pinned=%v pending=%v clarify=%v): %q",
								g, busy, pinned, pending, clarify, label)
						}
					}
				}
			}
		}
	}
}

// --- sidebar state-machine tests (detached sidebar) -------------------------

// TestSidebarAddSessionIdle asserts a freshly registered session is idle (○), never
// active and never the legacy sub-agent bullet (•). This is the baseline before any
// busy transition.
func TestSidebarAddSessionIdle(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	node := s.sessions["s1"]
	if node == nil {
		t.Fatal("addSession did not register the node")
	}
	if !strings.HasPrefix(node.Label, idleMark) {
		t.Fatalf("new session should be idle (%q), got %q", idleMark, node.Label)
	}
	if strings.Contains(node.Label, activeMark) {
		t.Fatalf("new session should not be active: %q", node.Label)
	}
	if strings.Contains(node.Label, "•") {
		t.Fatalf("new session should not show the legacy sub-agent bullet: %q", node.Label)
	}
}

// TestSidebarSetBusyTransition exercises the idle→active→idle relabel via setBusy,
// the helper the per-session marker turns on. The node's leading glyph must flip
// ○ → ● → ○ and the busy map must track it exactly.
func TestSidebarSetBusyTransition(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)

	if s.busy["s1"] {
		t.Fatal("busy should be false on a fresh session")
	}

	s.setBusy("s1", "Session 1", false, true)
	node := s.sessions["s1"]
	if !strings.HasPrefix(node.Label, activeMark) {
		t.Fatalf("after setBusy(true) want leading %q, got %q", activeMark, node.Label)
	}
	if !s.busy["s1"] {
		t.Fatal("setBusy(true) did not record busy=true in the map")
	}

	s.setBusy("s1", "Session 1", false, false)
	node = s.sessions["s1"]
	if !strings.HasPrefix(node.Label, idleMark) {
		t.Fatalf("after setBusy(false) want leading %q, got %q", idleMark, node.Label)
	}
	if s.busy["s1"] {
		t.Fatal("setBusy(false) should clear the busy map entry")
	}
}

// TestSidebarSetBusyPreservesBadges verifies flipping the busy marker never drops the
// ⏳/❓ badges or the ★ pin marker — the carry-over that all relabel paths must honor.
func TestSidebarSetBusyPreservesBadges(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", true)
	s.setApproval("s1", "Session 1", true, true)
	s.setClarify("s1", "Session 1", true, true)

	s.setBusy("s1", "Session 1", true, true)
	node := s.sessions["s1"]
	for _, want := range []string{activeMark, "★", approvalBadge, clarifyBadge} {
		if !strings.Contains(node.Label, want) {
			t.Fatalf("setBusy(true) dropped %q from %q", want, node.Label)
		}
	}
	if !strings.HasSuffix(node.Label, clarifyBadge) {
		t.Fatalf("clarify must still trail after setBusy: %q", node.Label)
	}

	s.setBusy("s1", "Session 1", true, false)
	node = s.sessions["s1"]
	for _, want := range []string{idleMark, "★", approvalBadge, clarifyBadge} {
		if !strings.Contains(node.Label, want) {
			t.Fatalf("setBusy(false) dropped %q from %q", want, node.Label)
		}
	}
}

// TestSidebarBusySurvivesRelabel is an explicit #236 acceptance: a rename and a pin
// toggle while a turn is in flight must keep the ● marker.
func TestSidebarBusySurvivesRelabel(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.setBusy("s1", "Session 1", false, true)

	s.relabelSession("s1", "Renamed", false)
	node := s.sessions["s1"]
	if !strings.HasPrefix(node.Label, activeMark) {
		t.Fatalf("rename dropped the active marker: %q", node.Label)
	}
	if !strings.Contains(node.Label, "Renamed") {
		t.Fatalf("rename did not apply the new title: %q", node.Label)
	}

	// Pin toggle mid-turn must also keep the ● and surface ★.
	s.relabelSession("s1", "Renamed", true)
	node = s.sessions["s1"]
	if !strings.HasPrefix(node.Label, activeMark) {
		t.Fatalf("pin toggle dropped the active marker: %q", node.Label)
	}
	if !strings.Contains(node.Label, "★") {
		t.Fatalf("pin marker missing after relabel(pinned): %q", node.Label)
	}
}

// TestSidebarBusySurvivesApprovalClarify checks the symmetric carry-over: while busy,
// an approval/clarify state change must keep the ● (and the other badge).
func TestSidebarBusySurvivesApprovalClarify(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.setBusy("s1", "Session 1", false, true)

	s.setApproval("s1", "Session 1", false, true)
	if got := s.sessions["s1"].Label; !strings.HasPrefix(got, activeMark) || !strings.Contains(got, approvalBadge) {
		t.Fatalf("setApproval dropped the active marker or the badge: %q", got)
	}
	s.setClarify("s1", "Session 1", false, true)
	if got := s.sessions["s1"].Label; !strings.HasPrefix(got, activeMark) || !strings.Contains(got, clarifyBadge) {
		t.Fatalf("setClarify dropped the active marker or the badge: %q", got)
	}

	// Clearing approval mid-turn keeps ●; clarify still on.
	s.setApproval("s1", "Session 1", false, false)
	if got := s.sessions["s1"].Label; !strings.HasPrefix(got, activeMark) || !strings.Contains(got, clarifyBadge) {
		t.Fatalf("clearing approval lost the active marker or clarify badge: %q", got)
	}
}

// TestSidebarBusyOutOfOrder covers "intent recorded before the node exists": setBusy
// may arrive before addSession (the sidebar is registered after the window). The busy
// map must record it so the node is created with ● once addSession lands — mirroring
// the approvals/clarify robustness contract.
func TestSidebarBusyOutOfOrder(t *testing.T) {
	s := newTestSidebar()
	s.setBusy("late", "Late", false, true)
	if !s.busy["late"] {
		t.Fatal("busy intent should be recorded even with no node yet")
	}
	s.addSession("late", "Late", false)
	node := s.sessions["late"]
	if node == nil {
		t.Fatal("addSession did not register the node")
	}
	if !strings.HasPrefix(node.Label, activeMark) {
		t.Fatalf("out-of-order busy should surface ● on add: %q", node.Label)
	}
}

// TestSidebarRemoveClearsBusy ensures closing a busy session does not leak its busy
// state (parallel to the approval/clarify remove tests).
func TestSidebarRemoveClearsBusy(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.setBusy("s1", "Session 1", false, true)
	s.removeSession("s1")
	if s.busy["s1"] {
		t.Fatal("busy state leaked after removeSession")
	}
	if _, ok := s.sessions["s1"]; ok {
		t.Fatal("session node leaked after removeSession")
	}
}

// --- syncBusy tests (the Workbench.tickBusyStatuses funnel) -----------------

// TestSidebarSyncBusyTransitions drives syncBusy through a multi-session scenario and
// asserts each session shows its OWN marker and that `changed` is reported exactly on
// a transition. This is the core #236 invariant: the marker is per-session, derived
// from the busy set, and reconciled even when the set empties (busy→idle edge).
func TestSidebarSyncBusyTransitions(t *testing.T) {
	s := newTestSidebar()
	s.addSession("a", "A", false)
	s.addSession("b", "B", false)

	// Initially both idle.
	if s.syncBusy(map[string]bool{}) {
		t.Fatal("syncBusy on already-idle sessions should report no change")
	}
	assertLeadingMark(t, s.sessions["a"].Label, idleMark, "a initial")
	assertLeadingMark(t, s.sessions["b"].Label, idleMark, "b initial")

	// a goes busy: only a flips to ●, b stays ○.
	if !s.syncBusy(map[string]bool{"a": true}) {
		t.Fatal("syncBusy should report a change when a turns busy")
	}
	assertLeadingMark(t, s.sessions["a"].Label, activeMark, "a busy")
	assertLeadingMark(t, s.sessions["b"].Label, idleMark, "b still idle")
	if !s.busy["a"] || s.busy["b"] {
		t.Fatalf("busy map out of sync: a=%v b=%v", s.busy["a"], s.busy["b"])
	}

	// Hand off: b busy, a idle. The previous ● on a must clear.
	if !s.syncBusy(map[string]bool{"b": true}) {
		t.Fatal("syncBusy should report a change on the a→idle/b→busy hand-off")
	}
	assertLeadingMark(t, s.sessions["a"].Label, idleMark, "a back to idle")
	assertLeadingMark(t, s.sessions["b"].Label, activeMark, "b now busy")

	// Both idle (the tricky edge: the set empties, which tickBusyStatuses would
	// otherwise early-return and strand the last ●).
	if !s.syncBusy(map[string]bool{}) {
		t.Fatal("syncBusy should report a change when the last session goes idle")
	}
	assertLeadingMark(t, s.sessions["a"].Label, idleMark, "a idle after drain")
	assertLeadingMark(t, s.sessions["b"].Label, idleMark, "b idle after drain")
	if s.syncBusy(map[string]bool{}) {
		t.Fatal("a second all-idle sync should be a no-op (no change)")
	}
}

// TestSidebarSyncBusyPreservesPin verifies a syncBusy-driven flip reads the workbench
// pin state (via IsPinned) so the ★ survives a marker transition. It uses a real
// workbench so the pin is set through the genuine TogglePin path.
func TestSidebarSyncBusyPreservesPin(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.TogglePin("a") // genuine pin: relabel + ★

	if got := w.sessions["a"].busy; got {
		t.Fatal("precondition: fresh session should not be busy")
	}

	// A turn starts: syncBusy must keep the ★ while flipping ○→●.
	if !w.sidebar.syncBusy(map[string]bool{"a": true}) {
		t.Fatal("syncBusy should report a change turning a busy")
	}
	label := w.sidebar.sessions["a"].Label
	if !strings.HasPrefix(label, activeMark) {
		t.Fatalf("expected ● after syncBusy(true): %q", label)
	}
	if !strings.Contains(label, "★") {
		t.Fatalf("syncBusy dropped the ★ pin marker: %q", label)
	}

	// Turn ends: flip back to ○, ★ still present.
	w.sidebar.syncBusy(map[string]bool{})
	label = w.sidebar.sessions["a"].Label
	if !strings.HasPrefix(label, idleMark) {
		t.Fatalf("expected ○ after syncBusy drain: %q", label)
	}
	if !strings.Contains(label, "★") {
		t.Fatalf("syncBusy dropped the ★ on idle: %q", label)
	}
}

// TestSidebarSyncBusyPreservesBadges asserts a syncBusy transition does not drop a
// waiting-for-input badge that was armed out-of-band.
func TestSidebarSyncBusyPreservesBadges(t *testing.T) {
	s := newTestSidebar()
	s.addSession("a", "A", false)
	s.setApproval("a", "A", false, true)
	s.setClarify("a", "A", false, true)

	s.syncBusy(map[string]bool{"a": true})
	label := s.sessions["a"].Label
	for _, want := range []string{activeMark, approvalBadge, clarifyBadge} {
		if !strings.Contains(label, want) {
			t.Fatalf("syncBusy dropped %q: %q", want, label)
		}
	}
}

// TestTickBusyStatusesWiresMarker is an integration test of the real production data
// path: it flips a SessionWindow's busy flag and drives Workbench.tickBusyStatuses,
// asserting the sidebar marker follows without any direct sidebar call. This guards
// the tui.go wiring (build busyIDs → syncBusy before the all-idle early return).
func TestTickBusyStatusesWiresMarker(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B")

	// Make A busy the way a real turn does and tick once.
	w.sessions["a"].busy = true
	w.tickBusyStatuses()
	if got := w.sidebar.sessions["a"].Label; !strings.HasPrefix(got, activeMark) {
		t.Fatalf("tickBusyStatuses did not mark A active: %q", got)
	}
	if got := w.sidebar.sessions["b"].Label; !strings.HasPrefix(got, idleMark) {
		t.Fatalf("B should remain idle: %q", got)
	}

	// A finishes; the next tick must clear its ● (the busy→idle edge).
	w.sessions["a"].busy = false
	w.tickBusyStatuses()
	if got := w.sidebar.sessions["a"].Label; !strings.HasPrefix(got, idleMark) {
		t.Fatalf("tickBusyStatuses did not clear A's marker on idle: %q", got)
	}
}

// --- push-on-transition (deliverSessionEvent drives the marker at the edge) --

// The idle/active marker is reconciled on two paths: the 1s tickBusyStatuses
// backstop, AND an immediate push inside Workbench.deliverSessionEvent that fires
// on the event which actually flips sw.busy (issue #236). The push closes the
// previous up-to-1s latency: a turn ending (Final/Error) clears the ● at once, and
// the turn's first streamed event arms it at once. These tests drive the real live
// delivery seam (Workbench.deliverSessionEvent) — the same seam issue #227's test
// uses — so they exercise the genuine wiring, not a re-implementation of it.

// TestBusyMarkerPushedOnTurnEndEvent asserts the busy→idle edge is pushed the
// instant a SessionEventFinal lands, WITHOUT waiting for the next tick sweep. The
// marker must read ○ right after delivery.
func TestBusyMarkerPushedOnTurnEndEvent(t *testing.T) {
	w := newTestWorkbench(t)
	silenceNotifications(w)
	sw := w.openWindow("a", "Alpha")

	// A turn is in flight: arm both the window flag and the sidebar active marker.
	sw.busy = true
	w.sidebar.setBusy("a", "Alpha", false, true)
	if got := w.sidebar.sessions["a"].Label; !strings.HasPrefix(got, activeMark) {
		t.Fatalf("precondition: marker should be active, got %q", got)
	}

	// The turn ends. sw.apply(Final) clears sw.busy; the push must clear the marker
	// immediately — no tickBusyStatuses involved.
	if !w.deliverSessionEvent("a", agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"}) {
		t.Fatal("deliverSessionEvent reported the Final was not delivered")
	}
	if got := w.sidebar.sessions["a"].Label; !strings.HasPrefix(got, idleMark) {
		t.Fatalf("Final should push the idle marker without a tick, got %q", got)
	}
	if w.sidebar.busy["a"] {
		t.Errorf("sidebar.busy[a] should be cleared after the Final push")
	}
}

// TestBusyMarkerPushedOnErrorEvent mirrors the above for the error turn-end path:
// a SessionEventError also clears sw.busy via sw.apply, so the marker must drop to
// ○ at once.
func TestBusyMarkerPushedOnErrorEvent(t *testing.T) {
	w := newTestWorkbench(t)
	silenceNotifications(w)
	sw := w.openWindow("a", "Alpha")

	sw.busy = true
	w.sidebar.setBusy("a", "Alpha", false, true)

	if !w.deliverSessionEvent("a", agent.SessionEvent{Type: agent.SessionEventError, Err: nil}) {
		t.Fatal("deliverSessionEvent reported the Error was not delivered")
	}
	if got := w.sidebar.sessions["a"].Label; !strings.HasPrefix(got, idleMark) {
		t.Fatalf("Error should push the idle marker without a tick, got %q", got)
	}
	if w.sidebar.busy["a"] {
		t.Errorf("sidebar.busy[a] should be cleared after the Error push")
	}
}

// TestBusyMarkerPushedOnFirstStreamedEvent covers the idle→active edge. The submit
// path sets sw.busy=true out-of-band (no SessionEvent); the sidebar only learns of
// it when the FIRST streamed event arrives. That event's delivery must push ● at
// once — sw.apply(Usage) does not touch busy, but the transition detector
// (sidebar.busy[id] != sw.busy) flips the marker.
func TestBusyMarkerPushedOnFirstStreamedEvent(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("a", "Alpha")

	// Submit just happened: sw.busy is true, but the sidebar has seen no event yet.
	sw.busy = true
	if got := w.sidebar.sessions["a"].Label; !strings.HasPrefix(got, idleMark) {
		t.Fatalf("precondition: marker should start idle, got %q", got)
	}

	// First streamed event. The push arms the active marker immediately.
	if !w.deliverSessionEvent("a", agent.SessionEvent{Type: agent.SessionEventUsage}) {
		t.Fatal("deliverSessionEvent reported the Usage event was not delivered")
	}
	if got := w.sidebar.sessions["a"].Label; !strings.HasPrefix(got, activeMark) {
		t.Fatalf("first streamed event should push the active marker without a tick, got %q", got)
	}
	if !w.sidebar.busy["a"] {
		t.Errorf("sidebar.busy[a] should be set after the first-event push")
	}
}

// TestBusyMarkerPushPreservesPinAndBadges verifies the push threads the live title
// and pinned flag (read in deliverSessionEvent) so a pinned, awaiting-approval
// session keeps its ★ and ⏳ across both the arm (first event) and clear (Final)
// pushes. This guards the carry-over the push inherits from setBusy.
func TestBusyMarkerPushPreservesPinAndBadges(t *testing.T) {
	w := newTestWorkbench(t)
	silenceNotifications(w)
	sw := w.openWindow("a", "Alpha")
	w.TogglePin("a") // genuine pin path: w.pinned[a]=true + ★
	w.sidebar.setApproval("a", "Alpha", true, true)
	sw.busy = true

	// First streamed event arms ●; ★ and ⏳ must survive.
	if !w.deliverSessionEvent("a", agent.SessionEvent{Type: agent.SessionEventUsage}) {
		t.Fatal("Usage event not delivered")
	}
	label := w.sidebar.sessions["a"].Label
	for _, want := range []string{activeMark, "★", approvalBadge} {
		if !strings.Contains(label, want) {
			t.Errorf("arm push dropped %q: %q", want, label)
		}
	}

	// Turn ends; the idle push must also keep ★ and ⏳.
	if !w.deliverSessionEvent("a", agent.SessionEvent{Type: agent.SessionEventFinal, Text: "done"}) {
		t.Fatal("Final event not delivered")
	}
	label = w.sidebar.sessions["a"].Label
	for _, want := range []string{idleMark, "★", approvalBadge} {
		if !strings.Contains(label, want) {
			t.Errorf("clear push dropped %q: %q", want, label)
		}
	}
}

// --- per-session independence (rendered tree) -------------------------------

// TestSidebarPerSessionMarkersRendered renders the whole panel and asserts each
// session row shows its OWN marker — the headline #236 requirement that there is no
// single global state. Idle and active sessions coexist with distinct glyphs.
func TestSidebarPerSessionMarkersRendered(t *testing.T) {
	w := newTestWorkbench(t)
	// Register session nodes directly on the sidebar (no real window layers) so the
	// rendered buffer shows the tree, not overlapping session windows — the same
	// render-path setup TestSidebarDrawsTodosInMiddleRegion uses.
	w.sidebar.addSession("idle1", "Alpha", false)
	w.sidebar.addSession("busy1", "Bravo", false)
	w.sidebar.addSession("idle2", "Charlie", false)
	w.sidebar.addSession("busy2", "Delta", false)

	// Arm two busy and two idle sessions directly through the sidebar API.
	w.sidebar.setBusy("busy1", "Bravo", false, true)
	w.sidebar.setBusy("busy2", "Delta", false, true)

	rows := renderSidebarRows(t, w, tv.Rect{X: 48, Y: 1, W: 32, H: 24})

	want := map[string]string{
		"Alpha":   idleMark,
		"Bravo":   activeMark,
		"Charlie": idleMark,
		"Delta":   activeMark,
	}
	for title, mark := range want {
		idx := rowWith(rows, title)
		if idx < 0 {
			t.Fatalf("session %q not rendered in tree:\n%s", title, joinRows(rows))
		}
		row := rows[idx]
		if !strings.Contains(row, mark) {
			t.Fatalf("session %q should show %q, row=%q", title, mark, row)
		}
		other := idleMark
		if mark == idleMark {
			other = activeMark
		}
		if strings.Contains(row, other) {
			t.Fatalf("session %q row leaked the %q marker: %q", title, other, row)
		}
		// No session row may carry a sub-agent glyph.
		for _, g := range subAgentGlyphs {
			if strings.Contains(row, g) {
				t.Fatalf("session %q row leaked sub-agent glyph %q: %q", title, g, row)
			}
		}
	}
}

// TestSessionBusyWithSubAgentGlyphsDisjoint covers the central #236 scenario: a
// session coordinating a WORKING sub-agent. The session row shows its own ● marker
// (busy == a turn is in flight) while the nested sub-agent row shows the lifecycle
// ▶ — and the two glyph systems never bleed into each other. This is exactly the
// "the triangle lives on the sub-agent, not the session row" target representation.
func TestSessionBusyWithSubAgentGlyphsDisjoint(t *testing.T) {
	w := newTestWorkbench(t)
	w.sidebar.addSession("s1", "Coordinator", false)
	// A sub-agent is running underneath the session.
	w.sidebar.applySubAgent("s1", subAgentEvent("w1", "worker", agent.StatusRunning))
	// The session itself is busy (a turn is in flight), so it must read ●.
	w.sidebar.setBusy("s1", "Coordinator", false, true)

	rows := renderSidebarRows(t, w, tv.Rect{X: 48, Y: 1, W: 32, H: 24})

	sIdx := rowWith(rows, "Coordinator")
	if sIdx < 0 {
		t.Fatalf("session row missing:\n%s", joinRows(rows))
	}
	sessionRow := rows[sIdx]
	if !strings.Contains(sessionRow, activeMark) {
		t.Errorf("coordinating session should show ●: %q", sessionRow)
	}
	if strings.Contains(sessionRow, idleMark) {
		t.Errorf("coordinating session should not show ○: %q", sessionRow)
	}
	// The session row must NOT carry the sub-agent triangle (or any lifecycle glyph).
	for _, g := range subAgentGlyphs {
		if strings.Contains(sessionRow, g) {
			t.Errorf("session row leaked sub-agent glyph %q: %q", g, sessionRow)
		}
	}

	// The nested sub-agent row shows the triangle — and never a session marker.
	aIdx := rowWith(rows, "worker")
	if aIdx < 0 {
		t.Fatalf("sub-agent row missing:\n%s", joinRows(rows))
	}
	agentRow := rows[aIdx]
	if !strings.Contains(agentRow, "▶") {
		t.Errorf("working sub-agent should show ▶: %q", agentRow)
	}
	if strings.Contains(agentRow, activeMark) || strings.Contains(agentRow, idleMark) {
		t.Errorf("sub-agent row leaked a session marker (●/○): %q", agentRow)
	}
}

// --- #230: the header renders no global count -------------------------------

// headerHasNoGlobalCount renders the panel and asserts the title row carries no
// aggregate "needs attention" counter — no ❓/⏳ badge and no trailing count digit.
// This is the holistic #230 fix: the phantom glyph-less header number is gone, so
// the header can never indicate a session that no row identifies.
func headerHasNoGlobalCount(t *testing.T, w *Workbench, rect tv.Rect) {
	t.Helper()
	row := headerRow(t, w, rect)
	if strings.Contains(row, clarifyBadge) {
		t.Errorf("header row shows the ❓ clarify indicator (global counter not removed): %q", row)
	}
	if strings.Contains(row, approvalBadge) {
		t.Errorf("header row shows the ⏳ approval indicator (global counter not removed): %q", row)
	}
	// The title "Sessions & Agents" has no digits; a stray digit is the phantom count.
	for _, r := range strings.TrimSpace(row) {
		if r >= '0' && r <= '9' {
			t.Errorf("header row carries a count digit (phantom global counter): %q", row)
			break
		}
	}
	if !strings.Contains(row, "Sessions") {
		t.Errorf("header title clobbered: %q", row)
	}
}

// TestHeaderNoGlobalCountBaseline asserts the header is just the title — no count —
// when nothing needs attention.
func TestHeaderNoGlobalCountBaseline(t *testing.T) {
	w := newTestWorkbench(t)
	headerHasNoGlobalCount(t, w, tv.Rect{X: 48, Y: 1, W: 32, H: 24})
}

// TestHeaderNoGlobalCountWhenSessionsNeedAttention is the decisive #230 test: even
// with sessions that DO need input (clarify/approval armed), the HEADER stays
// count-free — attention is shown only per row. Previously the header would show a
// glyph-less digit here while no row could identify the session.
func TestHeaderNoGlobalCountWhenSessionsNeedAttention(t *testing.T) {
	w := newTestWorkbench(t)
	w.sidebar.addSession("a", "Alpha", false)
	w.sidebar.addSession("b", "Bravo", false)
	w.sidebar.setClarify("a", "Alpha", false, true)
	w.sidebar.setApproval("b", "Bravo", false, true)
	w.sidebar.setBusy("b", "Bravo", false, true)

	rows := renderSidebarRows(t, w, tv.Rect{X: 48, Y: 1, W: 32, H: 24})

	// Header (row 0) must be clean of any aggregate count.
	headerHasNoGlobalCount(t, w, tv.Rect{X: 48, Y: 1, W: 32, H: 24})

	// Yet each needing-attention session is marked INDIVIDUALLY on its own row:
	// Alpha carries ❓, Bravo carries ⏳ and the ● active marker.
	if idx := rowWith(rows, "Alpha"); idx < 0 || !strings.Contains(rows[idx], clarifyBadge) {
		t.Fatalf("Alpha should be individually badged with ❓:\n%s", joinRows(rows))
	}
	if idx := rowWith(rows, "Bravo"); idx < 0 {
		t.Fatalf("Bravo not rendered:\n%s", joinRows(rows))
	} else {
		if !strings.Contains(rows[idx], approvalBadge) {
			t.Errorf("Bravo should be individually badged with ⏳: %q", rows[idx])
		}
		// The row is prefixed by the panel divider + tree indent, so check the marker
		// is present (and the idle marker is not) rather than a prefix match.
		if !strings.Contains(rows[idx], activeMark) {
			t.Errorf("Bravo should carry the ● active marker: %q", rows[idx])
		}
		if strings.Contains(rows[idx], idleMark) {
			t.Errorf("Bravo should not carry the ○ idle marker: %q", rows[idx])
		}
	}
}

// TestHeaderNoGlobalCountAcrossWidths checks the header is count-free at both the
// default and the minimum draggable sidebar width. The old right-aligned counter was
// width-sensitive (it could collapse to a lone digit at narrow widths — exactly the
// #230 phantom), so both widths must now be clean.
func TestHeaderNoGlobalCountAcrossWidths(t *testing.T) {
	w := newTestWorkbench(t)
	w.sidebar.addSession("a", "Alpha", false)
	w.sidebar.setClarify("a", "Alpha", false, true)
	w.sidebar.setApproval("a", "Alpha", false, true)

	for _, w_ := range []int{defaultSidebarWidth, minSidebarWidth} {
		rect := tv.Rect{X: 80 - w_, Y: 1, W: w_, H: 24}
		if !t.Run(widthName(w_), func(t *testing.T) {
			headerHasNoGlobalCount(t, w, rect)
		}) {
			t.FailNow()
		}
	}
}

// widthName returns a stable sub-test label for a sidebar column count.
func widthName(w int) string {
	switch w {
	case defaultSidebarWidth:
		return "default"
	case minSidebarWidth:
		return "min"
	default:
		return "w"
	}
}

// --- #230 regression: closing a session mid-prompt strands no phantom ------

// TestRemoveSessionLeavesNoPhantomIndicator asserts that closing a session which was
// waiting for input (clarify) or mid-turn (busy) leaves neither a badge nor any
// header count behind. With the global counter gone (#230) this is structural, but
// the test pins it so a future regression (e.g. re-introducing an aggregate) is
// caught.
func TestRemoveSessionLeavesNoPhantomIndicator(t *testing.T) {
	w := newTestWorkbench(t)
	w.sidebar.addSession("a", "Alpha", false)
	w.sidebar.addSession("b", "Bravo", false)
	w.sidebar.setClarify("a", "Alpha", false, true)
	w.sidebar.setBusy("a", "Alpha", false, true)

	// Close the needing-attention, busy session.
	w.sidebar.removeSession("a")

	if w.sidebar.clarify["a"] {
		t.Error("clarify state leaked after removeSession")
	}
	if w.sidebar.busy["a"] {
		t.Error("busy state leaked after removeSession")
	}
	if _, ok := w.sidebar.sessions["a"]; ok {
		t.Error("session node leaked after removeSession")
	}

	// Header must still be clean (no phantom count stranded by the close).
	headerHasNoGlobalCount(t, w, tv.Rect{X: 48, Y: 1, W: 32, H: 24})

	// The surviving session is unaffected and still individually marked.
	rows := renderSidebarRows(t, w, tv.Rect{X: 48, Y: 1, W: 32, H: 24})
	if idx := rowWith(rows, "Bravo"); idx < 0 {
		t.Fatalf("surviving session Bravo not rendered:\n%s", joinRows(rows))
	} else {
		if !strings.Contains(rows[idx], idleMark) {
			t.Errorf("Bravo should be idle (○): %q", rows[idx])
		}
		if strings.Contains(rows[idx], activeMark) {
			t.Errorf("Bravo should not carry the ● active marker after the close: %q", rows[idx])
		}
	}
	if rowWith(rows, "Alpha") >= 0 {
		t.Fatalf("closed session Alpha still rendered:\n%s", joinRows(rows))
	}
}

// --- waiting-for-input marker (the #207 attention funnel reused) ------------

// TestSessionWaitingForInputMarkedIndividually is the task's explicit requirement:
// a session waiting for input is marked INDIVIDUALLY. It must show the clarify ❓
// badge (and stay readable) on its own row, with no aggregate header count. Both
// the approval and clarify attention channels are covered.
func TestSessionWaitingForInputMarkedIndividually(t *testing.T) {
	s := newTestSidebar()
	s.addSession("a", "Alpha", false)
	s.addSession("b", "Bravo", false)

	// Only Alpha is waiting for input.
	s.setClarify("a", "Alpha", false, true)
	if got := s.sessions["a"].Label; !strings.Contains(got, clarifyBadge) {
		t.Fatalf("Alpha should carry the ❓ waiting-for-input badge: %q", got)
	}
	if got := s.sessions["b"].Label; strings.Contains(got, clarifyBadge) {
		t.Fatalf("Bravo should NOT carry the ❓ badge: %q", got)
	}

	// A busy + waiting-for-input session shows BOTH the ● marker and the ❓ badge.
	s.setBusy("a", "Alpha", false, true)
	if got := s.sessions["a"].Label; !strings.HasPrefix(got, activeMark) || !strings.Contains(got, clarifyBadge) {
		t.Fatalf("Alpha should be ● and badged ❓ while busy+waiting: %q", got)
	}

	// Approval is the other attention channel; it badges only its own row too.
	s.setApproval("b", "Bravo", false, true)
	if got := s.sessions["b"].Label; !strings.Contains(got, approvalBadge) {
		t.Fatalf("Bravo should carry the ⏳ approval badge: %q", got)
	}
	if got := s.sessions["a"].Label; strings.Contains(got, approvalBadge) {
		t.Fatalf("Alpha should NOT carry Bravo's ⏳ badge: %q", got)
	}
}

// --- helpers ----------------------------------------------------------------

// assertLeadingMark checks the label starts with the expected marker glyph.
func assertLeadingMark(t *testing.T, label, want, ctx string) {
	t.Helper()
	if !strings.HasPrefix(label, want) {
		t.Fatalf("%s: leading marker = %q, want %q (label %q)", ctx, firstGlyph(label), want, label)
	}
}

// firstGlyph returns the first rune of s as a string, for error messages.
func firstGlyph(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}
