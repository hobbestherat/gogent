package ui

// Issue #560 — a remote-approval outcome is surfaced in-band as a
// SessionEventNotice, which SessionWindow.apply renders as a "[System]" transcript
// line (kindSystem) via the existing addNote path. These tests pin that rendering
// and two invariants the fix relies on: a whitespace-only notice is dropped (no
// empty [System] line), and a notice never clears the busy state (it is
// informational, unlike a final/error event).

import (
	"strings"
	"testing"

	"gogent/internal/agent"
)

func TestSessionEventNoticeRendersSystemNote(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	sw.apply(agent.SessionEvent{Type: agent.SessionEventNotice, Text: "your always allow for example.com was saved"})

	all := sw.transcript.view.AllText()
	if !strings.Contains(all, "[System]") {
		t.Errorf("notice did not render a [System] header; transcript=%q", all)
	}
	if !strings.Contains(all, "always allow for example.com was saved") {
		t.Errorf("notice text missing from transcript; transcript=%q", all)
	}
	if n := len(sw.transcript.records); n == 0 || sw.transcript.records[n-1].kind != kindSystem {
		t.Errorf("last transcript record kind = %v, want kindSystem", safeKind(sw))
	}
}

func TestSessionEventNoticeDropsWhitespaceOnlyText(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	before := len(sw.transcript.records)
	sw.apply(agent.SessionEvent{Type: agent.SessionEventNotice, Text: "   \t  "})

	if got := len(sw.transcript.records); got != before {
		t.Errorf("whitespace-only notice added a record (%d → %d); it should be dropped", before, got)
	}
}

func TestSessionEventNoticeDoesNotClearBusy(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.setBusy(true)

	sw.apply(agent.SessionEvent{Type: agent.SessionEventNotice, Text: "late grant saved"})

	if !sw.busy {
		t.Error("an informational notice cleared the busy marker; only final/error events should")
	}
}

// safeKind reads the last record's kind without crashing when there are no records.
func safeKind(sw *SessionWindow) any {
	rs := sw.transcript.records
	if len(rs) == 0 {
		return "<no records>"
	}
	return rs[len(rs)-1].kind
}
