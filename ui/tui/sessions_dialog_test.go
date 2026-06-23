package ui

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// TestFilterSessions covers case-insensitive matching across title/id/model,
// the all-match empty query, and the no-match case.
func TestFilterSessions(t *testing.T) {
	items := []SessionMeta{
		{ID: "session-1", Title: "Bug hunt", Model: "gpt-4"},
		{ID: "session-2", Title: "Refactor", Model: "claude"},
		{ID: "session-3", Title: "notes", Model: "gpt-4o"},
	}
	for _, tc := range []struct {
		name  string
		query string
		want  []string // ids
	}{
		{"empty returns all", "", []string{"session-1", "session-2", "session-3"}},
		{"whitespace is empty", "   ", []string{"session-1", "session-2", "session-3"}},
		{"title match case-insensitive", "BUG", []string{"session-1"}},
		{"model match", "claude", []string{"session-2"}},
		{"model groups both gpt", "gpt", []string{"session-1", "session-3"}},
		{"id match", "session-3", []string{"session-3"}},
		{"no match", "zzz", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := filterSessions(items, tc.query)
			ids := make([]string, 0, len(got))
			for _, m := range got {
				ids = append(ids, m.ID)
			}
			if !sameStrings(ids, tc.want) {
				t.Errorf("filterSessions(%q) = %v, want %v", tc.query, ids, tc.want)
			}
		})
	}
}

// TestSortSessionsNewestFirst covers descending-date ordering with an id
// tie-break for sessions sharing a timestamp.
func TestSortSessionsNewestFirst(t *testing.T) {
	items := []SessionMeta{
		{ID: "old", CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "new", CreatedAt: "2026-06-19T12:00:00Z"},
		{ID: "mid-b", CreatedAt: "2026-03-01T00:00:00Z"},
		{ID: "mid-a", CreatedAt: "2026-03-01T00:00:00Z"},
	}
	sortSessionsNewestFirst(items)
	got := make([]string, len(items))
	for i, m := range items {
		got[i] = m.ID
	}
	want := []string{"new", "mid-a", "mid-b", "old"}
	if !sameStrings(got, want) {
		t.Errorf("sortSessionsNewestFirst = %v, want %v", got, want)
	}
}

// TestLoadSessionItems covers the nil-getter guard and that the result is sorted
// newest first regardless of the input order.
func TestLoadSessionItems(t *testing.T) {
	if got := loadSessionItems(nil); got != nil {
		t.Errorf("loadSessionItems(nil) = %v, want nil", got)
	}
	get := func() []SessionMeta {
		return []SessionMeta{
			{ID: "old", CreatedAt: "2026-01-01T00:00:00Z"},
			{ID: "new", CreatedAt: "2026-06-19T00:00:00Z"},
		}
	}
	items := loadSessionItems(get)
	if len(items) != 2 || items[0].ID != "new" || items[1].ID != "old" {
		t.Errorf("expected newest-first [new,old], got %+v", items)
	}
}

// TestFormatSessionRow covers the padded title, the date and the turn/message
// counts in a single deterministic line.
func TestFormatSessionRow(t *testing.T) {
	m := SessionMeta{Title: "Bug hunt", CreatedAt: "2026-06-19T12:04:05Z", Turns: 3, Messages: 9}
	want := padName("Bug hunt", sessionRowTitleWidth) + " 2026-06-19 12:04  3t 9m"
	if got := formatSessionRow(m); got != want {
		t.Errorf("formatSessionRow = %q, want %q", got, want)
	}

	// An empty title falls back to the id.
	row := formatSessionRow(SessionMeta{ID: "session-1", CreatedAt: "2026-06-19T12:04:05Z"})
	if !strings.Contains(row, "session-1") {
		t.Errorf("expected id fallback in row: %q", row)
	}
}

