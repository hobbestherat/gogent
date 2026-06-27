package ui

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/agent"
)

// --- glyph distinctness guard (issue #329 Phase 4) -------------------------

// TestWatcherGlyphDistinct is the glyph-disambiguation guard the issue calls for:
// the shared watcher glyph ◷ must be a single distinct cell that collides with
// none of the existing sidebar glyph sets — the session idle/active markers
// (○/●), the sub-agent lifecycle icons (▶/⏸/✓/✗/•) and the todo icons (☐/◐/✔) —
// nor the per-row badges (★/⏳/❓). It also pins the exact intended glyph so a
// future "tidy" cannot silently re-merge it.
func TestWatcherGlyphDistinct(t *testing.T) {
	if watcherGlyph != "◷" {
		t.Fatalf("watcherGlyph = %q, want ◷ (U+25F7)", watcherGlyph)
	}
	// Single display cell: a wide/zero-width glyph would shift the row columns.
	if n := len([]rune(watcherGlyph)); n != 1 {
		t.Fatalf("watcherGlyph must be a single rune, got %d runes", n)
	}

	others := map[string]string{}
	add := func(set, g string) {
		if prev, ok := others[g]; ok {
			t.Logf("note: %q appears in both %s and %s sets", g, prev, set)
		}
		others[g] = set
	}
	// Session idle/active markers.
	add("session", sessionStatusIcon(false))
	add("session", sessionStatusIcon(true))
	// Sub-agent lifecycle icons.
	for _, st := range []agent.AgentStatus{
		agent.StatusIdle, agent.StatusRunning, agent.StatusWaiting,
		agent.StatusCompleted, agent.StatusFailed,
	} {
		add("sub-agent", statusIcon(st))
	}
	// Todo icons.
	for _, st := range []agent.TodoStatus{
		agent.TodoPending, agent.TodoInProgress, agent.TodoCompleted,
	} {
		add("todo", todoStatusIcon(st))
	}
	// Per-row badges.
	add("badge", approvalBadge)
	add("badge", clarifyBadge)

	if set, ok := others[watcherGlyph]; ok {
		t.Errorf("watcher glyph %q collides with the %s glyph set", watcherGlyph, set)
	}
}

// --- watcherLabel -----------------------------------------------------------

// TestWatcherLabel covers the rendered watcher tree row: the leading ◷ glyph
// always present, a free-running watcher shown with its watcher:<name> session
// label, an attached watcher shown with the bare name, and the live ● busy marker
// inserted only while a fire runs.
func TestWatcherLabel(t *testing.T) {
	// Free-running, idle.
	free := watcherLabel(WatcherInfo{Name: "emailer", Free: true})
	if !strings.HasPrefix(free, watcherGlyph) {
		t.Errorf("watcher row must lead with the ◷ glyph: %q", free)
	}
	if !strings.Contains(free, "watcher:emailer") {
		t.Errorf("free-running label should use the watcher:<name> form: %q", free)
	}
	if strings.Contains(free, sessionStatusIcon(true)) {
		t.Errorf("idle watcher must not show the ● busy marker: %q", free)
	}

	// Attached, idle: bare name, no watcher: prefix.
	att := watcherLabel(WatcherInfo{Name: "gh", Free: false, TargetSession: "s1"})
	if !strings.HasPrefix(att, watcherGlyph) {
		t.Errorf("attached watcher row must lead with the ◷ glyph: %q", att)
	}
	if strings.Contains(att, "watcher:") {
		t.Errorf("attached label should use the bare name, not the watcher: form: %q", att)
	}
	if !strings.Contains(att, "gh") {
		t.Errorf("attached label should contain the name: %q", att)
	}

	// Running: the ● busy marker appears.
	busy := watcherLabel(WatcherInfo{Name: "gh", Free: false, Running: true})
	if !strings.Contains(busy, sessionStatusIcon(true)) {
		t.Errorf("running watcher should show the ● busy marker: %q", busy)
	}
}

// --- setWatchers reconciliation --------------------------------------------

// freeInfo / attInfo are tiny constructors for the two watcher kinds.
func freeInfo(id, name string, running bool) WatcherInfo {
	return WatcherInfo{ID: id, Name: name, Free: true, SessionID: "watcher:" + name, Running: running}
}
func attInfo(id, name, target string, running bool) WatcherInfo {
	return WatcherInfo{ID: id, Name: name, Free: false, TargetSession: target, SessionID: target, Running: running}
}

