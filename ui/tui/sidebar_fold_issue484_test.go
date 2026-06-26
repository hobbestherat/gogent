package ui

// Tests for issue #484: TTL-based folding of finished sub-agents into a per-
// session "finished" bucket, plus a persistent per-session status bar. All fold/
// TTL/status-bar state lives in the sidebar UI mirror (sidebar.go); the shared
// agent tree, server and remote handlers are untouched.
//
// The TTL clock is injected (sidebar.now / sidebar.ttl) so no test sleeps the
// real 60s. Production defaults are asserted separately.

import (
	"strings"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/config"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// --- production defaults ----------------------------------------------------

// TestFoldProductionDefaults pins the shipped clock + TTL so a "tidy" cannot
// silently change the user-visible 60s behaviour.
func TestFoldProductionDefaults(t *testing.T) {
	s := newTestSidebar()
	if s.ttl != subAgentFoldTTL {
		t.Fatalf("ttl = %v, want %v", s.ttl, subAgentFoldTTL)
	}
	if subAgentFoldTTL != 60*time.Second {
		t.Fatalf("subAgentFoldTTL = %v, want 60s", subAgentFoldTTL)
	}
	if s.now == nil {
		t.Fatal("now clock must be initialised (time.Now in production)")
	}
}

// --- test helpers -----------------------------------------------------------

// clock is an injectable sidebar clock: advance it to age out a fold without
// sleeping.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

// newFoldSidebar builds a detached sidebar with a controllable clock and a tiny
// TTL so folds are fast and deterministic.
func newFoldSidebar(t *testing.T) (*sidebar, *clock) {
	t.Helper()
	s := newTestSidebar()
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	s.now = c.now
	s.ttl = 5 * time.Second
	return s, c
}

// subEv builds a SessionEventSubAgent lifecycle event.
func subEv(agentID, name string, status agent.AgentStatus) agent.SessionEvent {
	return agent.SessionEvent{Type: agent.SessionEventSubAgent, AgentID: agentID, Name: name, Status: status}
}

// countNodeRefChildren counts real (nodeRef) children of a node, ignoring the
// synthetic status-bar / bucket nodes added by issue #484.
func countNodeRefChildren(n *tv.TreeNode) int {
	count := 0
	for _, c := range n.Children {
		if _, ok := c.Data.(nodeRef); ok {
			count++
		}
	}
	return count
}

// foldOf returns the session's fold bookkeeping (nil if none).
func foldOf(s *sidebar, id string) *sessionFold { return s.folds[id] }

// hasRowContaining reports whether any rendered sidebar row contains sub.
func hasRowContaining(rows []string, sub string) bool {
	for _, r := range rows {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

// --- S1: status bar created + pinned; bucket absent while nothing folded -----

// TestFold_StatusBarCreatedAndPinned verifies the per-session status bar is
// created as child[0] on the first sub-agent, is a synthetic (inert) leaf, shows
// the live counts, and that NO finished bucket exists yet (empty-bucket rule).
func TestFold_StatusBarCreatedAndPinned(t *testing.T) {
	s, _ := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)

	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusRunning))

	parent := s.sessions["s1"]
	if fold := foldOf(s, "s1"); fold == nil || fold.statusNode == nil {
		t.Fatalf("expected a sessionFold with a status bar after a sub-agent")
	} else if fold.bucketNode != nil {
		t.Fatalf("bucket must be absent while nothing is folded (empty-bucket rule), got %v", fold.bucketNode.Label)
	}
	// Status bar pinned at child[0].
	if parent.Children[0] != foldOf(s, "s1").statusNode {
		t.Fatalf("status bar must be child[0]")
	}
	// Status bar is a synthetic leaf (inert to select/activate).
	if _, ok := parent.Children[0].Data.(syntheticRef); !ok {
		t.Fatalf("status bar Data must be syntheticRef, got %T", parent.Children[0].Data)
	}
	if len(parent.Children[0].Children) != 0 {
		t.Fatalf("status bar must be a leaf (blank marker column)")
	}
	// Live counts rendered with the running glyph.
	if g := parent.Children[0].Label; !strings.Contains(g, "▶") || !strings.Contains(g, "1") {
		t.Fatalf("status bar label = %q, want it to show ▶1", g)
	}
	// The real agent row sits after the status bar.
	if countNodeRefChildren(parent) != 1 {
		t.Fatalf("want 1 real agent child, got %d", countNodeRefChildren(parent))
	}
}

