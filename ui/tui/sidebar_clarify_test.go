package ui

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/agent"
)

// applyClarifyEvent drives the sidebar's clarify API directly: it calls setClarify
// (reference-counting clarifyCount), the per-session side of Workbench's clarify
// block, AFTER the per-sub-agent dedup. It is used for sidebar-layer unit tests that
// exercise setClarify in isolation. It deliberately does NOT reproduce the
// clarifyWaiting dedup (that lives on the Workbench, above the sidebar) — use
// emitSubAgentClarify for whole-flow lifecycle tests that must exercise it.
//
// (Issue #230 removed the globalClarify header counter this shim used to resync;
// attention is now shown only per row, so there is nothing global to update here.)
func applyClarifyEvent(s *sidebar, id, title string, pinned bool, status agent.AgentStatus) {
	s.setClarify(id, title, pinned, status == agent.StatusWaiting)
}

// subAgentEvent builds a SessionEventSubAgent carrying the given sub-agent
// identity and lifecycle status, matching what agent.UserSession.emitSubAgent
// produces (AgentID + Name + Status).
func subAgentEvent(agentID, name string, status agent.AgentStatus) agent.SessionEvent {
	return agent.SessionEvent{Type: agent.SessionEventSubAgent, AgentID: agentID, Name: name, Status: status}
}

// emitSubAgentClarify reproduces — verbatim — the clarify side-effects that
// Workbench.EmitSessionEvent performs for a SessionEventSubAgent (tui.go's
// clarify block). It stands in for a real EmitSessionEvent call because
// desktop.Post only drains from the running event loop, which sidebar tests
// never start, so EmitSessionEvent's closure would never run. Mirroring the
// WHOLE block (including the per-sub-agent clarifyWaiting dedup that collapses
// repeated same-state events) keeps these lifecycle tests faithful to the real
// set/clear contract — including the multi-round-CLARIFY fix.
func emitSubAgentClarify(w *Workbench, id, title string, pinned bool, ev agent.SessionEvent) {
	if w.sidebar == nil {
		return
	}
	w.sidebar.applySubAgent(id, ev)
	key := ev.AgentID
	if key == "" {
		key = id + "/" + ev.Name
	}
	waiting := ev.Status == agent.StatusWaiting
	if w.clarifyWaiting == nil {
		w.clarifyWaiting = make(map[string]bool)
	}
	if waiting != w.clarifyWaiting[key] {
		if waiting {
			w.clarifyWaiting[key] = true
		} else {
			delete(w.clarifyWaiting, key)
		}
		w.sidebar.setClarify(id, title, pinned, waiting)
	}
}

// TestClarifyBadgeConstant pins the clarify glyph and, critically, that it is
// distinct from the approval glyph — the two indicators must be told apart at a
// glance in the header and on the row.
func TestClarifyBadgeConstant(t *testing.T) {
	if clarifyBadge != "❓" {
		t.Fatalf("clarifyBadge = %q, want %q", clarifyBadge, "❓")
	}
	if clarifyBadge == approvalBadge {
		t.Fatalf("clarifyBadge must differ from approvalBadge; both are %q", clarifyBadge)
	}
}

