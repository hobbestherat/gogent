package ui

// Tests for issues #484 / #490: TTL-based folding of finished sub-agents, now
// collapsed into a SINGLE per-session summary line (issue #490) that both
// renders the bracketed per-state counts ("[▶2 ‖1 ✓5 ✗1]") AND parents the
// TTL-folded ("archived") completed agents — with no leading ▸/▾ (turbotui
// HideMarker) and a trailing "+" (collapsed) / "-" (expanded) suffix that is the
// only expand affordance, absent while the summary is childless.
//
// All fold/TTL/summary state lives in the sidebar UI mirror (sidebar.go); the
// shared agent tree, server and remote handlers are untouched. The TTL clock is
// injected (sidebar.now / sidebar.ttl) so no test sleeps the real 60s.
// Production defaults are asserted separately.

import (
	"strings"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
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
// synthetic summary node (issue #490). Archived agents live UNDER the summary,
// not as siblings, so this counts only VISIBLE agent rows.
func countNodeRefChildren(n *tv.TreeNode) int {
	count := 0
	for _, c := range n.Children {
		if _, ok := c.Data.(nodeRef); ok {
			count++
		}
	}
	return count
}

// countSyntheticChildren counts synthetic (summary) children of a node. Issue
// #490 requires EXACTLY ONE per session (the old two-row status+bucket layout is
// gone), so tests assert this is 1 once a sub-agent exists.
func countSyntheticChildren(n *tv.TreeNode) int {
	count := 0
	for _, c := range n.Children {
		if _, ok := c.Data.(syntheticRef); ok {
			count++
		}
	}
	return count
}

// foldOf returns the session's fold bookkeeping (nil if none).
func foldOf(s *sidebar, id string) *sessionFold { return s.folds[id] }

// summaryOf returns the session's summary node (nil if none). Convenience for
// the single-node model.
func summaryOf(s *sidebar, id string) *tv.TreeNode {
	if f := s.folds[id]; f != nil {
		return f.summaryNode
	}
	return nil
}

// hasRowContaining reports whether any rendered sidebar row contains sub.
func hasRowContaining(rows []string, sub string) bool {
	for _, r := range rows {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

// rowContaining returns the first rendered row containing sub, or "".
func rowContaining(rows []string, sub string) string {
	for _, r := range rows {
		if strings.Contains(r, sub) {
			return r
		}
	}
	return ""
}

// rowYContaining returns the (panel-relative == screen, panel at Y:0) Y of the
// first row containing sub, used to target a real click at the right row.
func rowYContaining(rows []string, sub string) (int, bool) {
	for i, r := range rows {
		if strings.Contains(r, sub) {
			return i, true
		}
	}
	return 0, false
}

// suffixAfterBracket extracts the trailing suffix after the last ']' in a
// summary label. Returns "" when the label ends at ']' (childless / no suffix).
func suffixAfterBracket(label string) string {
	if i := strings.LastIndexByte(label, ']'); i >= 0 {
		return label[i+1:]
	}
	return ""
}

// renderSidebar lays the sidebar out into a fixed buffer and returns the painted
// rows (one string per row), mirroring the render-based fold tests. The panel is
// pinned at (0,0) with a generous size so labels are not truncated and no
// scrollbar column interferes with click coordinates.
func renderSidebar(w *Workbench) []string {
	w.sidebar.panel.SetBounds(tv.Rect{X: 0, Y: 0, W: 48, H: 20})
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
		rows = append(rows, strings.TrimRight(b.String(), " "))
	}
	return rows
}

// clickTreeAt drives the tree's full click handler (handleClick) at screen X,Y.
// Down=false so the release path runs (the press path returns early). Bypasses
// desktop hit-testing but exercises the tree's own row-mapping + OnToggle +
// OnSelect/OnSelectMouse/OnActivate dispatch.
func clickTreeAt(s *sidebar, x, y int) {
	s.tree.Root().OnClickFn(s.tree.Root(), tui.ClickEvent{X: x, Y: y, Button: tui.MouseLeft, Down: false})
}

// typeTreeKey drives the tree's full keyboard handler (handleType) for the
// selected row.
func typeTreeKey(s *sidebar, key tui.KeyCode, r rune) {
	s.tree.Root().OnTypeFn(s.tree.Root(), tui.TypeEvent{Key: key, Rune: r})
}

// --- S1: summary created + pinned; childless; no suffix ---------------------

// TestFold_SummaryCreatedAndPinned verifies the per-session summary is created as
// child[0] on the first sub-agent, is a synthetic (inert) PARENT with its marker
// hidden, shows the live counts, and has NO +/- suffix while childless.
func TestFold_SummaryCreatedAndPinned(t *testing.T) {
	s, _ := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)

	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusRunning))

	parent := s.sessions["s1"]
	summary := summaryOf(s, "s1")
	if summary == nil {
		t.Fatalf("expected a sessionFold with a summary node after a sub-agent")
	}
	if countSyntheticChildren(parent) != 1 {
		t.Fatalf("expected EXACTLY ONE synthetic summary child, got %d", countSyntheticChildren(parent))
	}
	// Summary pinned at child[0].
	if parent.Children[0] != summary {
		t.Fatalf("summary must be child[0]")
	}
	// Summary is synthetic (inert to select/activate) with its marker hidden.
	if _, ok := summary.Data.(syntheticRef); !ok {
		t.Fatalf("summary Data must be syntheticRef, got %T", summary.Data)
	}
	if !summary.HideMarker {
		t.Fatalf("summary must hide its leading ▸/▾ marker (issue #490)")
	}
	// Childless ⇒ no archived children ⇒ no suffix yet (empty-bucket rule).
	if len(summary.Children) != 0 {
		t.Fatalf("summary must be childless before any fold, got %d children", len(summary.Children))
	}
	if got := suffixAfterBracket(summary.Label); got != "" {
		t.Fatalf("childless summary must have NO suffix, got %q (label %q)", got, summary.Label)
	}
	// Live counts rendered with the running glyph.
	if g := summary.Label; !strings.Contains(g, "▶") || !strings.Contains(g, "1") {
		t.Fatalf("summary label = %q, want it to show ▶1", g)
	}
	// The real agent row sits after the summary.
	if countNodeRefChildren(parent) != 1 {
		t.Fatalf("want 1 real agent child, got %d", countNodeRefChildren(parent))
	}
}