// TestFold_NoStatusBarWithoutSubAgent ensures a session with only watchers (no
// sub-agents) gets no status bar / fold bookkeeping.
func TestFold_NoStatusBarWithoutSubAgent(t *testing.T) {
	s, _ := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	// Attach a watcher only.
	s.setWatchers(nil, map[string][]WatcherInfo{"s1": {attInfo("w1", "gh", "s1", false)}})
	if foldOf(s, "s1") != nil {
		t.Fatalf("a watcher-only session must not get fold bookkeeping")
	}
	if len(s.sessions["s1"].Children) != 1 {
		t.Fatalf("watcher-only session should have exactly the watcher child, got %d", len(s.sessions["s1"].Children))
	}
}

// --- TTL fold timing (core acceptance criterion) ----------------------------

// TestFold_CompletedVisibleWithinTTL: before the TTL elapses a completed
// sub-agent stays a normal visible child row and no bucket appears.
func TestFold_CompletedVisibleWithinTTL(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))

	parent := s.sessions["s1"]
	node := s.agents["a1"]
	// One tick at 4s (< 5s TTL): nothing folds.
	c.add(4 * time.Second)
	if changed := s.tickFolds(); changed {
		t.Fatalf("tickFolds should report no change within TTL, got changed=true")
	}
	if !isChildOf(parent, node) {
		t.Fatalf("completed agent must still be a visible child within TTL")
	}
	if isChildOf(parent, foldOf(s, "s1").bucketNode) { // bucketNode is nil -> isChildOf false
		t.Fatalf("no bucket should exist within TTL")
	}
	if foldOf(s, "s1").bucketNode != nil {
		t.Fatalf("bucketNode must be nil within TTL")
	}
	// Status bar still counts the (visible) completed agent.
	if g := foldOf(s, "s1").statusNode.Label; !strings.Contains(g, "✓") {
		t.Fatalf("status bar should include the completed agent in the ✓ count: %q", g)
	}
}

// TestFold_FoldsAfterTTL: once the TTL elapses, tickFolds moves the completed
// agent under a collapsed bucket pinned at child[1] and keeps it in the ✓ count.
func TestFold_FoldsAfterTTL(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))

	parent := s.sessions["s1"]
	node := s.agents["a1"]

	c.add(5 * time.Second) // exactly TTL -> folds (>=)
	changed := s.tickFolds()
	if !changed {
		t.Fatalf("tickFolds should report a change once TTL elapsed")
	}
	fold := foldOf(s, "s1")
	if fold.bucketNode == nil {
		t.Fatalf("bucket should be attached after a fold")
	}
	if fold.bucketNode.Expanded {
		t.Fatalf("bucket must be collapsed by default once non-empty")
	}
	if !isChildOf(fold.bucketNode, node) {
		t.Fatalf("completed agent should be moved under the bucket")
	}
	if isChildOf(parent, node) {
		t.Fatalf("completed agent should no longer be a visible child of the session")
	}
	// Bucket pinned immediately after the status bar (child[1]).
	if parent.Children[1] != fold.bucketNode {
		t.Fatalf("bucket must be child[1], immediately after the status bar")
	}
	// Bucket label reflects the folded count.
	if g := fold.bucketNode.Label; !strings.Contains(g, "✓") || !strings.Contains(g, "1") {
		t.Fatalf("bucket label = %q, want [✓ 1]", g)
	}
	// Status bar ✓ count still includes the folded agent.
	if g := fold.statusNode.Label; !strings.Contains(g, "✓") {
		t.Fatalf("status bar must still count folded agents in ✓: %q", g)
	}
	// A second sweep is a no-op (idempotent), so no extra redraw is forced.
	if s.tickFolds() {
		t.Fatalf("a second tickFolds should be a no-op once everything is folded")
	}
}