// TestSetWatchersFreeAreTopLevelRoots verifies a free-running watcher renders as a
// top-level root node in the tree (its own root, not nested under a session).
func TestSetWatchersFreeAreTopLevelRoots(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)

	changed := s.setWatchers([]WatcherInfo{freeInfo("w1", "emailer", false)}, nil)
	if !changed {
		t.Fatal("setWatchers should report a change when a root is added")
	}
	node, ok := s.watchers["w1"]
	if !ok {
		t.Fatal("free-running watcher node was not created")
	}
	if s.watcherParents["w1"] != "" {
		t.Errorf("free-running watcher parent = %q, want \"\" (top-level)", s.watcherParents["w1"])
	}
	// It must be among the tree roots, not a child of the session.
	if !isTreeRoot(s, node) {
		t.Error("free-running watcher node is not a top-level tree root")
	}
	if isChildOf(s.sessions["s1"], node) {
		t.Error("free-running watcher must not be nested under a session")
	}
}

// TestSetWatchersAttachedAreChildren verifies an attached watcher renders as a
// child of its target session's node (mirroring sub-agents), not as a root.
func TestSetWatchersAttachedAreChildren(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)

	s.setWatchers(nil, map[string][]WatcherInfo{"s1": {attInfo("w1", "gh", "s1", false)}})
	node, ok := s.watchers["w1"]
	if !ok {
		t.Fatal("attached watcher node was not created")
	}
	if s.watcherParents["w1"] != "s1" {
		t.Errorf("attached watcher parent = %q, want s1", s.watcherParents["w1"])
	}
	if !isChildOf(s.sessions["s1"], node) {
		t.Error("attached watcher node is not a child of its target session")
	}
	if isTreeRoot(s, node) {
		t.Error("attached watcher must not be a top-level root")
	}
}

// TestSetWatchersAttachedUnknownSessionSkipped verifies an attached watcher whose
// target session has no node yet is skipped (it appears once the session exists),
// rather than being orphaned as a root.
func TestSetWatchersAttachedUnknownSessionSkipped(t *testing.T) {
	s := newTestSidebar()
	// No session "ghost" added.
	changed := s.setWatchers(nil, map[string][]WatcherInfo{"ghost": {attInfo("w1", "gh", "ghost", false)}})
	if changed {
		t.Error("setWatchers should report no change when the only watcher is skipped")
	}
	if _, ok := s.watchers["w1"]; ok {
		t.Error("attached watcher under an unknown session must not be created")
	}
}

// TestSetWatchersRelabelInPlace verifies a busy-flip relabels the existing node in
// place (no churn): the same node object is reused, only its label changes, and
// the change is reported.
func TestSetWatchersRelabelInPlace(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.setWatchers([]WatcherInfo{freeInfo("w1", "emailer", false)}, nil)
	node := s.watchers["w1"]
	before := node.Label

	changed := s.setWatchers([]WatcherInfo{freeInfo("w1", "emailer", true)}, nil)
	if !changed {
		t.Error("a busy-flip should report a change")
	}
	if s.watchers["w1"] != node {
		t.Error("the node should be reused in place, not recreated")
	}
	if node.Label == before {
		t.Errorf("label should change on a busy-flip: still %q", node.Label)
	}
	if !strings.Contains(node.Label, sessionStatusIcon(true)) {
		t.Errorf("busy label should show the ● marker: %q", node.Label)
	}
}

// TestSetWatchersNoChangeWhenStable verifies a second identical reconcile reports
// no change, so the caller does not redraw on an idle tick.
func TestSetWatchersNoChangeWhenStable(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.setWatchers([]WatcherInfo{freeInfo("w1", "emailer", false)}, nil)
	if s.setWatchers([]WatcherInfo{freeInfo("w1", "emailer", false)}, nil) {
		t.Error("a stable reconcile should report no change")
	}
}

// TestSetWatchersRemovesVanished verifies a watcher dropped from the list is
// detached from the tree and its bookkeeping cleared.
func TestSetWatchersRemovesVanished(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.setWatchers([]WatcherInfo{freeInfo("w1", "emailer", false)}, nil)
	node := s.watchers["w1"]

	changed := s.setWatchers(nil, nil)
	if !changed {
		t.Error("removing the only watcher should report a change")
	}
	if _, ok := s.watchers["w1"]; ok {
		t.Error("vanished watcher should be dropped from the map")
	}
	if _, ok := s.watcherParents["w1"]; ok {
		t.Error("vanished watcher parent bookkeeping should be cleared")
	}
	if isTreeRoot(s, node) {
		t.Error("vanished watcher node should be detached from the tree roots")
	}
}