// TestFold_NoSummaryWithoutSubAgent ensures a session with only watchers (no
// sub-agents) gets no summary / fold bookkeeping.
func TestFold_NoSummaryWithoutSubAgent(t *testing.T) {
	s, _ := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	// Attach a watcher only.
	s.setWatchers(nil, map[string][]WatcherInfo{"s1": {attInfo("w1", "gh", "s1", false)}})
	if foldOf(s, "s1") != nil {
		t.Fatalf("a watcher-only session must not get fold bookkeeping")
	}
	if n := len(s.sessions["s1"].Children); n != 1 {
		t.Fatalf("watcher-only session should have exactly the watcher child, got %d", n)
	}
}

// --- single summary line, never two (core #490 acceptance) ------------------

// TestFold_ExactlyOneSummaryLine: after a fold there is still exactly ONE
// synthetic child (the summary), never two, and no separate "[✓ N]" bucket row
// exists. This is the headline #490 fix.
func TestFold_ExactlyOneSummaryLine(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "done1", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a2", "done2", agent.StatusCompleted))

	c.add(5 * time.Second)
	s.tickFolds()

	parent := s.sessions["s1"]
	if got := countSyntheticChildren(parent); got != 1 {
		t.Fatalf("after a fold there must be EXACTLY ONE synthetic summary child, got %d", got)
	}
	summary := summaryOf(s, "s1")
	if summary == nil {
		t.Fatalf("summary node must exist")
	}
	// Both archived agents live under the single summary, not as extra rows.
	if got := len(summary.Children); got != 2 {
		t.Fatalf("summary must parent both archived agents, got %d children", got)
	}
	// The totals line IS the summary; there must be no separate "[✓ N]" bucket
	// row (the old second synthetic row). The summary label contains the folded
	// count inline and a trailing suffix, never a bare "[✓ 2]".
	if !strings.Contains(summary.Label, "✓2") {
		t.Fatalf("summary label %q must include the archived count ✓2 inline", summary.Label)
	}
	if strings.Contains(summary.Label, "[✓ ") {
		t.Fatalf("summary label %q must not be a bare [✓ N] bucket row", summary.Label)
	}
}

// --- TTL fold timing (core acceptance criterion) ----------------------------

// TestFold_CompletedVisibleWithinTTL: before the TTL elapses a completed
// sub-agent stays a normal visible child row and the summary is childless.
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
	summary := summaryOf(s, "s1")
	if len(summary.Children) != 0 {
		t.Fatalf("summary must be childless within TTL, got %d children", len(summary.Children))
	}
	if got := suffixAfterBracket(summary.Label); got != "" {
		t.Fatalf("childless summary must have no suffix within TTL, got %q", got)
	}
	// Summary still counts the (visible) completed agent.
	if g := summary.Label; !strings.Contains(g, "✓") {
		t.Fatalf("summary should include the completed agent in the ✓ count: %q", g)
	}
}