// TestFold_TTLMeasuredFromDelivery ensures the TTL clock starts at the moment
// the completion event is delivered, not at session creation.
func TestFold_TTLMeasuredFromDelivery(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	// Let wall time pass before the completion arrives.
	c.add(30 * time.Second)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	// Only 2s after delivery: must still be visible.
	c.add(2 * time.Second)
	s.tickFolds()
	if fold := foldOf(s, "s1"); fold.bucketNode != nil {
		t.Fatalf("agent must not fold before TTL measured from delivery has elapsed")
	}
	// Past TTL measured from delivery: folds.
	c.add(5 * time.Second)
	s.tickFolds()
	if fold := foldOf(s, "s1"); fold.bucketNode == nil {
		t.Fatalf("agent should fold once TTL-from-delivery elapses")
	}
}

// TestFold_DuplicateCompletionDoesNotResetTTL: a repeated StatusCompleted event
// for the same agent must not restart the 60s clock.
func TestFold_DuplicateCompletionDoesNotResetTTL(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	first := foldOf(s, "s1").entries["a1"].finishedAt

	c.add(2 * time.Second)
	// A duplicate completion arrives: must not reset finishedAt.
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	if got := foldOf(s, "s1").entries["a1"].finishedAt; !got.Equal(first) {
		t.Fatalf("duplicate completion must not reset finishedAt: was %v now %v", first, got)
	}
	// And must not double-add the node under the bucket.
	c.add(5 * time.Second)
	s.tickFolds()
	if n := len(foldOf(s, "s1").bucketNode.Children); n != 1 {
		t.Fatalf("duplicate completion must not add a second bucket child, got %d", n)
	}
}

// --- Failed agents: never auto-fold + manual dismiss ------------------------

// TestFold_FailedNeverAutoFolds: a failed agent stays a visible child across
// ticks past the TTL (only StatusCompleted folds) and shows in the ✗ count.
func TestFold_FailedNeverAutoFolds(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "boom", agent.StatusFailed))

	parent := s.sessions["s1"]
	node := s.agents["a1"]

	c.add(1 * time.Hour) // way past TTL
	s.tickFolds()

	if !isChildOf(parent, node) {
		t.Fatalf("failed agent must stay a visible child (never auto-folded)")
	}
	if fold := foldOf(s, "s1"); fold.bucketNode != nil {
		t.Fatalf("no bucket should be created for a failed-only session")
	}
	if g := foldOf(s, "s1").statusNode.Label; !strings.Contains(g, "✗") {
		t.Fatalf("status bar should count the failed agent in ✗: %q", g)
	}
}

// TestFold_DismissFailedClearsRow: dismissFailed removes every undismissed failed
// agent of a session, drops its ✗ count, and tears down the chrome when none
// remain.
func TestFold_DismissFailedClearsRow(t *testing.T) {
	s, _ := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "boom1", agent.StatusFailed))
	s.applySubAgent("s1", subEv("a2", "run", agent.StatusRunning))
	parent := s.sessions["s1"]

	s.dismissFailed("s1")

	if _, ok := s.agents["a1"]; ok {
		t.Fatalf("dismissed failed agent must be removed from s.agents")
	}
	if isChildOf(parent, s.agents["a1"]) {
		t.Fatalf("dismissed failed agent row must be removed from the tree")
	}
	// The running agent is untouched.
	if _, ok := s.agents["a2"]; !ok {
		t.Fatalf("dismiss must not touch non-failed agents")
	}
	// ✗ count is now 0; the bar still shows the running agent (so it stays).
	if g := foldOf(s, "s1").statusNode.Label; strings.Contains(g, "✗") {
		t.Fatalf("status bar should no longer show ✗ after dismiss: %q", g)
	}
}

