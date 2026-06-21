package ui

import "testing"

// #206: the sidebar tree highlight must follow the active session on every
// focus-change path (new/cycle/close/menu), not just tree-internal navigation.
func TestSidebarHighlightFollowsFocus(t *testing.T) {
	s := newTestSidebar()
	s.addSession("a", "Alpha", false)
	s.addSession("b", "Bravo", false)
	s.addSession("c", "Charlie", false)

	s.focusSession("b")
	if got := s.tree.Selected(); got == nil || got != s.sessions["b"] {
		t.Fatalf("after focusSession(b), tree selection = %v, want Bravo node", got)
	}
	s.focusSession("c")
	if got := s.tree.Selected(); got != s.sessions["c"] {
		t.Fatalf("after focusSession(c), tree selection = %v, want Charlie node", got)
	}
	// Unknown id is a safe no-op (selection unchanged).
	s.focusSession("missing")
	if got := s.tree.Selected(); got != s.sessions["c"] {
		t.Errorf("focusSession(unknown) changed selection to %v", got)
	}
}