// TestFold_FoldsAfterTTL: once the TTL elapses, tickFolds moves the completed
// agent UNDER the collapsed summary, keeps it in the ✓ count, and the summary
// gains the "+" suffix.
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
	summary := summaryOf(s, "s1")
	if summary == nil {
		t.Fatalf("summary should survive a fold")
	}
	if summary.Expanded {
		t.Fatalf("summary must be collapsed by default once it has children (first-fold rule)")
	}
	if !isChildOf(summary, node) {
		t.Fatalf("completed agent should be moved under the summary")
	}
	if isChildOf(parent, node) {
		t.Fatalf("completed agent should no longer be a visible child of the session")
	}
	// Summary stays child[0].
	if parent.Children[0] != summary {
		t.Fatalf("summary must stay child[0]")
	}
	// Has archived children + collapsed ⇒ "+" suffix.
	if got := suffixAfterBracket(summary.Label); got != "+" {
		t.Fatalf("collapsed summary with children must show '+' suffix, got %q (label %q)", got, summary.Label)
	}
	// Summary ✓ count still includes the folded agent.
	if g := summary.Label; !strings.Contains(g, "✓") {
		t.Fatalf("summary must still count folded agents in ✓: %q", g)
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
	if summary := summaryOf(s, "s1"); len(summary.Children) != 0 {
		t.Fatalf("agent must not fold before TTL measured from delivery has elapsed")
	}
	// Past TTL measured from delivery: folds.
	c.add(5 * time.Second)
	s.tickFolds()
	if summary := summaryOf(s, "s1"); len(summary.Children) != 1 {
		t.Fatalf("agent should fold once TTL-from-delivery elapses, got %d children", len(summary.Children))
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
	// And must not double-add the node under the summary.
	c.add(5 * time.Second)
	s.tickFolds()
	if n := len(summaryOf(s, "s1").Children); n != 1 {
		t.Fatalf("duplicate completion must not add a second summary child, got %d", n)
	}
}

// --- failed agents: never auto-fold + manual dismiss ------------------------

// TestFold_FailedNeverAutoFolds: a failed agent stays a visible child across
// ticks past the TTL (only StatusCompleted folds) and shows in the ✗ count; the
// summary never parents it.
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
	summary := summaryOf(s, "s1")
	if len(summary.Children) != 0 {
		t.Fatalf("summary must never parent a failed agent, got %d children", len(summary.Children))
	}
	if got := suffixAfterBracket(summary.Label); got != "" {
		t.Fatalf("a failed-only session summary must be childless (no suffix), got %q", got)
	}
	if g := summary.Label; !strings.Contains(g, "✗") {
		t.Fatalf("summary should count the failed agent in ✗: %q", g)
	}
}

// TestFold_DismissFailedClearsRow: dismissFailed removes every undismissed failed
// agent of a session, drops its ✗ count, and refreshes the summary.
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
	// ✗ count is now 0; the summary still shows the running agent (so it stays).
	if g := summaryOf(s, "s1").Label; strings.Contains(g, "✗") {
		t.Fatalf("summary should no longer show ✗ after dismiss: %q", g)
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

// TestFold_PerSessionIndependence: folding/expanding one session's summary does
// not touch another's nodes, expand state, or counts.
func TestFold_PerSessionIndependence(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.addSession("s2", "Session 2", false)
	s.applySubAgent("s1", subEv("a1", "w1", agent.StatusCompleted))
	s.applySubAgent("s2", subEv("b1", "w2", agent.StatusCompleted))

	c.add(5 * time.Second)
	s.tickFolds()

	sum1, sum2 := summaryOf(s, "s1"), summaryOf(s, "s2")
	if len(sum1.Children) != 1 || len(sum2.Children) != 1 {
		t.Fatalf("both sessions should have folded one agent under their summaries")
	}
	// Expand only s1's summary.
	sum1.Expanded = true
	if sum2.Expanded {
		t.Fatalf("expanding s1's summary must not expand s2's")
	}
	// s1's folded agent is not under s2's summary and vice-versa.
	if isChildOf(sum2, s.agents["a1"]) {
		t.Fatalf("s1's agent must not land under s2's summary")
	}
	if isChildOf(sum1, s.agents["b1"]) {
		t.Fatalf("s2's agent must not land under s1's summary")
	}
	// Dismissing failed in s1 does not affect s2.
	s.applySubAgent("s2", subEv("b2", "fail", agent.StatusFailed))
	s.dismissFailed("s1")
	if _, ok := s.agents["b2"]; !ok {
		t.Fatalf("dismissing s1 must not remove s2's failed agent")
	}
}

// --- toggle: OnToggle hook --------------------------------------------------

// TestFold_OnToggleHookFlipsSuffixAndVisibility drives the host OnToggle hook
// directly: it returns true for a childed summary, flips Expanded, refreshes the
// +/- suffix, and the archived child becomes visible/hidden via flatten(). It
// returns false for a childless summary and for real nodes (self-filter).
func TestFold_OnToggleHookFlipsSuffixAndVisibility(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	c.add(5 * time.Second)
	s.tickFolds()

	summary := summaryOf(s, "s1")
	archived := s.agents["a1"]

	// Collapsed by default ⇒ "+", child hidden (not in flattened rows).
	if summary.Expanded {
		t.Fatalf("summary should be collapsed after first fold")
	}
	if got := suffixAfterBracket(summary.Label); got != "+" {
		t.Fatalf("collapsed summary suffix = %q, want +", got)
	}
	if s.tree.SelectNode(archived) {
		t.Fatalf("archived child must be hidden while summary collapsed")
	}

	// Toggle via the hook ⇒ expand: "-" suffix, child visible.
	if !s.tree.OnToggle(summary, tui.ClickEvent{}) {
		t.Fatalf("OnToggle must return true for a childed summary")
	}
	if !summary.Expanded {
		t.Fatalf("OnToggle must flip Expanded to true")
	}
	if got := suffixAfterBracket(summary.Label); got != "-" {
		t.Fatalf("expanded summary suffix = %q, want - (label %q)", got, summary.Label)
	}
	if !s.tree.SelectNode(archived) {
		t.Fatalf("archived child must be visible once summary expanded")
	}

	// Toggle again ⇒ collapse: "+" suffix, child hidden.
	if !s.tree.OnToggle(summary, tui.ClickEvent{}) {
		t.Fatalf("OnToggle must return true on collapse")
	}
	if summary.Expanded {
		t.Fatalf("OnToggle must flip Expanded back to false")
	}
	if got := suffixAfterBracket(summary.Label); got != "+" {
		t.Fatalf("re-collapsed summary suffix = %q, want +", got)
	}
	if s.tree.SelectNode(archived) {
		t.Fatalf("archived child must be hidden again once summary collapsed")
	}
}

// TestFold_OnToggleSelfFilters asserts the hook leaves real rows alone: a
// childless summary, a real session node, and a real agent node all return false
// so they keep the tree's default marker / activate behaviour.
func TestFold_OnToggleSelfFilters(t *testing.T) {
	s, _ := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "run", agent.StatusRunning))

	session := s.sessions["s1"]
	agentNode := s.agents["a1"]
	summary := summaryOf(s, "s1") // childless (nothing folded)

	if s.tree.OnToggle(summary, tui.ClickEvent{}) {
		t.Fatalf("OnToggle must return false for a childless summary (nothing to toggle)")
	}
	if s.tree.OnToggle(session, tui.ClickEvent{}) {
		t.Fatalf("OnToggle must return false for a real session node")
	}
	if s.tree.OnToggle(agentNode, tui.ClickEvent{}) {
		t.Fatalf("OnToggle must return false for a real agent node")
	}
}