// TestFold_DismissFailedTearsDownWhenNoAgentsRemain: dismissing the only agent
// (a failure) returns the session row to its clean pre-agent state.
func TestFold_DismissFailedTearsDownWhenNoAgentsRemain(t *testing.T) {
	s, _ := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "boom", agent.StatusFailed))

	s.dismissFailed("s1")

	if foldOf(s, "s1") != nil {
		t.Fatalf("fold bookkeeping should be torn down when no agents remain")
	}
	if n := len(s.sessions["s1"].Children); n != 0 {
		t.Fatalf("session row should be clean after dismissing the only agent, got %d children", n)
	}
}

// TestFold_DismissFailedNoOp covers the documented guards: unknown session and a
// session with no failed agents are no-ops.
func TestFold_DismissFailedNoOp(t *testing.T) {
	s, _ := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "run", agent.StatusRunning))
	before := len(s.sessions["s1"].Children)

	s.dismissFailed("ghost") // unknown session
	s.dismissFailed("s1")    // no failed agents
	if n := len(s.sessions["s1"].Children); n != before {
		t.Fatalf("dismiss should be a no-op with no failed agents: was %d now %d", before, n)
	}
}

// --- per-session independence ------------------------------------------------

// TestFold_PerSessionIndependence: folding/expanding one session's bucket does
// not touch another's nodes, expand state, or counts.
func TestFold_PerSessionIndependence(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.addSession("s2", "Session 2", false)
	s.applySubAgent("s1", subEv("a1", "w1", agent.StatusCompleted))
	s.applySubAgent("s2", subEv("b1", "w2", agent.StatusCompleted))

	c.add(5 * time.Second)
	s.tickFolds()

	f1, f2 := foldOf(s, "s1"), foldOf(s, "s2")
	if f1.bucketNode == nil || f2.bucketNode == nil {
		t.Fatalf("both sessions should have folded buckets")
	}
	// Expand only s1's bucket.
	f1.bucketNode.Expanded = true
	if f2.bucketNode.Expanded {
		t.Fatalf("expanding s1's bucket must not expand s2's")
	}
	// s1's folded agent is not under s2's bucket and vice-versa.
	if isChildOf(f2.bucketNode, s.agents["a1"]) {
		t.Fatalf("s1's agent must not land under s2's bucket")
	}
	if isChildOf(f1.bucketNode, s.agents["b1"]) {
		t.Fatalf("s2's agent must not land under s1's bucket")
	}
	// Dismissing failed in s1 does not affect s2.
	s.applySubAgent("s2", subEv("b2", "fail", agent.StatusFailed))
	s.dismissFailed("s1")
	if _, ok := s.agents["b2"]; !ok {
		t.Fatalf("dismissing s1 must not remove s2's failed agent")
	}
}

// --- expand-to-reveal + monologue -------------------------------------------

// TestFold_ExpandToRevealOpensMonologue: a folded-then-revealed finished agent
// is still selectable and opens its monologue; synthetic rows never do.
func TestFold_ExpandToRevealOpensMonologue(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	fetched := 0
	w.SetHandlers(Handlers{
		GetTranscript: func(sessionID, agentID string) []ChatMessage {
			fetched++
			return []ChatMessage{{Role: "assistant", Content: "ok"}}
		},
	})
	s := w.sidebar
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	s.now = c.now
	s.ttl = 1 * time.Second
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	c.add(2 * time.Second)
	s.tickFolds()

	fold := foldOf(s, "s1")
	fold.bucketNode.Expanded = true // reveal
	agentNode := s.agents["a1"]

	// A click on the revealed finished agent opens its monologue.
	w.monolog = nil
	s.tree.OnSelectMouse(agentNode)
	if w.monolog == nil || fetched == 0 {
		t.Fatalf("revealed finished agent should open its monologue")
	}

	// Synthetic rows (status bar + bucket) are inert.
	for _, n := range []*tv.TreeNode{fold.statusNode, fold.bucketNode} {
		w.monolog = nil
		s.tree.OnSelectMouse(n)
		if w.monolog != nil {
			t.Fatalf("click on a synthetic row must not open a monologue: %q", n.Label)
		}
		s.tree.OnActivate(n)
		if w.monolog != nil {
			t.Fatalf("activate on a synthetic row must not open a monologue: %q", n.Label)
		}
	}
}