// TestSessionLabelClarifyBadge covers the rendered session row across the
// clarify / approval / pin combinations. Invariants from issue #207:
//   - the ❓ badge is appended only when clarify is set, and it trails the title;
//   - it is appended AFTER the ⏳ approval badge, so the two never swap order;
//   - neither badge shifts the leading status icon or the ★ pin marker.
func TestSessionLabelClarifyBadge(t *testing.T) {
	// The leading glyph is the session's idle marker (○), not a sub-agent bullet —
	// see issue #236. An idle session shows ○.
	icon := sessionStatusIcon(false)
	for _, tc := range []struct {
		name          string
		pinned        bool
		pending       bool
		clarify       bool
		wantClarify   bool
		wantApproval  bool
		wantStarFirst bool
	}{
		{name: "plain"},
		{name: "clarify only", clarify: true, wantClarify: true},
		{name: "approval only", pending: true, wantApproval: true},
		{name: "clarify+approval", clarify: true, pending: true, wantClarify: true, wantApproval: true},
		{name: "pinned+clarify", pinned: true, clarify: true, wantClarify: true, wantStarFirst: true},
		{name: "pinned+clarify+approval", pinned: true, clarify: true, pending: true, wantClarify: true, wantApproval: true, wantStarFirst: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			label := sessionLabel("Session 1", false, tc.pinned, tc.pending, tc.clarify)

			if got := strings.Contains(label, clarifyBadge); got != tc.wantClarify {
				t.Fatalf("clarify badge presence = %v, want %v (label %q)", got, tc.wantClarify, label)
			}
			if got := strings.Contains(label, approvalBadge); got != tc.wantApproval {
				t.Fatalf("approval badge presence = %v, want %v (label %q)", got, tc.wantApproval, label)
			}

			// Badges trail the title and never displace the leading status icon.
			if !strings.HasPrefix(label, icon) {
				t.Fatalf("status icon %q must lead the label, got %q", icon, label)
			}
			if !strings.Contains(label, "Session 1") {
				t.Fatalf("title lost from label %q", label)
			}

			// When both badges are present, approval precedes clarify and clarify
			// is the suffix (appended LAST, per the issue).
			if tc.wantClarify {
				if !strings.HasSuffix(label, clarifyBadge) {
					t.Fatalf("clarify badge should trail the label, got %q", label)
				}
			} else if tc.wantApproval {
				if !strings.HasSuffix(label, approvalBadge) {
					t.Fatalf("approval badge should trail the label, got %q", label)
				}
			}
			if tc.wantClarify && tc.wantApproval {
				if strings.Index(label, approvalBadge) >= strings.Index(label, clarifyBadge) {
					t.Fatalf("approval must precede clarify, got %q", label)
				}
			}

			if tc.wantStarFirst && !strings.Contains(label, "★") {
				t.Fatalf("expected ★ marker in %q", label)
			}
		})
	}
}

// TestSidebarClarifyBadge is the clarify analogue of TestSidebarApprovalBadge:
// setClarify toggles the row badge, the marker survives an unrelated relabel
// (rename and a pin toggle), and it clears once the sub-agent stops waiting.
func TestSidebarClarifyBadge(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)

	node := s.sessions["s1"]
	if strings.Contains(node.Label, clarifyBadge) {
		t.Fatalf("new session should not carry the clarify badge: %q", node.Label)
	}

	s.setClarify("s1", "Session 1", false, true)
	if !strings.Contains(node.Label, clarifyBadge) {
		t.Fatalf("expected clarify badge after setClarify(waiting): %q", node.Label)
	}
	if !s.clarify["s1"] {
		t.Fatal("clarify set not recorded in the map")
	}

	// A rename must preserve the clarify badge (issue #207 acceptance).
	s.relabelSession("s1", "Renamed", false)
	if !strings.Contains(node.Label, clarifyBadge) {
		t.Fatalf("clarify badge lost across rename: %q", node.Label)
	}
	if !strings.Contains(node.Label, "Renamed") {
		t.Fatalf("relabel did not apply the new title: %q", node.Label)
	}

	// A pin toggle must also preserve the badge and surface the ★ marker.
	s.relabelSession("s1", "Renamed", true)
	if !strings.Contains(node.Label, clarifyBadge) {
		t.Fatalf("clarify badge lost across pin toggle: %q", node.Label)
	}
	if !strings.Contains(node.Label, "★") {
		t.Fatalf("pin marker missing after relabel(pinned): %q", node.Label)
	}

	// Clearing waiting removes the badge but keeps the pinned title intact.
	s.setClarify("s1", "Renamed", true, false)
	if strings.Contains(node.Label, clarifyBadge) {
		t.Fatalf("clarify badge should clear once no longer waiting: %q", node.Label)
	}
	if !strings.Contains(node.Label, "Renamed") || !strings.Contains(node.Label, "★") {
		t.Fatalf("title/pin disturbed by clearing clarify: %q", node.Label)
	}
	if s.clarify["s1"] {
		t.Fatal("clarify entry should be deleted once cleared")
	}
}

