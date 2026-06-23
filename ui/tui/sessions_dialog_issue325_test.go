package ui

import (
	"strings"
	"testing"
)

// Issue #325: the browser must let the user tell a closed (archived) session from
// an open one. formatSessionRow marks archived rows; formatSessionDetail surfaces
// the archived status in the side pane.

// TestFormatSessionRowMarksArchived proves only archived rows carry the
// "(archived)" marker, and that the marker is purely a function of the Archived
// flag (same title/date/counts otherwise).
func TestFormatSessionRowMarksArchived(t *testing.T) {
	active := SessionMeta{ID: "s1", Title: "Bug hunt", CreatedAt: "2026-06-19T12:00:00Z", Turns: 3, Messages: 8}
	archived := active
	archived.Archived = true

	activeRow := formatSessionRow(active)
	archivedRow := formatSessionRow(archived)

	if strings.Contains(activeRow, "archived") {
		t.Errorf("active row should not mention archived: %q", activeRow)
	}
	if !strings.Contains(archivedRow, "(archived)") {
		t.Errorf("archived row missing (archived) marker: %q", archivedRow)
	}
	// The archived row is the active row plus the marker — the shared prefix
	// (title/date/turns/messages) must be identical so columns still line up.
	if !strings.HasPrefix(archivedRow, activeRow) {
		t.Errorf("archived row %q should extend the active row %q, not replace it", archivedRow, activeRow)
	}
}

// TestFormatSessionDetailMarksArchived proves the side-pane detail reports the
// archived status only for archived sessions.
func TestFormatSessionDetailMarksArchived(t *testing.T) {
	active := SessionMeta{ID: "s1", Title: "Open one", CreatedAt: "2026-06-19T12:00:00Z"}
	archived := active
	archived.Archived = true

	if d := formatSessionDetail(active); strings.Contains(strings.ToLower(d), "archived") {
		t.Errorf("active detail should not mention archived:\n%s", d)
	}
	d := formatSessionDetail(archived)
	if !strings.Contains(strings.ToLower(d), "archived") {
		t.Errorf("archived detail missing archived status:\n%s", d)
	}
}

// TestFormatSessionRowArchivedStillShowsTitle guards against the marker eating
// the title/counts: an archived row must still contain its title text and the
// "Nt Nm" turn/message summary.
func TestFormatSessionRowArchivedStillShowsContent(t *testing.T) {
	row := formatSessionRow(SessionMeta{Title: "Refactor", CreatedAt: "2026-06-19T12:00:00Z", Turns: 5, Messages: 12, Archived: true})
	if !strings.Contains(row, "Refactor") {
		t.Errorf("archived row dropped the title: %q", row)
	}
	if !strings.Contains(row, "5t") || !strings.Contains(row, "12m") {
		t.Errorf("archived row dropped the turn/message counts: %q", row)
	}
	if !strings.Contains(row, "(archived)") {
		t.Errorf("archived row missing marker: %q", row)
	}
}