// --- restore / rebuild ------------------------------------------------------

// TestFold_RemoveSessionClearsFoldedState: closing a session drops every
// sub-agent's bookkeeping (including folded ones under the bucket) and a re-add
// starts clean.
func TestFold_RemoveSessionClearsFoldedState(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "w1", agent.StatusCompleted))
	c.add(5 * time.Second)
	s.tickFolds() // a1 now folded under bucket

	s.removeSession("s1")
	if foldOf(s, "s1") != nil {
		t.Fatalf("fold bookkeeping should be cleared on removeSession")
	}
	if _, ok := s.agents["a1"]; ok {
		t.Fatalf("folded agent must be removed from s.agents on removeSession")
	}

	// Re-add: clean state, no stale status bar / bucket / counts.
	s.addSession("s1", "Session 1", false)
	if foldOf(s, "s1") != nil {
		t.Fatalf("re-added session should start with no fold bookkeeping")
	}
	if n := len(s.sessions["s1"].Children); n != 0 {
		t.Fatalf("re-added session should be clean, got %d children", n)
	}
	s.applySubAgent("s1", subEv("a2", "fresh", agent.StatusRunning))
	if g := foldOf(s, "s1").statusNode.Label; !strings.Contains(g, "▶1") || strings.Contains(g, "✓") {
		t.Fatalf("re-added session status bar should show only the fresh agent: %q", g)
	}
}

// --- status-bar counts ------------------------------------------------------

// TestFold_StatusBarCountsMixed verifies the bracketed counts with zero counts
// omitted, folded agents included in ✓, and undismissed failures in ✗.
func TestFold_StatusBarCountsMixed(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "r1", agent.StatusRunning))
	s.applySubAgent("s1", subEv("a2", "r2", agent.StatusRunning))
	s.applySubAgent("s1", subEv("a3", "w1", agent.StatusWaiting))
	s.applySubAgent("s1", subEv("a4", "done1", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a5", "done2", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a6", "boom", agent.StatusFailed))

	bar := foldOf(s, "s1").statusNode.Label
	for _, seg := range []string{"▶2", "‖1", "✓2", "✗1"} {
		if !strings.Contains(bar, seg) {
			t.Fatalf("status bar %q should contain %q", bar, seg)
		}
	}
	// Zero counts omitted (no idle glyph).
	if strings.Contains(bar, "•") {
		t.Fatalf("status bar should omit zero/idle counts: %q", bar)
	}

	// Fold the two completed agents: ✓ count unchanged (includes folded), bucket
	// shows the folded count.
	c.add(5 * time.Second)
	s.tickFolds()
	bar = foldOf(s, "s1").statusNode.Label
	for _, seg := range []string{"▶2", "‖1", "✓2", "✗1"} {
		if !strings.Contains(bar, seg) {
			t.Fatalf("status bar after fold %q should still contain %q (folded ✓ included)", bar, seg)
		}
	}
	if g := foldOf(s, "s1").bucketNode.Label; !strings.Contains(g, "✓ 2") {
		t.Fatalf("bucket label %q should show folded count [✓ 2]", g)
	}
}