// TestSidebarClarifyApprovalIndependence verifies the two badge tracks do not
// stomp on each other: clearing one preserves the other on the live node, in
// both directions. This guards the s.approvals[id] / s.clarify[id] carry-over
// in setApproval / setClarify / relabelSession.
func TestSidebarClarifyApprovalIndependence(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)

	// Both badges on: approval is drawn first, clarify second (suffix).
	s.setApproval("s1", "Session 1", false, true)
	s.setClarify("s1", "Session 1", false, true)
	node := s.sessions["s1"]
	if !strings.Contains(node.Label, approvalBadge) || !strings.Contains(node.Label, clarifyBadge) {
		t.Fatalf("expected both badges, got %q", node.Label)
	}
	if !strings.HasSuffix(node.Label, clarifyBadge) {
		t.Fatalf("clarify must be the trailing badge, got %q", node.Label)
	}

	// Clearing approval must leave clarify on the row.
	s.setApproval("s1", "Session 1", false, false)
	node = s.sessions["s1"]
	if strings.Contains(node.Label, approvalBadge) {
		t.Fatalf("approval should be cleared: %q", node.Label)
	}
	if !strings.Contains(node.Label, clarifyBadge) {
		t.Fatalf("clarify must survive clearing approval: %q", node.Label)
	}

	// Re-arm approval, then clear clarify; approval must remain.
	s.setApproval("s1", "Session 1", false, true)
	s.setClarify("s1", "Session 1", false, false)
	node = s.sessions["s1"]
	if !strings.Contains(node.Label, approvalBadge) {
		t.Fatalf("approval must survive clearing clarify: %q", node.Label)
	}
	if strings.Contains(node.Label, clarifyBadge) {
		t.Fatalf("clarify should be cleared: %q", node.Label)
	}
}

// TestSidebarRemoveClearsClarify ensures a closed session does not leak its
// clarify state into the badge set (parallel to TestSidebarRemoveClearsApproval).
func TestSidebarRemoveClearsClarify(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.setClarify("s1", "Session 1", false, true)
	s.removeSession("s1")
	if s.clarify["s1"] {
		t.Fatal("clarify state leaked after removeSession")
	}
}

// TestSidebarGlobalClarify was removed: it exercised setGlobalClarify /
// globalClarify, which issue #230 deleted along with the phantom glyph-less header
// counter. The #230 regression coverage (header renders no aggregate count) now
// lives in sidebar_busy_test.go (TestHeaderNoGlobalCount*).

// TestSidebarClarifyOutOfOrder covers the "intent recorded before the node
// exists" path: a SessionEventSubAgent(StatusWaiting) may arrive before the
// session window is registered. The clarify set must still record it so the
// badge appears once addSession lands — the same robustness the approval track
// relies on.
func TestSidebarClarifyOutOfOrder(t *testing.T) {
	s := newTestSidebar()
	// Event arrives before any node exists for the session.
	s.setClarify("late", "Late", false, true)
	if !s.clarify["late"] {
		t.Fatal("clarify intent should be recorded even with no node yet")
	}
	// The session is registered later; its node picks up the badge.
	s.addSession("late", "Late", false)
	node := s.sessions["late"]
	if node == nil {
		t.Fatal("addSession did not register the node")
	}
	if !strings.Contains(node.Label, clarifyBadge) {
		t.Fatalf("out-of-order clarify badge should surface on add: %q", node.Label)
	}
}

// TestClarifyLifecycle asserts the set/clear contract for a single sub-agent,
// driven through the same two-line shim EmitSessionEvent runs: StatusWaiting
// arms the badge and bumps the global count; every other lifecycle status
// clears it.
func TestClarifyLifecycle(t *testing.T) {
	clearStatuses := []struct {
		name   string
		status agent.AgentStatus
	}{
		{"running clears", agent.StatusRunning},
		{"completed clears", agent.StatusCompleted},
		{"failed clears", agent.StatusFailed},
		{"idle clears", agent.StatusIdle},
	}
	for _, cs := range clearStatuses {
		cs := cs
		t.Run(cs.name, func(t *testing.T) {
			s := newTestSidebar()
			s.addSession("s1", "Session 1", false)

			applyClarifyEvent(s, "s1", "Session 1", false, agent.StatusWaiting)
			if !s.clarify["s1"] {
				t.Fatalf("%s: badge not set on StatusWaiting", cs.name)
			}
			node := s.sessions["s1"]
			if !strings.Contains(node.Label, clarifyBadge) {
				t.Fatalf("%s: row badge missing after waiting", cs.name)
			}

			applyClarifyEvent(s, "s1", "Session 1", false, cs.status)
			if s.clarify["s1"] {
				t.Fatalf("%s: badge should clear on %s", cs.name, cs.status)
			}
			node = s.sessions["s1"]
			if strings.Contains(node.Label, clarifyBadge) {
				t.Fatalf("%s: row badge should clear on %s: %q", cs.name, cs.status, node.Label)
			}
		})
	}
}