// TestSetWatchersRepointDetachesAndReattaches verifies a placement change (a
// watcher that flips from free-running root to attached child) detaches the node
// from its old container and re-adds it under the new one.
func TestSetWatchersRepointDetachesAndReattaches(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	// Start free-running (top-level).
	s.setWatchers([]WatcherInfo{freeInfo("w1", "x", false)}, nil)
	if s.watcherParents["w1"] != "" {
		t.Fatalf("expected top-level placement first")
	}
	// Now the same id arrives as attached under s1.
	s.setWatchers(nil, map[string][]WatcherInfo{"s1": {attInfo("w1", "x", "s1", false)}})
	if s.watcherParents["w1"] != "s1" {
		t.Errorf("after repoint parent = %q, want s1", s.watcherParents["w1"])
	}
	if isTreeRoot(s, s.watchers["w1"]) {
		t.Error("repointed watcher should no longer be a top-level root")
	}
	if !isChildOf(s.sessions["s1"], s.watchers["w1"]) {
		t.Error("repointed watcher should be a child of s1")
	}
}

// TestRemoveSessionDropsAttachedWatchers verifies closing a session drops its
// attached watcher bookkeeping (the nodes vanish with the parent), while a
// free-running watcher root is untouched.
func TestRemoveSessionDropsAttachedWatchers(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.setWatchers(
		[]WatcherInfo{freeInfo("free1", "emailer", false)},
		map[string][]WatcherInfo{"s1": {attInfo("att1", "gh", "s1", false)}},
	)

	s.removeSession("s1")

	if _, ok := s.watchers["att1"]; ok {
		t.Error("attached watcher bookkeeping should be dropped when its session closes")
	}
	if _, ok := s.watcherParents["att1"]; ok {
		t.Error("attached watcher parent bookkeeping should be dropped on session close")
	}
	if _, ok := s.watchers["free1"]; !ok {
		t.Error("free-running watcher root must survive an unrelated session close")
	}
}

// TestReorderPreservesWatcherRoots verifies reorder keeps free-running watcher
// roots (keyed on their watcher:<name> session id, which order never lists) at the
// tail rather than dropping them.
func TestReorderPreservesWatcherRoots(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "S1", false)
	s.addSession("s2", "S2", false)
	s.setWatchers([]WatcherInfo{freeInfo("w1", "emailer", false)}, nil)

	s.reorder([]string{"s2", "s1"})

	// Sessions reordered as requested; the watcher root still present.
	ids := sidebarIDs(s)
	if len(ids) < 2 || ids[0] != "s2" || ids[1] != "s1" {
		t.Errorf("session order = %v, want [s2 s1 ...]", ids)
	}
	if _, ok := s.watchers["w1"]; !ok || !isTreeRoot(s, s.watchers["w1"]) {
		t.Error("free-running watcher root was dropped by reorder")
	}
}

// --- free-running watcher / open-window dedup (fixes round 1) --------------