// TestFold_StatusBarZeroOmission: a session with only completed + failed agents
// omits the running/waiting segments.
func TestFold_StatusBarZeroOmission(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "done", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a2", "boom", agent.StatusFailed))
	c.add(5 * time.Second)
	s.tickFolds()

	bar := foldOf(s, "s1").statusNode.Label
	if strings.Contains(bar, "▶") || strings.Contains(bar, "‖") {
		t.Fatalf("status bar %q should omit running/waiting (zero counts)", bar)
	}
	if !strings.Contains(bar, "✓1") || !strings.Contains(bar, "✗1") {
		t.Fatalf("status bar %q should show [✓1 ✗1]", bar)
	}
}

// --- selection stability ----------------------------------------------------

// TestFold_SelectionStaysOnFoldedRow moves to the bucket when the selected
// completed agent is the one folded away by a background tick.
func TestFold_SelectionStaysOnFoldedRow(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	agentNode := s.agents["a1"]
	if !s.tree.SelectNode(agentNode) {
		t.Fatalf("setup: could not select the in-TTL completed agent")
	}

	c.add(5 * time.Second)
	s.tickFolds()

	if got := s.tree.Selected(); got != foldOf(s, "s1").bucketNode {
		t.Fatalf("selection should re-anchor to the bucket, got %v", got)
	}
}

// TestFold_SelectionUnaffectedWhenOtherRowFolds: when a DIFFERENT agent folds,
// the selected (still-visible) row keeps the highlight.
func TestFold_SelectionUnaffectedWhenOtherRowFolds(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "old", agent.StatusCompleted)) // folds first
	c.add(1 * time.Second)
	s.applySubAgent("s1", subEv("a2", "new", agent.StatusCompleted)) // within TTL
	a2 := s.agents["a2"]
	if !s.tree.SelectNode(a2) {
		t.Fatalf("setup: could not select a2")
	}

	c.add(4 * time.Second) // a1 age 5 (folds), a2 age 4 (stays)
	s.tickFolds()

	if got := s.tree.Selected(); got != a2 {
		t.Fatalf("selection should stay on the still-visible a2, got %v", got)
	}
	if !isChildOf(foldOf(s, "s1").bucketNode, s.agents["a1"]) {
		t.Fatalf("a1 should have folded")
	}
	if isChildOf(foldOf(s, "s1").bucketNode, a2) {
		t.Fatalf("a2 should still be visible (not folded)")
	}
}

// --- bucket detach on re-run edge -------------------------------------------

// TestFold_UnfoldOnRerunDetachesBucket: if a folded completed agent leaves the
// completed state (defensive re-run edge), it returns to the visible list and an
// emptied bucket is detached so no "[✓ 0]" row lingers.
func TestFold_UnfoldOnRerunDetachesBucket(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	c.add(5 * time.Second)
	s.tickFolds()
	if foldOf(s, "s1").bucketNode == nil {
		t.Fatalf("setup: agent should have folded")
	}

	// Re-run: the same agent emits Running again.
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusRunning))

	parent := s.sessions["s1"]
	if foldOf(s, "s1").bucketNode != nil {
		t.Fatalf("emptied bucket should be detached after unfold (no [✓ 0] row)")
	}
	if !isChildOf(parent, s.agents["a1"]) {
		t.Fatalf("agent should be back in the visible list after re-run")
	}
	if e := foldOf(s, "s1").entries["a1"]; e.folded || !e.finishedAt.IsZero() {
		t.Fatalf("re-run should clear folded + finishedAt, got folded=%v finishedAt=%v", e.folded, e.finishedAt)
	}
}

// --- watcher interleave ordering -------------------------------------------