// --- toggle: full click path (integration, criterion #2) --------------------

// TestFold_FullClickTogglesAndIsInert: a real click on the summary row toggles
// expansion (via the wired OnToggle), reveals/hides the archived children, and
// NEVER opens a monologue or raises a window (syntheticRef inert, issue #302).
// A repeat click does not activate either.
func TestFold_FullClickTogglesAndIsInert(t *testing.T) {
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

	summary := summaryOf(s, "s1")

	// Collapsed: render, find the summary row's screen Y, click it.
	rows := renderSidebar(w)
	y, ok := rowYContaining(rows, "✓1")
	if !ok {
		t.Fatalf("summary row not rendered:\n%s", strings.Join(rows, "\n"))
	}
	if !strings.Contains(rowContaining(rows, "✓1"), "+") {
		t.Fatalf("summary row should show + suffix while collapsed:\n%s", strings.Join(rows, "\n"))
	}
	// Archived agent hidden while collapsed.
	if hasRowContaining(rows, "worker") {
		t.Fatalf("archived agent must be hidden while summary collapsed:\n%s", strings.Join(rows, "\n"))
	}

	w.monolog = nil
	clickTreeAt(s, 10, y) // expand

	if !summary.Expanded {
		t.Fatalf("a click on the summary row must expand it")
	}
	if w.monolog != nil || fetched != 0 {
		t.Fatalf("a body click on the summary must NOT open a monologue")
	}
	rows = renderSidebar(w)
	if !hasRowContaining(rows, "worker") {
		t.Fatalf("archived agent must be visible once summary expanded:\n%s", strings.Join(rows, "\n"))
	}
	if !strings.Contains(rowContaining(rows, "✓1"), "-") {
		t.Fatalf("summary row should show - suffix once expanded:\n%s", strings.Join(rows, "\n"))
	}

	// Click again: collapse, and a repeat click must still not activate.
	w.monolog = nil
	clickTreeAt(s, 10, y) // collapse
	if summary.Expanded {
		t.Fatalf("a second click must collapse the summary")
	}
	if w.monolog != nil || fetched != 0 {
		t.Fatalf("a repeat click on the summary must NOT activate (open a monologue)")
	}
	rows = renderSidebar(w)
	if hasRowContaining(rows, "worker") {
		t.Fatalf("archived agent must be hidden again once collapsed:\n%s", strings.Join(rows, "\n"))
	}
}

// TestFold_FullClickChildlessSummaryInert: clicking a childless summary does
// nothing (no toggle) and opens no monologue.
func TestFold_FullClickChildlessSummaryInert(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	w.SetHandlers(Handlers{
		GetTranscript: func(string, string) []ChatMessage { return nil },
	})
	s := w.sidebar
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "run", agent.StatusRunning)) // running, nothing folded

	summary := summaryOf(s, "s1")
	rows := renderSidebar(w)
	y, ok := rowYContaining(rows, "▶1")
	if !ok {
		t.Fatalf("summary row not rendered:\n%s", strings.Join(rows, "\n"))
	}

	w.monolog = nil
	clickTreeAt(s, 10, y)
	if summary.Expanded {
		t.Fatalf("clicking a childless summary must not toggle anything")
	}
	if w.monolog != nil {
		t.Fatalf("clicking a childless summary must not open a monologue")
	}
}

// --- toggle: keyboard path + draw-time reconcile (criterion #2) -------------