// TestSetWatchersSuppressesRootWhenSessionOpen verifies the dedup guard: when a
// free-running watcher's dedicated watcher:<name> session has an open window, the
// separate ◷ root is suppressed (the session row represents it) — no double entry.
func TestSetWatchersSuppressesRootWhenSessionOpen(t *testing.T) {
	s := newTestSidebar()
	s.setWatchers([]WatcherInfo{freeInfo("w1", "emailer", false)}, nil)
	if _, ok := s.watchers["w1"]; !ok {
		t.Fatal("precondition: ◷ root should exist while the window is closed")
	}
	// The watcher's session window opens (its id is "watcher:emailer").
	s.addSession("watcher:emailer", "watcher:emailer", false)

	s.setWatchers([]WatcherInfo{freeInfo("w1", "emailer", false)}, nil)
	if _, ok := s.watchers["w1"]; ok {
		t.Error("◷ root should be suppressed once the watcher's session window is open")
	}
	// Exactly one top-level row mentions the watcher (the session row).
	rows := 0
	for _, n := range s.tree.Roots {
		if strings.Contains(n.Label, "watcher:emailer") {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("want exactly 1 top-level row for the open watcher, got %d", rows)
	}
}

// TestSetWatchersRootReappearsAfterSessionClose verifies the other half of the
// dedup contract the comment promises: closing the watcher's session window brings
// the ◷ root back.
func TestSetWatchersRootReappearsAfterSessionClose(t *testing.T) {
	s := newTestSidebar()
	s.addSession("watcher:emailer", "watcher:emailer", false)
	s.setWatchers([]WatcherInfo{freeInfo("w1", "emailer", false)}, nil)
	if _, ok := s.watchers["w1"]; ok {
		t.Fatal("precondition: ◷ root suppressed while the window is open")
	}

	s.removeSession("watcher:emailer")
	s.setWatchers([]WatcherInfo{freeInfo("w1", "emailer", false)}, nil)
	if _, ok := s.watchers["w1"]; !ok {
		t.Error("◷ root should reappear after the watcher's session window closes")
	}
}

// TestSetWatchersAttachedNotSuppressedByOpenSession is the negative guard: the
// dedup is for FREE-running watchers only. An attached watcher's SessionID is its
// (open) target session, but it must still render as a ◷ CHILD — the open target
// must not suppress it.
func TestSetWatchersAttachedNotSuppressedByOpenSession(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.setWatchers(nil, map[string][]WatcherInfo{"s1": {attInfo("a1", "gh", "s1", false)}})
	if _, ok := s.watchers["a1"]; !ok {
		t.Error("an attached watcher must render even though its target session is open")
	}
	if !isChildOf(s.sessions["s1"], s.watchers["a1"]) {
		t.Error("attached watcher should be a child of its open target session")
	}
}

// --- refreshWatcherNodes (tui.go integration) ------------------------------

// TestRefreshWatcherNodesNilHandler verifies the no-op guard: with no
// ListWatchers wired, refreshWatcherNodes reports no change (the sidebar renders
// no watcher nodes).
func TestRefreshWatcherNodesNilHandler(t *testing.T) {
	w := newTestWorkbench(t)
	if w.refreshWatcherNodes() {
		t.Error("refreshWatcherNodes with no handler should report no change")
	}
}

// TestRefreshWatcherNodesSplitsFreeAndAttached drives the full path tui.go uses:
// the free set is fetched with the empty session id and each open session is
// queried for its own attached watchers, then reconciled. A free-running watcher
// lands as a top-level root and an attached one nests under its target session.
func TestRefreshWatcherNodesSplitsFreeAndAttached(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s1", "S1")
	w.openWindow("s2", "S2")

	w.SetHandlers(Handlers{
		ListWatchers: func(id string) []WatcherInfo {
			switch id {
			case "": // free-running, visible to all
				return []WatcherInfo{freeInfo("free1", "emailer", false)}
			case "s1":
				return []WatcherInfo{
					freeInfo("free1", "emailer", false), // free leaks into the per-session list
					attInfo("att1", "gh", "s1", false),  // s1's own attached watcher
					attInfo("other", "x", "s2", false),  // another session's — must be ignored for s1
				}
			case "s2":
				return []WatcherInfo{freeInfo("free1", "emailer", false)}
			}
			return nil
		},
	})

	if !w.refreshWatcherNodes() {
		t.Fatal("first refresh should report a change")
	}
	s := w.sidebar
	// Free-running watcher is a top-level root.
	if n := s.watchers["free1"]; n == nil || s.watcherParents["free1"] != "" || !isTreeRoot(s, n) {
		t.Errorf("free-running watcher not rendered as a top-level root (parent=%q)", s.watcherParents["free1"])
	}
	// Attached watcher nests under s1.
	if n := s.watchers["att1"]; n == nil || s.watcherParents["att1"] != "s1" || !isChildOf(s.sessions["s1"], n) {
		t.Errorf("attached watcher not nested under s1 (parent=%q)", s.watcherParents["att1"])
	}
	// The other session's attached watcher must NOT be attributed to s1 (the filter
	// keys on TargetSession==sid). It belongs to s2, which did not report it, so it
	// should not appear at all.
	if _, ok := s.watchers["other"]; ok {
		t.Error("an attached watcher whose target is not the queried session leaked into the tree")
	}
}

// TestRefreshWatcherNodesStableSecondCall verifies a second identical refresh is a
// no-op (no redundant redraw on an idle status tick).
func TestRefreshWatcherNodesStableSecondCall(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s1", "S1")
	w.SetHandlers(Handlers{
		ListWatchers: func(id string) []WatcherInfo {
			if id == "s1" {
				return []WatcherInfo{attInfo("att1", "gh", "s1", false)}
			}
			return nil
		},
	})
	if !w.refreshWatcherNodes() {
		t.Fatal("first refresh should report a change")
	}
	if w.refreshWatcherNodes() {
		t.Error("a stable second refresh should report no change")
	}
}

// --- helpers ----------------------------------------------------------------

func isTreeRoot(s *sidebar, node *tv.TreeNode) bool {
	for _, r := range s.tree.Roots {
		if r == node {
			return true
		}
	}
	return false
}

func isChildOf(parent, node *tv.TreeNode) bool {
	if parent == nil {
		return false
	}
	for _, c := range parent.Children {
		if c == node {
			return true
		}
	}
	return false
}