// TestFold_WatcherInterleaveOrdering pins the synthetic-prefix invariant when a
// watcher is attached and later detached around a fold: status bar stays
// child[0], bucket child[1], watcher preserved.
func TestFold_WatcherInterleaveOrdering(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	// Watcher attached first.
	s.setWatchers(nil, map[string][]WatcherInfo{"s1": {attInfo("w1", "gh", "s1", false)}})
	// Then a sub-agent that completes and folds.
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	c.add(5 * time.Second)
	s.tickFolds()

	parent := s.sessions["s1"]
	fold := foldOf(s, "s1")
	if parent.Children[0] != fold.statusNode {
		t.Fatalf("status bar must stay child[0] with a watcher present")
	}
	if parent.Children[1] != fold.bucketNode {
		t.Fatalf("bucket must stay child[1] with a watcher present")
	}
	wNode := s.watchers["w1"]
	if wNode == nil || !isChildOf(parent, wNode) {
		t.Fatalf("watcher node must survive the fold and remain a child")
	}

	// Detach the watcher: synthetic nodes keep their positions.
	s.setWatchers(nil, nil)
	parent = s.sessions["s1"]
	if parent.Children[0] != fold.statusNode || parent.Children[1] != fold.bucketNode {
		t.Fatalf("synthetic prefix must survive a watcher detach: %v", parent.Children)
	}
	if isChildOf(parent, wNode) {
		t.Fatalf("watcher should be detached")
	}
}

// --- Overall count unchanged by fold ---------------------------------------

// TestFold_OverallAgentsCountUnchangedByFold: folding is visibility-only, so
// len(s.agents) (which drives the Overall band) is unchanged by a fold; only a
// dismissed failure leaves it.
func TestFold_OverallAgentsCountUnchangedByFold(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	before := len(s.agents)

	c.add(5 * time.Second)
	s.tickFolds()

	if got := len(s.agents); got != before {
		t.Fatalf("folding must not change len(s.agents): was %d now %d", before, got)
	}
	// A dismissed failure does leave the count.
	s.applySubAgent("s1", subEv("a2", "boom", agent.StatusFailed))
	s.dismissFailed("s1")
	if got := len(s.agents); got != before {
		t.Fatalf("only a dismissed failure should leave s.agents: was %d now %d", before, got)
	}
}

// --- render: empty-bucket rule (S1) + folded bucket (S2) --------------------

// TestFold_RenderEmptyBucketRule renders the sidebar to assert that while nothing
// is folded no "[✓ 0]" / "▸" bucket row is painted (the bucket must be absent),
// and that after a fold the bucket row appears.
func TestFold_RenderEmptyBucketRule(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	s := w.sidebar
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	s.now = c.now
	s.ttl = 5 * time.Second
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "done1", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a2", "done2", agent.StatusCompleted))

	render := func() []string {
		w.sidebar.panel.SetBounds(tv.Rect{X: 0, Y: 0, W: 32, H: 12})
		w.desktop.Redraw()
		abs := w.sidebar.panel.AbsoluteBounds()
		var rows []string
		for y := 0; y < abs.H; y++ {
			var b strings.Builder
			for x := 0; x < abs.W; x++ {
				ch := w.app.ReadCell(abs.X+x, abs.Y+y).Ch
				if ch == 0 {
					ch = ' '
				}
				b.WriteRune(ch)
			}
			rows = append(rows, b.String())
		}
		return rows
	}

	// Within TTL: status bar shows ✓2, both rows visible, no bucket.
	rows := render()
	if !hasRowContaining(rows, "[✓2") {
		t.Fatalf("within TTL the status bar should show [✓2]:\n%s", strings.Join(rows, "\n"))
	}
	if hasRowContaining(rows, "[✓ 0]") || hasRowContaining(rows, "▸") {
		t.Fatalf("no bucket row should be painted while nothing folded:\n%s", strings.Join(rows, "\n"))
	}

	// After fold: bucket row appears, status bar still ✓2.
	c.add(5 * time.Second)
	s.tickFolds()
	rows = render()
	if !hasRowContaining(rows, "[✓2") {
		t.Fatalf("status bar should still show ✓2 after fold:\n%s", strings.Join(rows, "\n"))
	}
	if !hasRowContaining(rows, "▸") || !hasRowContaining(rows, "[✓ 2]") {
		t.Fatalf("a folded bucket row ▸ [✓ 2] should be painted after fold:\n%s", strings.Join(rows, "\n"))
	}
}

// --- menu dismiss wiring ----------------------------------------------------

