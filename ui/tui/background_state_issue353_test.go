package ui

import (
	"strings"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/config"
)

const backgroundMark = "◐"

func TestIssue353SessionStatusGlyphTriState(t *testing.T) {
	if got := sessionStatusGlyph(false, false); got != idleMark {
		t.Fatalf("idle glyph = %q, want %q", got, idleMark)
	}
	if got := sessionStatusGlyph(false, true); got != backgroundMark {
		t.Fatalf("background glyph = %q, want %q", got, backgroundMark)
	}
	if got := sessionStatusGlyph(true, true); got != activeMark {
		t.Fatalf("busy+background glyph = %q, want busy to dominate with %q", got, activeMark)
	}
	for _, g := range append(subAgentGlyphs, idleMark, activeMark) {
		if backgroundMark == g {
			t.Fatalf("background marker %q collides with existing glyph %q", backgroundMark, g)
		}
	}
}

func TestIssue353SidebarBackgroundMarkerSurvivesRelabelAndBadges(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.setBackground("s1", "Session 1", false, true)
	if got := s.sessions["s1"].Label; !strings.HasPrefix(got, backgroundMark) {
		t.Fatalf("background session label = %q, want leading %q", got, backgroundMark)
	}
	if !s.background["s1"] {
		t.Fatal("setBackground(true) did not record the background map entry")
	}

	s.setApproval("s1", "Session 1", false, true)
	s.setClarify("s1", "Session 1", false, true)
	s.relabelSession("s1", "Renamed", true)
	got := s.sessions["s1"].Label
	for _, want := range []string{backgroundMark, "★", "Renamed", approvalBadge, clarifyBadge} {
		if !strings.Contains(got, want) {
			t.Fatalf("background relabel dropped %q from %q", want, got)
		}
	}
	if !strings.HasPrefix(got, backgroundMark) {
		t.Fatalf("background marker should remain leading after relabel, got %q", got)
	}

	s.setBusy("s1", "Renamed", true, true)
	if got := s.sessions["s1"].Label; !strings.HasPrefix(got, activeMark) {
		t.Fatalf("busy should dominate background marker, got %q", got)
	}
	s.setBusy("s1", "Renamed", true, false)
	if got := s.sessions["s1"].Label; !strings.HasPrefix(got, backgroundMark) {
		t.Fatalf("clearing busy should restore background marker, got %q", got)
	}
	s.setBackground("s1", "Renamed", true, false)
	if got := s.sessions["s1"].Label; !strings.HasPrefix(got, idleMark) {
		t.Fatalf("clearing background should restore idle marker, got %q", got)
	}
	if s.background["s1"] {
		t.Fatal("setBackground(false) should clear the background map entry")
	}
}

func TestIssue353SessionWindowBackgroundStateDefersIdleEdge(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	var submitted int
	sw.submitFn = func() { submitted++ }

	sw.pending = "queued while work continues"
	sw.setBusy(true)
	sw.apply(agent.SessionEvent{Type: agent.SessionEventBackground, Background: true})
	sw.apply(agent.SessionEvent{Type: agent.SessionEventFinal, Text: "foreground done"})

	if !sw.background {
		t.Fatal("background flag should remain set after foreground final")
	}
	if sw.busy {
		t.Fatal("foreground busy flag should be cleared by final event")
	}
	if sw.statusState != backgroundStatusText {
		t.Fatalf("statusState = %q, want %q", sw.statusState, backgroundStatusText)
	}
	if submitted != 0 {
		t.Fatalf("queued input drained while background work was still running: submitted=%d", submitted)
	}
	if sw.pending == "" {
		t.Fatal("pending queue was cleared before the session was truly idle")
	}

	sw.apply(agent.SessionEvent{Type: agent.SessionEventBackground, Background: false})
	if sw.background || sw.busy {
		t.Fatalf("after background clear got busy=%v background=%v, want fully idle", sw.busy, sw.background)
	}
	if sw.statusState != "idle" {
		t.Fatalf("statusState = %q, want idle", sw.statusState)
	}
	if submitted != 1 {
		t.Fatalf("queued input should drain exactly once on true idle edge, submitted=%d", submitted)
	}
	if sw.pending != "" {
		t.Fatalf("pending queue should be empty after true idle edge, got %q", sw.pending)
	}
}

func TestIssue353BackgroundStatusColorUsesThemeRole(t *testing.T) {
	got := statusColorFor(false, true, agent.SessionStats{}, config.BudgetConfig{})
	if got != colorInfo {
		t.Fatalf("background status colour = %+v, want colorInfo theme role %+v", got, colorInfo)
	}
	if got == colorNote {
		t.Fatalf("background status colour reused idle colour %+v", got)
	}
	busy := statusColorFor(true, true, agent.SessionStats{}, config.BudgetConfig{})
	if busy == colorInfo {
		t.Fatalf("busy+background should keep foreground busy colour, got background role %+v", busy)
	}
}