// TestClarifyFlaggedSessions tracks WHICH sessions are currently flagged (the
// per-session ❓ membership a row reads), not the number of waiting events: each
// session shows at most one badge, and one resuming clears only its own flag while
// the other stays flagged. (Issue #230 removed the aggregate global count this test
// used to assert; the per-session membership is the surviving invariant.)
func TestClarifyFlaggedSessions(t *testing.T) {
	s := newTestSidebar()
	s.addSession("a", "A", false)
	s.addSession("b", "B", false)

	applyClarifyEvent(s, "a", "A", false, agent.StatusWaiting)
	if !s.clarify["a"] || s.clarify["b"] {
		t.Fatalf("only a should be flagged after first session waits (a=%v b=%v)", s.clarify["a"], s.clarify["b"])
	}
	applyClarifyEvent(s, "b", "B", false, agent.StatusWaiting)
	if !s.clarify["a"] || !s.clarify["b"] {
		t.Fatalf("both sessions should be flagged after second session waits (a=%v b=%v)", s.clarify["a"], s.clarify["b"])
	}
	// A second sub-agent event for an already-flagged session keeps it flagged once
	// (a session shows at most one badge) — clarifyCount rises but membership stays.
	applyClarifyEvent(s, "a", "A", false, agent.StatusWaiting)
	if !s.clarify["a"] {
		t.Fatal("session a should still be flagged once after a repeat waiting event")
	}

	// b resumes; a is still waiting, so only b's flag clears.
	applyClarifyEvent(s, "b", "B", false, agent.StatusRunning)
	if s.clarify["b"] {
		t.Fatal("session b should be un-flagged after it resumes")
	}
	if !s.clarify["a"] {
		t.Fatal("session a should still be flagged while its sub-agent waits")
	}
}

// TestClarifyBadgePreservesPinned verifies the clarify event path threads the
// pinned flag through, so a waiting pinned session shows both ★ and ❓.
func TestClarifyBadgePreservesPinned(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", true)
	applyClarifyEvent(s, "s1", "Session 1", true, agent.StatusWaiting)
	node := s.sessions["s1"]
	if !strings.Contains(node.Label, "★") {
		t.Fatalf("pinned marker lost through clarify event: %q", node.Label)
	}
	if !strings.Contains(node.Label, clarifyBadge) {
		t.Fatalf("clarify badge missing: %q", node.Label)
	}
}

// --- header render-path tests (drive the real panel.DrawFn via the desktop) ---

// headerRow renders the sidebar panel at rect and returns the title row (row 0)
// as a string of one rune per column, where a wide/empty cell becomes a space.
func headerRow(t *testing.T, w *Workbench, rect tv.Rect) string {
	t.Helper()
	rows := renderSidebarRows(t, w, rect)
	if len(rows) == 0 {
		t.Fatal("no rows rendered")
	}
	return rows[0]
}