// TestFormatSessionDetail covers every metadata field and that an empty model
// is omitted.
func TestFormatSessionDetail(t *testing.T) {
	m := SessionMeta{
		ID: "session-7", Title: "Session 7", CreatedAt: "2026-06-19T12:04:05Z",
		Turns: 4, Messages: 12, TokensIn: 1500, TokensOut: 800, Model: "gpt-test",
	}
	got := formatSessionDetail(m)
	for _, want := range []string{
		"Title: Session 7",
		"ID: session-7",
		"Created: 2026-06-19 12:04",
		"Turns: 4",
		"Messages: 12",
		"Tokens: 1.5k / 800",
		"Model: gpt-test",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatSessionDetail missing %q in:\n%s", want, got)
		}
	}

	// An empty model line is omitted.
	noModel := formatSessionDetail(SessionMeta{ID: "s", Title: "s", CreatedAt: "2026-06-19T12:04:05Z"})
	if strings.Contains(noModel, "Model:") {
		t.Errorf("empty model should be omitted:\n%s", noModel)
	}
}

// TestFormatSessionDate covers RFC3339 parsing, the empty case and the
// unparseable fallback.
func TestFormatSessionDate(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"2026-06-19T12:04:05Z", "2026-06-19 12:04"},
		{"", "unknown"},
		{"not-a-date", "not-a-date"},
	} {
		if got := formatSessionDate(tc.in); got != tc.want {
			t.Errorf("formatSessionDate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEmptySessionsDetail covers the no-sessions invitation and the search
// no-match note.
func TestEmptySessionsDetail(t *testing.T) {
	if got := emptySessionsDetail(0, ""); !strings.Contains(got, "No saved sessions") {
		t.Errorf("expected invitation for no sessions, got %q", got)
	}
	if got := emptySessionsDetail(0, "abc"); got != "No matching sessions." {
		t.Errorf("expected no-match note, got %q", got)
	}
}

// TestSessionsDialogSize covers the content-driven sizing the Saved Sessions
// browser adopted in issue #322. Unlike the shared browserDialogSpec (an 85%
// terminal-share that resolves to the 80%×85% balloon — 160×42 on 200×50), the
// dedicated sessionsDialogSpec is a fixed content footprint: PreferredW 90 / PrefH
// 20, capped at 120×30 and floored at 60×14. So it sizes to its content and STAYS
// there — it never balloons to fill a wide terminal, which is the whole point of
// the fix. This test drives the REAL w.sessionsDialogSpec() (not an inline mirror)
// so a drift in the source spec — including the PrefH-vs-PreferredH field-name trap
// the issue snippet warns about — is caught here.
func TestSessionsDialogSize(t *testing.T) {
	spec := newTestWorkbench(t).sessionsDialogSpec()
	for _, tc := range []struct {
		name             string
		screenW, screenH int
		wantW, wantH     int
	}{
		// PreferredW 90 sits below the 80% cap on a roomy terminal, so the dialog is
		// its content size (90×20) — NOT the old 160×42 balloon.
		{"roomy terminal sizes to content not the balloon", 200, 50, 90, 20},
		// And it does not keep growing on an ultrawide terminal: still 90×20.
		{"ultrawide stays at content size", 300, 80, 90, 20},
		// On a narrow-ish terminal the 80% width cap (64) bites before PreferredW.
		{"medium terminal width capped at 80%", 80, 24, 64, 20},
		// A mid terminal is wide enough for the full PreferredW.
		{"mid terminal honours preferred width", 120, 40, 90, 20},
		// Tiny terminal: both floors win (MinW 60, MinH 14), even past the edge.
		{"small terminal floors both", 50, 16, 60, 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, gotW, gotH := tv.ResolveDialogRect(spec, tc.screenW, tc.screenH)
			if gotW != tc.wantW || gotH != tc.wantH {
				t.Errorf("sessions size(%d,%d) = %dx%d, want %dx%d",
					tc.screenW, tc.screenH, gotW, gotH, tc.wantW, tc.wantH)
			}
		})
	}

	// The crux of #322: on a roomy terminal it must NOT be the 160×42 percentage
	// balloon that motivated the issue.
	_, _, bw, bh := tv.ResolveDialogRect(spec, 200, 50)
	if bw >= 160 || bh >= 42 {
		t.Errorf("sessions dialog on 200x50 = %dx%d — it still balloons toward the 160x42 box (#322)", bw, bh)
	}
}