// TestFold_KeyboardTogglesAndSuffixTracks: keyboard Left/Right/Space flip
// Expanded natively (no OnToggle hook on the keyboard path), so the +/- suffix
// would go stale — the draw-time reconcile (syncFoldSuffixes, run from
// panel.DrawFn before the tree paints) must re-derive it. We assert the label is
// stale immediately after the key, then correct after a render.
func TestFold_KeyboardTogglesAndSuffixTracks(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	s := w.sidebar
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	s.now = c.now
	s.ttl = 1 * time.Second
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	c.add(2 * time.Second)
	s.tickFolds()

	summary := summaryOf(s, "s1")
	if !s.tree.SelectNode(summary) {
		t.Fatalf("setup: could not select the summary")
	}

	// Right expands (collapsed → expanded). No hook fires, so the label is stale
	// until the next draw.
	typeTreeKey(s, tui.KeyRight, 0)
	if !summary.Expanded {
		t.Fatalf("KeyRight must expand the summary")
	}
	if got := suffixAfterBracket(summary.Label); got != "+" {
		t.Fatalf("before redraw the suffix is stale (+), got %q", got)
	}
	renderSidebar(w) // triggers syncFoldSuffixes via panel.DrawFn
	if got := suffixAfterBracket(summary.Label); got != "-" {
		t.Fatalf("after redraw the suffix must track Expanded (-), got %q (label %q)", got, summary.Label)
	}

	// Left collapses. Again stale until redraw.
	typeTreeKey(s, tui.KeyLeft, 0)
	if summary.Expanded {
		t.Fatalf("KeyLeft must collapse the summary")
	}
	if got := suffixAfterBracket(summary.Label); got != "-" {
		t.Fatalf("before redraw the suffix is stale (-), got %q", got)
	}
	renderSidebar(w)
	if got := suffixAfterBracket(summary.Label); got != "+" {
		t.Fatalf("after redraw the suffix must track Expanded (+), got %q", got)
	}

	// Space toggles back to expanded and the reconcile fixes the suffix.
	typeTreeKey(s, tui.KeyRune, ' ')
	renderSidebar(w)
	if !summary.Expanded {
		t.Fatalf("Space must toggle the summary to expanded")
	}
	if got := suffixAfterBracket(summary.Label); got != "-" {
		t.Fatalf("after Space+redraw the suffix must be -, got %q", got)
	}
}

// TestFold_SyncFoldSuffixesDirect: the reconcile helper itself, called directly,
// re-derives the suffix from Expanded without mutating counts or structure, and
// is a no-op for a childless summary.
func TestFold_SyncFoldSuffixesDirect(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "done", agent.StatusCompleted))
	c.add(5 * time.Second)
	s.tickFolds()

	summary := summaryOf(s, "s1")
	before := summary.Label // "[✓1]+"
	if got := suffixAfterBracket(before); got != "+" {
		t.Fatalf("setup suffix = %q, want +", got)
	}

	// Flip Expanded programmatically (simulating a keyboard toggle) without
	// refreshFoldChrome: the label is stale.
	summary.Expanded = true
	if got := suffixAfterBracket(summary.Label); got != "+" {
		t.Fatalf("label should be stale before reconcile, got %q", got)
	}
	s.syncFoldSuffixes()
	if got := suffixAfterBracket(summary.Label); got != "-" {
		t.Fatalf("syncFoldSuffixes must re-derive suffix to -, got %q (label %q)", got, summary.Label)
	}
	// The counts bracket is untouched (only the suffix changed).
	if base := summary.Label[:strings.LastIndexByte(summary.Label, ']')+1]; !strings.Contains(base, "✓1") {
		t.Fatalf("syncFoldSuffixes must not alter the counts bracket: %q", base)
	}

	// Collapse back, then empty the summary (unfold) and confirm reconcile yields
	// no suffix for a childless summary without corrupting the label.
	summary.Expanded = false
	s.syncFoldSuffixes()
	if got := suffixAfterBracket(summary.Label); got != "+" {
		t.Fatalf("after collapse reconcile suffix = %q, want +", got)
	}
}