// TestClarifyHeaderRender is a #230 regression test on the header render path.
// With a session waiting for input, the header must show NO ❓ indicator and no
// count digit (the global counter is gone) — but the waiting session's own ROW
// still carries its individual ❓ badge. The panel is positioned at Y:1, matching
// sidebar.reposition, to leave the menubar's row 0 alone.
func TestClarifyHeaderRender(t *testing.T) {
	w := newTestWorkbench(t)
	w.sidebar.addSession("a", "Alpha", false)
	w.sidebar.setClarify("a", "Alpha", false, true)

	rows := renderSidebarRows(t, w, tv.Rect{X: 48, Y: 1, W: 32, H: 24})
	row := rows[0]

	if strings.Contains(row, clarifyBadge) {
		t.Fatalf("header must not show ❓ (global counter removed, #230): %q", row)
	}
	if strings.Contains(row, approvalBadge) {
		t.Fatalf("header must not show ⏳: %q", row)
	}
	for _, r := range strings.TrimSpace(row) {
		if r >= '0' && r <= '9' {
			t.Fatalf("header carries a phantom count digit (#230): %q", row)
		}
	}
	if !strings.Contains(row, "Sessions") {
		t.Fatalf("header title clobbered: %q", row)
	}
	// The waiting session is still marked INDIVIDUALLY on its own row.
	if idx := rowWith(rows, "Alpha"); idx < 0 || !strings.Contains(rows[idx], clarifyBadge) {
		t.Fatalf("Alpha should carry its own ❓ badge:\n%s", joinRows(rows))
	}
}

// TestClarifyHeaderRenderBoth is a #230 regression test at the default width: with
// BOTH a pending approval and a waiting clarify armed on their own rows, the header
// shows NEITHER glyph and no count — attention is per-row only. (Pre-#230 this is
// exactly the configuration that produced the phantom glyph-less header digit.)
func TestClarifyHeaderRenderBoth(t *testing.T) {
	w := newTestWorkbench(t)
	w.sidebar.addSession("a", "Alpha", false)
	w.sidebar.addSession("b", "Bravo", false)
	w.sidebar.setApproval("a", "Alpha", false, true)
	w.sidebar.setClarify("b", "Bravo", false, true)

	rows := renderSidebarRows(t, w, tv.Rect{X: 48, Y: 1, W: 32, H: 24})
	row := rows[0]

	if strings.Contains(row, approvalBadge) {
		t.Errorf("header must not show ⏳ (#230): %q", row)
	}
	if strings.Contains(row, clarifyBadge) {
		t.Errorf("header must not show ❓ (#230): %q", row)
	}
	// Both badges must still appear on their respective rows.
	if idx := rowWith(rows, "Alpha"); idx < 0 || !strings.Contains(rows[idx], approvalBadge) {
		t.Errorf("Alpha row missing its ⏳ badge:\n%s", joinRows(rows))
	}
	if idx := rowWith(rows, "Bravo"); idx < 0 || !strings.Contains(rows[idx], clarifyBadge) {
		t.Errorf("Bravo row missing its ❓ badge:\n%s", joinRows(rows))
	}
}

// TestClarifyHeaderNoOverlapAtMinWidth keeps the min-width render path covered
// after #230. At the minimum draggable sidebar width there is no longer any header
// indicator to overlap, so this simply asserts the header stays count-free (no
// glyph and no lone digit) even at the narrowest width with both attention channels
// armed — the configuration that previously collapsed to a phantom digit.
func TestClarifyHeaderNoOverlapAtMinWidth(t *testing.T) {
	w := newTestWorkbench(t)
	w.sidebar.addSession("a", "Alpha", false)
	w.sidebar.setApproval("a", "Alpha", false, true)
	w.sidebar.setClarify("a", "Alpha", false, true)

	row := headerRow(t, w, tv.Rect{X: 80 - minSidebarWidth, Y: 1, W: minSidebarWidth, H: 24})

	if strings.Contains(row, approvalBadge) {
		t.Errorf("approval glyph ⏳ leaked into header at min width (#230): %q", row)
	}
	if strings.Contains(row, clarifyBadge) {
		t.Errorf("clarify glyph ❓ leaked into header at min width (#230): %q", row)
	}
	for _, r := range strings.TrimSpace(row) {
		if r >= '0' && r <= '9' {
			t.Errorf("phantom count digit at min width (#230): %q", row)
			break
		}
	}
}

