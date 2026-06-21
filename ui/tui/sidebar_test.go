package ui

import (
	"strings"
	"testing"
)

// newTestSidebar builds a sidebar detached from any live desktop. The workbench
// is only referenced by the (uninvoked) select/activate callbacks, so a bare one
// is enough to exercise the node-label and approval-badge bookkeeping.
func newTestSidebar() *sidebar { return newSidebar(&Workbench{}) }

// TestSessionLabelBadge covers the rendered session row across the pin/approval
// combinations: the ⏳ badge is appended only when a prompt is pending, and it
// trails the title so it cannot shift the status icon or ★ marker.
func TestSessionLabelBadge(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pinned        bool
		pending       bool
		wantBadge     bool
		wantStarFirst bool
	}{
		{name: "plain"},
		{name: "pinned", pinned: true, wantStarFirst: true},
		{name: "pending", pending: true, wantBadge: true},
		{name: "pinned+pending", pinned: true, pending: true, wantBadge: true, wantStarFirst: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Idle (busy=false): the leading glyph is the ○ session marker (issue #236).
			label := sessionLabel("Session 1", false, tc.pinned, tc.pending, false)
			if got := strings.Contains(label, approvalBadge); got != tc.wantBadge {
				t.Fatalf("badge presence = %v, want %v (label %q)", got, tc.wantBadge, label)
			}
			if tc.wantBadge && !strings.HasSuffix(label, approvalBadge) {
				t.Fatalf("badge should trail the label, got %q", label)
			}
			if tc.wantStarFirst && !strings.Contains(label, "★") {
				t.Fatalf("expected ★ marker in %q", label)
			}
		})
	}
}

// TestSidebarApprovalBadge verifies setApproval toggles the node badge and that
// the pending state survives an unrelated relabel (rename/pin), matching issue
// #55's "badge stays until the prompt is answered".
func TestSidebarApprovalBadge(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)

	node := s.sessions["s1"]
	if strings.Contains(node.Label, approvalBadge) {
		t.Fatalf("new session should not be badged: %q", node.Label)
	}

	s.setApproval("s1", "Session 1", false, true)
	if !strings.Contains(node.Label, approvalBadge) {
		t.Fatalf("expected badge after setApproval(pending): %q", node.Label)
	}

	// A rename must preserve the pending badge.
	s.relabelSession("s1", "Renamed", false)
	if !strings.Contains(node.Label, approvalBadge) {
		t.Fatalf("badge lost across relabel: %q", node.Label)
	}
	if !strings.Contains(node.Label, "Renamed") {
		t.Fatalf("relabel did not apply new title: %q", node.Label)
	}

	s.setApproval("s1", "Renamed", false, false)
	if strings.Contains(node.Label, approvalBadge) {
		t.Fatalf("badge should clear once resolved: %q", node.Label)
	}
}

// TestSidebarRemoveClearsApproval ensures a closed session does not leak its
// pending-approval state into the badge set.
func TestSidebarRemoveClearsApproval(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.setApproval("s1", "Session 1", false, true)
	s.removeSession("s1")
	if s.approvals["s1"] {
		t.Fatal("approval state leaked after removeSession")
	}
}

// TestSidebarGlobalApprovals was removed: it exercised setGlobalApprovals /
// globalApprovals, which issue #230 deleted (the phantom glyph-less header counter
// is gone — attention is shown only per row). The #230 regression coverage now lives
// in sidebar_busy_test.go (headerHasNoGlobalCount / TestHeaderNoGlobalCount*).