// TestFold_EnterIsNoOp: Enter on the summary neither toggles nor opens a
// monologue (synthetic node; OnActivate bails on syntheticRef).
func TestFold_EnterIsNoOp(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	fetched := 0
	w.SetHandlers(Handlers{
		GetTranscript: func(string, string) []ChatMessage {
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

	summary := summaryOf(s, "s1")
	s.tree.SelectNode(summary)
	w.monolog = nil
	typeTreeKey(s, tui.KeyEnter, 0)
	if summary.Expanded {
		t.Fatalf("Enter must not toggle the summary")
	}
	if w.monolog != nil || fetched != 0 {
		t.Fatalf("Enter on the summary must be a no-op (no monologue)")
	}
}

// --- expand-to-reveal + monologue (#302 inert contract) ---------------------

// TestFold_ExpandToRevealOpensMonologue: a folded-then-revealed finished agent
// is still selectable and opens its monologue; the synthetic summary never does.
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

	summary := summaryOf(s, "s1")
	summary.Expanded = true // reveal
	agentNode := s.agents["a1"]

	// A click on the revealed finished agent opens its monologue.
	w.monolog = nil
	s.tree.OnSelectMouse(agentNode)
	if w.monolog == nil || fetched == 0 {
		t.Fatalf("revealed finished agent should open its monologue")
	}

	// The synthetic summary is inert to both click and activate.
	w.monolog = nil
	s.tree.OnSelectMouse(summary)
	if w.monolog != nil {
		t.Fatalf("click on the summary must not open a monologue: %q", summary.Label)
	}
	s.tree.OnActivate(summary)
	if w.monolog != nil {
		t.Fatalf("activate on the summary must not open a monologue: %q", summary.Label)
	}
}

// --- render: no leading marker + trailing suffix (criterion #1) --------------

// TestFold_RenderNoLeadingMarkerTrailingSuffix renders the sidebar to assert:
// (a) the summary row has NO leading ▸/▾ (HideMarker) and a trailing suffix that
// is absent while childless, "+" while collapsed, "-" while expanded; (b) there
// is never a separate "[✓ N]" bucket row; (c) archived children hide/show with
// the toggle.
func TestFold_RenderNoLeadingMarkerTrailingSuffix(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	s := w.sidebar
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	s.now = c.now
	s.ttl = 5 * time.Second
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "done1", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a2", "done2", agent.StatusCompleted))

	// Within TTL: summary shows ✓2, both rows visible, childless (no suffix), no
	// leading marker on the summary, no bucket row.
	rows := renderSidebar(w)
	sumRow := rowContaining(rows, "[✓2")
	if sumRow == "" {
		t.Fatalf("within TTL the summary should show [✓2]:\n%s", strings.Join(rows, "\n"))
	}
	if strings.Contains(sumRow, "▸") || strings.Contains(sumRow, "▾") {
		t.Fatalf("summary row must have NO leading ▸/▾ (HideMarker): %q", sumRow)
	}
	if got := suffixAfterBracket(sumRow); got != "" {
		t.Fatalf("childless summary must have no suffix in render, got %q", got)
	}
	if hasRowContaining(rows, "[✓ ") {
		t.Fatalf("no separate [✓ N] bucket row should be painted:\n%s", strings.Join(rows, "\n"))
	}

	// After fold: summary gains "+", archived agents hidden, still no leading
	// marker and no bucket row.
	c.add(5 * time.Second)
	s.tickFolds()
	rows = renderSidebar(w)
	sumRow = rowContaining(rows, "[✓2")
	if sumRow == "" {
		t.Fatalf("summary should still show ✓2 after fold:\n%s", strings.Join(rows, "\n"))
	}
	if strings.Contains(sumRow, "▸") || strings.Contains(sumRow, "▾") {
		t.Fatalf("summary row must keep NO leading ▸/▾ after fold: %q", sumRow)
	}
	if !strings.Contains(sumRow, "+") {
		t.Fatalf("collapsed summary row must end with + suffix: %q", sumRow)
	}
	if hasRowContaining(rows, "done1") || hasRowContaining(rows, "done2") {
		t.Fatalf("archived agents must be hidden while collapsed:\n%s", strings.Join(rows, "\n"))
	}
	if hasRowContaining(rows, "[✓ ") {
		t.Fatalf("no separate [✓ N] bucket row after fold:\n%s", strings.Join(rows, "\n"))
	}

	// Expand: suffix flips to "-", archived agents appear.
	summary := summaryOf(s, "s1")
	summary.Expanded = true
	rows = renderSidebar(w)
	sumRow = rowContaining(rows, "[✓2")
	if !strings.Contains(sumRow, "-") {
		t.Fatalf("expanded summary row must end with - suffix: %q", sumRow)
	}
	if !hasRowContaining(rows, "done1") || !hasRowContaining(rows, "done2") {
		t.Fatalf("archived agents must be visible once expanded:\n%s", strings.Join(rows, "\n"))
	}
}

// --- status-bar counts ------------------------------------------------------

// TestFold_StatusBarCountsMixed verifies the bracketed counts with zero counts
// omitted, folded agents included in ✓, and undismissed failures in ✗; and that
// folding adds the suffix without changing the totals.
func TestFold_StatusBarCountsMixed(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "r1", agent.StatusRunning))
	s.applySubAgent("s1", subEv("a2", "r2", agent.StatusRunning))
	s.applySubAgent("s1", subEv("a3", "w1", agent.StatusWaiting))
	s.applySubAgent("s1", subEv("a4", "done1", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a5", "done2", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a6", "boom", agent.StatusFailed))

	summary := summaryOf(s, "s1")
	bar := summary.Label
	for _, seg := range []string{"▶2", "‖1", "✓2", "✗1"} {
		if !strings.Contains(bar, seg) {
			t.Fatalf("summary %q should contain %q", bar, seg)
		}
	}
	// Zero counts omitted (no idle glyph).
	if strings.Contains(bar, "•") {
		t.Fatalf("summary should omit zero/idle counts: %q", bar)
	}
	// Nothing folded yet ⇒ no suffix.
	if got := suffixAfterBracket(bar); got != "" {
		t.Fatalf("summary must have no suffix before any fold, got %q", got)
	}

	// Fold the two completed agents: ✓ count unchanged (includes folded), suffix
	// becomes "+", both archived under the summary.
	c.add(5 * time.Second)
	s.tickFolds()
	bar = summary.Label
	for _, seg := range []string{"▶2", "‖1", "✓2", "✗1"} {
		if !strings.Contains(bar, seg) {
			t.Fatalf("summary after fold %q should still contain %q (folded ✓ included)", bar, seg)
		}
	}
	if got := suffixAfterBracket(bar); got != "+" {
		t.Fatalf("summary suffix after fold = %q, want +", got)
	}
	if n := len(summary.Children); n != 2 {
		t.Fatalf("summary should parent both archived completed agents, got %d children", n)
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

	bar := summaryOf(s, "s1").Label
	if strings.Contains(bar, "▶") || strings.Contains(bar, "‖") {
		t.Fatalf("summary %q should omit running/waiting (zero counts)", bar)
	}
	if !strings.Contains(bar, "✓1") || !strings.Contains(bar, "✗1") {
		t.Fatalf("summary %q should show [✓1 ✗1]", bar)
	}
}

// --- selection stability ----------------------------------------------------

// TestFold_SelectionReanchorsToSummary moves to the summary when the selected
// completed agent is the one folded away by a background tick (collapsed).
func TestFold_SelectionReanchorsToSummary(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	agentNode := s.agents["a1"]
	if !s.tree.SelectNode(agentNode) {
		t.Fatalf("setup: could not select the in-TTL completed agent")
	}

	c.add(5 * time.Second)
	s.tickFolds()

	if got := s.tree.Selected(); got != summaryOf(s, "s1") {
		t.Fatalf("selection should re-anchor to the summary, got %v", got)
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
	summary := summaryOf(s, "s1")
	if !isChildOf(summary, s.agents["a1"]) {
		t.Fatalf("a1 should have folded under the summary")
	}
	if isChildOf(summary, a2) {
		t.Fatalf("a2 should still be visible (not folded)")
	}
}

// TestFold_SelectionStaysWhenSummaryExpanded: when the summary is EXPANDED and
// the selected archived child folds (it is already under it), the selection
// stays on the (still-visible) child rather than jumping to the summary. The
// summary must be expanded AFTER the fold (the first fold forces collapse by
// design), so the archived child is then visible and selectable.
func TestFold_SelectionStaysWhenSummaryExpanded(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	agentNode := s.agents["a1"]
	c.add(5 * time.Second)
	s.tickFolds() // a1 archived under the (collapsed) summary
	summary := summaryOf(s, "s1")
	if summary.Expanded {
		t.Fatalf("first fold must collapse the summary")
	}
	// Reveal the archived child by expanding AFTER the fold.
	summary.Expanded = true

	// The archived child is now visible: select it, then run an idle sweep — the
	// selection must not drift to the summary (the child is still on screen).
	if !s.tree.SelectNode(agentNode) {
		t.Fatalf("archived child should be selectable while summary expanded")
	}
	s.tickFolds()
	if got := s.tree.Selected(); got != agentNode {
		t.Fatalf("selection should stay on the visible archived child, got %v", got)
	}
}

// --- unfold on re-run edge --------------------------------------------------

// TestFold_UnfoldOnRerunKeepsSummary: if a folded completed agent leaves the
// completed state (defensive re-run edge), it returns to the visible list and
// the summary reverts to childless (no suffix) — but, unlike the old bucket, the
// summary NODE survives because it is also the always-present status bar.
func TestFold_UnfoldOnRerunKeepsSummary(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusCompleted))
	c.add(5 * time.Second)
	s.tickFolds()
	if len(summaryOf(s, "s1").Children) != 1 {
		t.Fatalf("setup: agent should have folded under the summary")
	}

	// Re-run: the same agent emits Running again.
	s.applySubAgent("s1", subEv("a1", "worker", agent.StatusRunning))

	parent := s.sessions["s1"]
	summary := summaryOf(s, "s1")
	if summary == nil {
		t.Fatalf("summary node must SURVIVE an unfold-to-childless (it is the status bar)")
	}
	if len(summary.Children) != 0 {
		t.Fatalf("summary should be childless again after unfold, got %d", len(summary.Children))
	}
	if got := suffixAfterBracket(summary.Label); got != "" {
		t.Fatalf("childless summary after unfold must have no suffix, got %q", got)
	}
	if !isChildOf(parent, s.agents["a1"]) {
		t.Fatalf("agent should be back in the visible list after re-run")
	}
	if e := foldOf(s, "s1").entries["a1"]; e.folded || !e.finishedAt.IsZero() {
		t.Fatalf("re-run should clear folded + finishedAt, got folded=%v finishedAt=%v", e.folded, e.finishedAt)
	}
}