// TestClarifyBadgeMultipleSubAgentsOneSession drives the real EmitSessionEvent
// clarify flow (via emitSubAgentClarify) with two DISTINCT sub-agents in one
// session. The ❓ badge must persist while either is StatusWaiting and clear only
// when the last one resolves. This also confirms the per-sub-agent clarifyWaiting
// dedup keys by AgentID, so two different agents each count once.
func TestClarifyBadgeMultipleSubAgentsOneSession(t *testing.T) {
	w := newTestWorkbench(t)
	w.sidebar.addSession("s1", "Session 1", false)

	// Sub-agent A goes waiting (asks a CLARIFY question).
	emitSubAgentClarify(w, "s1", "Session 1", false, subAgentEvent("A", "asker", agent.StatusWaiting))
	if !w.sidebar.clarify["s1"] {
		t.Fatalf("session should be flagged after first sub-agent waits (clarifyCount=%d)", w.sidebar.clarifyCount["s1"])
	}

	// Sub-agent B goes waiting too; the session is still flagged once.
	emitSubAgentClarify(w, "s1", "Session 1", false, subAgentEvent("B", "helper", agent.StatusWaiting))
	if !w.sidebar.clarify["s1"] {
		t.Fatalf("session should remain flagged once with two waiting sub-agents (clarifyCount=%d)", w.sidebar.clarifyCount["s1"])
	}
	if w.sidebar.clarifyCount["s1"] != 2 {
		t.Fatalf("clarifyCount=%d, want 2 (two distinct waiting sub-agents)", w.sidebar.clarifyCount["s1"])
	}

	// Sub-agent A resolves while B is STILL waiting. The badge must remain.
	emitSubAgentClarify(w, "s1", "Session 1", false, subAgentEvent("A", "asker", agent.StatusCompleted))
	if !w.sidebar.clarify["s1"] {
		t.Fatalf("clarify badge cleared while another sub-agent is still waiting (clarifyCount=%d)", w.sidebar.clarifyCount["s1"])
	}
	if w.sidebar.clarifyCount["s1"] != 1 {
		t.Fatalf("clarifyCount=%d, want 1 (one sub-agent is still waiting)", w.sidebar.clarifyCount["s1"])
	}

	// B resolves too: badge clears, count back to 0.
	emitSubAgentClarify(w, "s1", "Session 1", false, subAgentEvent("B", "helper", agent.StatusCompleted))
	if w.sidebar.clarify["s1"] {
		t.Fatalf("clarify badge should clear once the last waiting sub-agent resolves (clarifyCount=%d)", w.sidebar.clarifyCount["s1"])
	}
	if w.sidebar.clarifyCount["s1"] != 0 {
		t.Fatalf("clarifyCount=%d, want 0 after all sub-agents resolved", w.sidebar.clarifyCount["s1"])
	}
}

// TestSidebarClarifyNoStaleGlobalAfterRemove is a regression test rooted in #230:
// closing a session that is currently waiting for input must not strand any
// attention indicator. With the global header counter gone (#230) there is no
// aggregate to go stale, but removeSession must still wipe the per-session clarify
// membership and its reference count so the closed session cannot keep a row-badge
// ghost alive. (The pre-#230 version asserted globalClarify here; the per-session
// state is the surviving invariant.)
func TestSidebarClarifyNoStaleGlobalAfterRemove(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	applyClarifyEvent(s, "s1", "Session 1", false, agent.StatusWaiting)
	if !s.clarify["s1"] {
		t.Fatalf("clarify=%v, want true while the session waits", s.clarify["s1"])
	}

	// User closes the waiting session: the per-session clarify state must clear.
	s.removeSession("s1")
	if s.clarify["s1"] {
		t.Fatal("clarify state leaked after removeSession")
	}
	if s.clarifyCount["s1"] != 0 {
		t.Errorf("clarifyCount leaked: %d", s.clarifyCount["s1"])
	}
}

