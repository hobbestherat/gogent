package ui

import (
	"testing"

	"gogent/internal/config"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Issue #302: a single mouse click on a sub-agent sidebar entry must open its
// monologue popup (via OnSelectMouse), a click on a session entry must not, and
// keyboard selection (OnSelect) must never pop a window during traversal.
func TestSidebarSubAgentMouseClickOpensMonolog(t *testing.T) {
	w := NewWorkbench([]*config.ModelConfig{{Name: "m", Model: "m"}})
	fetched := 0
	w.SetHandlers(Handlers{
		GetTranscript: func(sessionID, agentID string) []ChatMessage {
			fetched++
			return []ChatMessage{{Role: "assistant", Content: "sub-agent output"}}
		},
	})
	s := w.sidebar

	agentNode := &tv.TreeNode{Data: nodeRef{sessionID: "s1", agentID: "a1", name: "counter"}}
	sessNode := &tv.TreeNode{Data: nodeRef{sessionID: "s1", name: "Session 1"}}

	// Single mouse click on a sub-agent → monologue opens.
	s.tree.OnSelectMouse(agentNode)
	if w.monolog == nil {
		t.Fatalf("OnSelectMouse on a sub-agent node should open the monologue popup")
	}
	if fetched == 0 {
		t.Fatalf("the sub-agent transcript should have been fetched")
	}

	// Mouse click on a session node → must NOT open a monologue.
	if w.monolog != nil {
		w.desktop.RemoveLayer(w.monolog)
		w.monolog = nil
	}
	s.tree.OnSelectMouse(sessNode)
	if w.monolog != nil {
		t.Fatalf("OnSelectMouse on a session node must not open a monologue")
	}

	// Keyboard selection (OnSelect) onto a sub-agent must NOT pop a window — it
	// only fires on traversal; the popup is reached by Enter (OnActivate) instead.
	s.tree.OnSelect(agentNode)
	if w.monolog != nil {
		t.Fatalf("OnSelect (keyboard) on a sub-agent must not open a monologue")
	}

	// Enter (OnActivate) on a sub-agent still opens it (the keyboard path).
	s.tree.OnActivate(agentNode)
	if w.monolog == nil {
		t.Fatalf("OnActivate (Enter) on a sub-agent should open the monologue")
	}
}