// TestFold_DismissFailedSubAgentsNoOpWithoutActiveSession ensures the View-menu
// action does not panic and is a no-op when there is no active session.
func TestFold_DismissFailedSubAgentsNoOpWithoutActiveSession(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	// No active session: must be a no-op, not a panic.
	w.dismissFailedSubAgents()
	if len(w.sidebar.folds) != 0 {
		t.Fatalf("no folds should exist with no active session")
	}
}

// --- integration: tickBusyStatuses drives the fold -------------------------

// TestFold_DrivenByTickBusyStatuses verifies the design's central wiring claim:
// the existing 1s status ticker (tickBusyStatuses) is what expires folds, even
// for an otherwise-idle session — no per-agent timers.
func TestFold_DrivenByTickBusyStatuses(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	s := w.sidebar
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	s.now = c.now
	s.ttl = 5 * time.Second
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))

	// Before TTL: tickBusyStatuses must not fold.
	c.add(2 * time.Second)
	w.tickBusyStatuses()
	if foldOf(s, "s1").bucketNode != nil {
		t.Fatalf("tickBusyStatuses must not fold before the TTL elapses")
	}
	// After TTL: the same sweep folds it.
	c.add(5 * time.Second)
	w.tickBusyStatuses()
	if foldOf(s, "s1").bucketNode == nil {
		t.Fatalf("tickBusyStatuses should fold a completed agent once its TTL elapses")
	}
}

// --- dismiss while a folded bucket exists -----------------------------------

// TestFold_DismissFailedKeepsBucket: dismissing failed agents does not disturb an
// existing folded-completed bucket; ✓ stays, ✗ drops.
func TestFold_DismissFailedKeepsBucket(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "done", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a2", "boom", agent.StatusFailed))
	c.add(5 * time.Second)
	s.tickFolds() // a1 folded under bucket

	s.dismissFailed("s1")

	fold := foldOf(s, "s1")
	if fold.bucketNode == nil {
		t.Fatalf("dismissing a failed agent must not tear down the folded bucket")
	}
	if !isChildOf(fold.bucketNode, s.agents["a1"]) {
		t.Fatalf("folded completed agent must remain under the bucket")
	}
	if _, ok := s.agents["a2"]; ok {
		t.Fatalf("failed agent must be dismissed")
	}
	bar := fold.statusNode.Label
	if strings.Contains(bar, "✗") {
		t.Fatalf("✗ count should drop after dismiss: %q", bar)
	}
	if !strings.Contains(bar, "✓1") {
		t.Fatalf("✓ count should still include the folded agent: %q", bar)
	}
}

// --- bucket expand state stable across ticks --------------------------------

// TestFold_BucketStaysAsUserLeftIt: once the bucket exists, a later fold (another
// agent aging out) does not override a user's manual expand, and a second sweep
// does not collapse it.
func TestFold_BucketStaysAsUserLeftIt(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "old", agent.StatusCompleted))
	c.add(5 * time.Second)
	s.tickFolds()
	bucket := foldOf(s, "s1").bucketNode
	if bucket.Expanded {
		t.Fatalf("bucket must be collapsed by default on first fold")
	}
	// User expands it.
	bucket.Expanded = true
	// A later completed agent folds in: expand state must be preserved.
	s.applySubAgent("s1", subEv("a2", "newer", agent.StatusCompleted))
	c.add(5 * time.Second)
	s.tickFolds()
	if !foldOf(s, "s1").bucketNode.Expanded {
		t.Fatalf("a later fold must not collapse a bucket the user expanded")
	}
	if n := len(foldOf(s, "s1").bucketNode.Children); n != 2 {
		t.Fatalf("both completed agents should be folded, got %d", n)
	}
	// Another idle sweep must not flip it either.
	s.tickFolds()
	if !foldOf(s, "s1").bucketNode.Expanded {
		t.Fatalf("an idle sweep must not collapse the bucket")
	}
}