// --- restore / rebuild ------------------------------------------------------

// TestFold_RemoveSessionClearsFoldedState: closing a session drops every
// sub-agent's bookkeeping (including archived ones under the summary) and a
// re-add starts clean.
func TestFold_RemoveSessionClearsFoldedState(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "w1", agent.StatusCompleted))
	c.add(5 * time.Second)
	s.tickFolds() // a1 now archived under summary

	s.removeSession("s1")
	if foldOf(s, "s1") != nil {
		t.Fatalf("fold bookkeeping should be cleared on removeSession")
	}
	if _, ok := s.agents["a1"]; ok {
		t.Fatalf("archived agent must be removed from s.agents on removeSession")
	}

	// Re-add: clean state, no stale summary / counts.
	s.addSession("s1", "Session 1", false)
	if foldOf(s, "s1") != nil {
		t.Fatalf("re-added session should start with no fold bookkeeping")
	}
	if n := len(s.sessions["s1"].Children); n != 0 {
		t.Fatalf("re-added session should be clean, got %d children", n)
	}
	s.applySubAgent("s1", subEv("a2", "fresh", agent.StatusRunning))
	if g := summaryOf(s, "s1").Label; !strings.Contains(g, "▶1") || strings.Contains(g, "✓") {
		t.Fatalf("re-added session summary should show only the fresh agent: %q", g)
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

// --- watcher interleave ordering -------------------------------------------

// TestFold_WatcherInterleaveOrdering pins the synthetic-prefix invariant when a
// watcher is attached and later detached around a fold: the summary stays
// child[0], the watcher is a visible sibling, and the folded agent lives UNDER
// the summary.
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
	summary := summaryOf(s, "s1")
	if parent.Children[0] != summary {
		t.Fatalf("summary must stay child[0] with a watcher present")
	}
	if countSyntheticChildren(parent) != 1 {
		t.Fatalf("exactly one synthetic child expected, got %d", countSyntheticChildren(parent))
	}
	wNode := s.watchers["w1"]
	if wNode == nil || !isChildOf(parent, wNode) {
		t.Fatalf("watcher node must survive the fold and remain a visible child")
	}
	if !isChildOf(summary, s.agents["a1"]) {
		t.Fatalf("folded agent must live under the summary, not as a sibling")
	}

	// Detach the watcher: the summary keeps its position.
	s.setWatchers(nil, nil)
	parent = s.sessions["s1"]
	if parent.Children[0] != summary {
		t.Fatalf("summary must survive a watcher detach: %v", parent.Children)
	}
	if isChildOf(parent, wNode) {
		t.Fatalf("watcher should be detached")
	}
}

