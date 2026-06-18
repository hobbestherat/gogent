package ui

import (
	"strings"
	"testing"

	"gogent/internal/permission"
)

// approvalBadge is the prefix sessionLabel adds for a session awaiting a
// permission decision (issue #55).
const approvalBadge = "⏳"

// TestSidebarApprovalBadge verifies the ⏳ badge is added to and cleared from a
// session's sidebar label, and that the global indicator tracks the count.
func TestSidebarApprovalBadge(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B")

	s := w.sidebar
	if changed := s.setApprovalPending("a", true); !changed {
		t.Fatal("expected first setApprovalPending to report a change")
	}
	if !strings.Contains(s.sessions["a"].Label, approvalBadge) {
		t.Errorf("session a missing approval badge: %q", s.sessions["a"].Label)
	}
	if strings.Contains(s.sessions["b"].Label, approvalBadge) {
		t.Errorf("session b should have no badge: %q", s.sessions["b"].Label)
	}
	// Setting the same state again is a no-op (no redundant redraw).
	if s.setApprovalPending("a", true) {
		t.Error("re-setting the same approval state should report no change")
	}

	// Unknown sessions are ignored.
	if s.setApprovalPending("missing", true) {
		t.Error("setApprovalPending on an unknown session should report no change")
	}

	// Clearing removes the badge.
	if changed := s.setApprovalPending("a", false); !changed {
		t.Fatal("expected clearing to report a change")
	}
	if strings.Contains(s.sessions["a"].Label, approvalBadge) {
		t.Errorf("approval badge not cleared: %q", s.sessions["a"].Label)
	}

	if !s.setGlobalApprovals(3) {
		t.Fatal("expected global count change")
	}
	if s.setGlobalApprovals(3) {
		t.Error("re-setting the same global count should report no change")
	}
}

// TestApprovalBadgeSurvivesRelabel verifies a pending badge persists when the
// session is renamed or pinned (the label is re-rendered from the live state).
func TestApprovalBadgeSurvivesRelabel(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.sidebar.setApprovalPending("a", true)

	w.SetSessionTitle("a", "Renamed")
	if l := w.sidebar.sessions["a"].Label; !strings.Contains(l, approvalBadge) || !strings.Contains(l, "Renamed") {
		t.Errorf("badge or title lost on rename: %q", l)
	}

	w.TogglePin("a")
	if l := w.sidebar.sessions["a"].Label; !strings.Contains(l, approvalBadge) || !strings.Contains(l, "★") {
		t.Errorf("badge or pin marker lost on pin: %q", l)
	}
}

// TestUpdateApprovalBadges reconciles the sidebar with the pending requests from
// the wired approval source.
func TestUpdateApprovalBadges(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B")

	pending := []permission.Request{}
	w.SetApprovalSource(func() []permission.Request { return pending })

	// One request for b: only b is badged, global count is 1.
	pending = []permission.Request{{Action: permission.ActionShell, Session: "b"}}
	w.updateApprovalBadges()
	if strings.Contains(w.sidebar.sessions["a"].Label, approvalBadge) {
		t.Errorf("session a should not be badged: %q", w.sidebar.sessions["a"].Label)
	}
	if !strings.Contains(w.sidebar.sessions["b"].Label, approvalBadge) {
		t.Errorf("session b should be badged: %q", w.sidebar.sessions["b"].Label)
	}
	if w.sidebar.pending != 1 {
		t.Errorf("global count = %d, want 1", w.sidebar.pending)
	}

	// Request answered: badge and count clear.
	pending = nil
	w.updateApprovalBadges()
	if strings.Contains(w.sidebar.sessions["b"].Label, approvalBadge) {
		t.Errorf("badge not cleared after request answered: %q", w.sidebar.sessions["b"].Label)
	}
	if w.sidebar.pending != 0 {
		t.Errorf("global count = %d, want 0", w.sidebar.pending)
	}
}

// TestRequesterLabel resolves the requesting session's title and (non-root)
// agent for the prompt's "Requested by" line.
func TestRequesterLabel(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.SetSessionTitle("a", "Refactor")

	tests := []struct {
		name string
		req  permission.Request
		want string
	}{
		{"session only", permission.Request{Session: "a", Agent: "root"}, "Refactor"},
		{"named sub-agent", permission.Request{Session: "a", Agent: "researcher"}, "Refactor · researcher"},
		{"unknown session falls back to id", permission.Request{Session: "ghost"}, "ghost"},
		{"no attribution", permission.Request{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.requesterLabel(tc.req); got != tc.want {
				t.Fatalf("requesterLabel = %q, want %q", got, tc.want)
			}
		})
	}
}
