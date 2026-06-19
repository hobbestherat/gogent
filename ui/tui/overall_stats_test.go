package ui

import (
	"strings"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/agent"
	"gogent/internal/stats"
)

// TestBuildOverallStats covers the mapping from a Statistics report + the
// sidebar's own node counts into the panel's typed view, including the
// whole-number cache-hit percentage derived from the connector snapshot.
func TestBuildOverallStats(t *testing.T) {
	report := stats.Report{Totals: stats.Totals{
		Primary: stats.ConnectorStat{
			Requests: 42, Errors: 3, TokensIn: 1000, TokensOut: 500, CachedTokensIn: 250,
		},
		// The auxiliary (fast/compression) backend must NOT bleed into the headline
		// totals, to avoid double counting.
		Fast: stats.ConnectorStat{Requests: 9, TokensIn: 9999, TokensOut: 9999},
	}}
	got := buildOverallStats(report, 3, 5)
	want := overallStats{Sessions: 3, SubAgents: 5, TokensIn: 1000, TokensOut: 500,
		Requests: 42, Errors: 3, CacheHitPct: 25}
	if got != want {
		t.Fatalf("buildOverallStats = %+v, want %+v", got, want)
	}
}

// TestBuildOverallStatsEmpty verifies a zero report yields a safe zero view (the
// panel's first frame before any traffic, and the "no statistics handler" path).
func TestBuildOverallStatsEmpty(t *testing.T) {
	got := buildOverallStats(stats.Report{}, 0, 0)
	if got != (overallStats{}) {
		t.Fatalf("empty report should yield zero view, got %+v", got)
	}
}

// TestFormatOverallStats pins the rendered metric rows: label column alignment,
// compact token formatting and the cache-hit percentage. Exact lines document the
// alignment contract the band relies on.
func TestFormatOverallStats(t *testing.T) {
	got := formatOverallStats(overallStats{
		Sessions: 3, SubAgents: 5, TokensIn: 1234, TokensOut: 567,
		Requests: 42, Errors: 0, CacheHitPct: 25,
	})
	want := []string{
		"sessions   3",
		"sub-agents 5",
		"tokens in  1.2k",
		"tokens out 567",
		"requests   42",
		"errors     0",
		"cache hit  25%",
	}
	if len(got) != len(want) {
		t.Fatalf("formatOverallStats produced %d lines, want %d (%q)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("line %d = %q, want %q", i, got[i], w)
		}
	}
}

// TestFormatOverallStatsTokenSuffixes covers the compact token formatting at the
// thousand / million boundaries, exercised through the formatter.
func TestFormatOverallStatsTokenSuffixes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tokensIn   int
		wantInLine string
	}{
		{"under a thousand", 999, "tokens in  999"},
		{"exact thousand", 1000, "tokens in  1.0k"},
		{"millions", 1234567, "tokens in  1.2M"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := formatOverallStats(overallStats{TokensIn: tc.tokensIn})
			found := false
			for _, l := range lines {
				if l == tc.wantInLine {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected line %q among %q", tc.wantInLine, lines)
			}
		})
	}
}

// TestOverallBandHeightInvariant pins the relationship between the formatter's
// line count, the reserved band height and the error-line index, so a formatter
// change cannot silently desync the band layout or the red-error highlighting.
func TestOverallBandHeightInvariant(t *testing.T) {
	lines := formatOverallStats(overallStats{})
	if len(lines) != overallMetricLines {
		t.Fatalf("formatOverallStats line count = %d, want overallMetricLines %d",
			len(lines), overallMetricLines)
	}
	if got := overallBandHeight - 2; got != overallMetricLines {
		t.Fatalf("overallBandHeight-2 = %d, want overallMetricLines %d", got, overallMetricLines)
	}
	if overallErrLineIdx >= len(lines) {
		t.Fatalf("overallErrLineIdx %d out of range (%d lines)", overallErrLineIdx, len(lines))
	}
	if !strings.HasPrefix(lines[overallErrLineIdx], "errors") {
		t.Fatalf("line %d = %q, want the errors row", overallErrLineIdx, lines[overallErrLineIdx])
	}
}

// TestSidebarRefreshOverallStats verifies the sidebar joins a report with its own
// session / sub-agent node counts, so addSession / applySubAgent move the panel's
// counts even when the report itself is unchanged.
func TestSidebarRefreshOverallStats(t *testing.T) {
	s := newTestSidebar()
	s.addSession("s1", "Session 1", false)
	s.addSession("s2", "Session 2", false)
	s.applySubAgent("s1", agent.SessionEvent{
		Type: agent.SessionEventSubAgent, AgentID: "a1", Name: "worker", Status: agent.StatusRunning,
	})

	report := stats.Report{Totals: stats.Totals{Primary: stats.ConnectorStat{
		Requests: 10, Errors: 1, TokensIn: 500, TokensOut: 50, CachedTokensIn: 100,
	}}}
	s.refreshOverallStats(report)

	if s.overall.Sessions != 2 {
		t.Errorf("Sessions = %d, want 2", s.overall.Sessions)
	}
	if s.overall.SubAgents != 1 {
		t.Errorf("SubAgents = %d, want 1", s.overall.SubAgents)
	}
	if s.overall.Requests != 10 || s.overall.Errors != 1 {
		t.Errorf("Requests/Errors = %d/%d, want 10/1", s.overall.Requests, s.overall.Errors)
	}
	if s.overall.CacheHitPct != 20 {
		t.Errorf("CacheHitPct = %d, want 20", s.overall.CacheHitPct)
	}
}

// TestSidebarOverallBandReservation covers the LayoutFn split: the bottom band is
// reserved (and the tree shrunk) when the sidebar is tall enough, and dropped so
// a short terminal keeps a usable session tree. SetBounds invokes LayoutFn.
func TestSidebarOverallBandReservation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		h         int
		wantBand  int
		wantTreeH int
	}{
		{"tall reserves band", 40, overallBandHeight, 40 - 1 - overallBandHeight},
		{"just enough for band + min tree", 14, overallBandHeight, 4},
		{"one short drops band", 13, 0, 12},
		{"very short drops band", 12, 0, 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSidebar()
			s.panel.SetBounds(tv.Rect{X: 0, Y: 0, W: sidebarWidth, H: tc.h})
			if s.overallBandH != tc.wantBand {
				t.Errorf("overallBandH = %d, want %d", s.overallBandH, tc.wantBand)
			}
			got := s.tree.Root().Bounds
			if got.H != tc.wantTreeH {
				t.Errorf("tree height = %d, want %d", got.H, tc.wantTreeH)
			}
			if got.W != sidebarWidth-3 || got.X != 2 || got.Y != 1 {
				t.Errorf("tree rect = %+v, want W=%d X=2 Y=1", got, sidebarWidth-3)
			}
		})
	}
}

// TestScheduleOverallRefreshSkipsWithoutHandler ensures the coalesced refresh is
// a no-op when the statistics handler is absent (e.g. the workbench used without
// a backend, or in tests), so it never arms a timer that would fire a Post onto
// a desktop that has nothing to show.
func TestScheduleOverallRefreshSkipsWithoutHandler(t *testing.T) {
	w := &Workbench{sidebar: newTestSidebar()} // no handlers.GetStatistics
	w.scheduleOverallRefresh()
	if w.statsRefresh != nil {
		t.Fatal("scheduled a refresh timer without a statistics handler")
	}
}