// --- expand state stable across ticks --------------------------------------

// TestFold_FirstFoldCollapsesLaterFoldsPreserve: the first fold collapses the
// summary; a later fold does not override a user's manual expand, and an idle
// sweep does not collapse it.
func TestFold_FirstFoldCollapsesLaterFoldsPreserve(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "old", agent.StatusCompleted))
	c.add(5 * time.Second)
	s.tickFolds()
	summary := summaryOf(s, "s1")
	if summary.Expanded {
		t.Fatalf("summary must be collapsed by default on first fold")
	}
	// User expands it.
	summary.Expanded = true
	// A later completed agent folds in: expand state must be preserved.
	s.applySubAgent("s1", subEv("a2", "newer", agent.StatusCompleted))
	c.add(5 * time.Second)
	s.tickFolds()
	if !summaryOf(s, "s1").Expanded {
		t.Fatalf("a later fold must not collapse a summary the user expanded")
	}
	if n := len(summaryOf(s, "s1").Children); n != 2 {
		t.Fatalf("both completed agents should be archived, got %d", n)
	}
	// Another idle sweep must not flip it either.
	s.tickFolds()
	if !summaryOf(s, "s1").Expanded {
		t.Fatalf("an idle sweep must not collapse the summary")
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

// --- dismiss while an archive exists ----------------------------------------

// TestFold_DismissFailedKeepsArchive: dismissing failed agents does not disturb
// an existing archived-completed set under the summary; ✓ stays, ✗ drops.
func TestFold_DismissFailedKeepsArchive(t *testing.T) {
	s, c := newFoldSidebar(t)
	s.addSession("s1", "Session 1", false)
	s.applySubAgent("s1", subEv("a1", "done", agent.StatusCompleted))
	s.applySubAgent("s1", subEv("a2", "boom", agent.StatusFailed))
	c.add(5 * time.Second)
	s.tickFolds() // a1 archived under summary

	s.dismissFailed("s1")

	summary := summaryOf(s, "s1")
	if summary == nil {
		t.Fatalf("dismissing a failed agent must not tear down the summary")
	}
	if !isChildOf(summary, s.agents["a1"]) {
		t.Fatalf("archived completed agent must remain under the summary")
	}
	if _, ok := s.agents["a2"]; ok {
		t.Fatalf("failed agent must be dismissed")
	}
	bar := summary.Label
	if strings.Contains(bar, "✗") {
		t.Fatalf("✗ count should drop after dismiss: %q", bar)
	}
	if !strings.Contains(bar, "✓1") {
		t.Fatalf("✓ count should still include the archived agent: %q", bar)
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
	if len(summaryOf(s, "s1").Children) != 0 {
		t.Fatalf("tickBusyStatuses must not fold before the TTL elapses")
	}
	// After TTL: the same sweep folds it.
	c.add(5 * time.Second)
	w.tickBusyStatuses()
	if len(summaryOf(s, "s1").Children) != 1 {
		t.Fatalf("tickBusyStatuses should fold a completed agent once its TTL elapses")
	}
}