// TestClarifyBadgeMultiRoundSingleSubAgent is a regression test for the
// clarifyWaiting dedup in Workbench.EmitSessionEvent.
//
// Agent.SetStatus does not emit; only the explicit emitSubAgent calls do. In
// runInteractive, a sub-agent that asks CLARIFY more than once (the primary use
// case for interactive agents) emits: Running (launch) → Waiting (CLARIFY #1)
// → Waiting (CLARIFY #2) → … → Completed. The resume between rounds (the loop's
// top-of-round SetStatus(StatusRunning) after the coordinator answers) is NEVER
// emitted, so a naive per-session event counter would see two consecutive
// StatusWaiting increments with no balancing decrement and inflate, leaving the
// ❓ badge stuck ON after completion.
//
// EmitSessionEvent collapses repeated same-state events per sub-agent
// (clarifyWaiting[key]), so the second StatusWaiting is a no-op and the count
// stays balanced. This replays that exact sequence through the real flow
// (emitSubAgentClarify) and asserts the badge clears on completion.
func TestClarifyBadgeMultiRoundSingleSubAgent(t *testing.T) {
	w := newTestWorkbench(t)
	w.sidebar.addSession("s1", "Session 1", false)

	// Faithful replay of runInteractive's emitSubAgent sequence for one
	// interactive sub-agent that CLARIFIES twice then completes. The resume
	// between the two CLARIFY rounds is never emitted (no Running event lands).
	emitSubAgentClarify(w, "s1", "Session 1", false, subAgentEvent("A", "asker", agent.StatusRunning)) // launch
	emitSubAgentClarify(w, "s1", "Session 1", false, subAgentEvent("A", "asker", agent.StatusWaiting)) // CLARIFY #1
	emitSubAgentClarify(w, "s1", "Session 1", false, subAgentEvent("A", "asker", agent.StatusWaiting)) // CLARIFY #2 (resume not emitted)

	if !w.sidebar.clarify["s1"] {
		t.Fatalf("session should be flagged while the sub-agent waits (clarifyCount=%d)", w.sidebar.clarifyCount["s1"])
	}
	if w.sidebar.clarifyCount["s1"] != 1 {
		t.Fatalf("dedup should keep clarifyCount at 1 for one waiting sub-agent, got %d", w.sidebar.clarifyCount["s1"])
	}

	// The sub-agent finishes. The badge must clear (count balanced back to 0).
	emitSubAgentClarify(w, "s1", "Session 1", false, subAgentEvent("A", "asker", agent.StatusCompleted)) // finish

	if w.sidebar.clarify["s1"] {
		t.Fatalf("clarify badge stuck ON after a multi-round CLARIFY sub-agent completed (clarifyCount=%d)", w.sidebar.clarifyCount["s1"])
	}
	if w.sidebar.clarifyCount["s1"] != 0 {
		t.Fatalf("clarifyCount=%d, want 0 after the sub-agent completed", w.sidebar.clarifyCount["s1"])
	}
}

// TestClarifyWaitingDedupIdempotentReArm confirms a single sub-agent that waits,
// resolves, then waits again (a fresh CLARIFY after a resumed round) re-arms the
// badge exactly once each time — the dedup must not permanently latch, nor
// double-count the second waiting.
func TestClarifyWaitingDedupIdempotentReArm(t *testing.T) {
	w := newTestWorkbench(t)
	w.sidebar.addSession("s1", "Session 1", false)
	ev := func(status agent.AgentStatus) agent.SessionEvent {
		return subAgentEvent("A", "asker", status)
	}

	emitSubAgentClarify(w, "s1", "Session 1", false, ev(agent.StatusWaiting))
	if w.sidebar.clarifyCount["s1"] != 1 {
		t.Fatalf("first wait: clarifyCount=%d, want 1", w.sidebar.clarifyCount["s1"])
	}
	// Resumed round (a non-waiting event lands), then a fresh CLARIFY.
	emitSubAgentClarify(w, "s1", "Session 1", false, ev(agent.StatusRunning))
	if w.sidebar.clarify["s1"] {
		t.Fatalf("badge should clear on resume (clarifyCount=%d)", w.sidebar.clarifyCount["s1"])
	}
	emitSubAgentClarify(w, "s1", "Session 1", false, ev(agent.StatusWaiting))
	if w.sidebar.clarifyCount["s1"] != 1 {
		t.Fatalf("re-arm: clarifyCount=%d, want 1 (dedup must allow re-arm)", w.sidebar.clarifyCount["s1"])
	}
	emitSubAgentClarify(w, "s1", "Session 1", false, ev(agent.StatusCompleted))
	if w.sidebar.clarify["s1"] {
		t.Fatalf("badge should clear after completion (clarifyCount=%d)", w.sidebar.clarifyCount["s1"])
	}
}
