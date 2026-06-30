package ui

import (
	"strings"
	"testing"

	"gogent/internal/stats"
)

// This file guards the issue #546 display surface: buildOverallStats now carries the
// cache write share + read/write token magnitudes (aggregate and model-scoped), formatOverallStats
// renders the two-row "cache rd"/"cache wr" breakdown, and writeConnector surfaces the per-backend
// write figures. The report-level method (CacheWritePercent) is pinned in internal/stats; these
// tests pin the UI wiring on top of it. Criterion 3 (no regressions) is covered by the band/row
// invariants asserted at the end; criterion 2 (usability/width) by the fit-at-the-floor test.

// ----------------------------------------------------------------------------
// buildOverallStats: the cache write breakdown is populated from the primary connector.
// ----------------------------------------------------------------------------

// TestBuildOverallStatsCacheWriteAggregate pins the aggregate ("All models") path: the three
// new fields come from report.Totals.Primary, exactly as CacheHitPct already did.
func TestBuildOverallStatsCacheWriteAggregate(t *testing.T) {
	report := stats.Report{Totals: stats.Totals{
		// 700 reads / 200 writes of 1000 input → 70% read, 20% write.
		Primary: stats.ConnectorStat{TokensIn: 1000, CachedTokensIn: 700, CacheWriteTokensIn: 200},
	}}
	got := buildOverallStats(report, 0, 0, nil, "", nil)
	if got.CacheWritePct != 20 {
		t.Errorf("CacheWritePct = %d, want 20 (200/1000)", got.CacheWritePct)
	}
	if got.CacheReadTokens != 700 {
		t.Errorf("CacheReadTokens = %d, want 700 (prim.CachedTokensIn)", got.CacheReadTokens)
	}
	if got.CacheWriteTokens != 200 {
		t.Errorf("CacheWriteTokens = %d, want 200 (prim.CacheWriteTokensIn)", got.CacheWriteTokens)
	}
	if got.CacheHitPct != 70 {
		t.Errorf("CacheHitPct = %d, want 70 (read share unchanged)", got.CacheHitPct)
	}
}

// TestBuildOverallStatsCacheWriteFastDoesNotBleed is the double-counting guard (criterion 3):
// the auxiliary/fast backend carries its own (large) cache figures, but the Overall panel's
// cache breakdown must draw ONLY from the primary backend — the fast backend is reported
// separately in the Statistics dialog to avoid double counting. If the write field ever bled
// from Fast, the panel would over-report write spend.
func TestBuildOverallStatsCacheWriteFastDoesNotBleed(t *testing.T) {
	report := stats.Report{Totals: stats.Totals{
		Primary: stats.ConnectorStat{TokensIn: 1000, CachedTokensIn: 700, CacheWriteTokensIn: 200},
		Fast:    stats.ConnectorStat{TokensIn: 9999, CachedTokensIn: 9999, CacheWriteTokensIn: 9999},
	}}
	got := buildOverallStats(report, 0, 0, nil, "", nil)
	if got.CacheWriteTokens != 200 {
		t.Errorf("CacheWriteTokens = %d, want 200 — Fast backend (%d) must not bleed into the panel",
			got.CacheWriteTokens, 9999)
	}
	if got.CacheReadTokens != 700 {
		t.Errorf("CacheReadTokens = %d, want 700 — Fast backend must not bleed", got.CacheReadTokens)
	}
}

// TestBuildOverallStatsCacheWriteEmpty is the empty-report / first-frame path: a zero report
// yields zero cache fields (no panic, no divide-by-zero), so the panel renders "cache rd 0% 0"
// before any traffic.
func TestBuildOverallStatsCacheWriteEmpty(t *testing.T) {
	got := buildOverallStats(stats.Report{}, 0, 0, nil, "", nil)
	if got.CacheWritePct != 0 || got.CacheReadTokens != 0 || got.CacheWriteTokens != 0 {
		t.Errorf("empty report cache fields = pct=%d read=%d write=%d, want all 0",
			got.CacheWritePct, got.CacheReadTokens, got.CacheWriteTokens)
	}
}

// TestBuildOverallStatsCacheWriteModelScoped pins the model-scoped path (issue #191 selector):
// when a model is selected, the cache breakdown is sourced from that model's per-model connector
// (ms.Connector), NOT the cluster aggregate. This is the per-model reachability for the write
// figure.
func TestBuildOverallStatsCacheWriteModelScoped(t *testing.T) {
	report := stats.Report{
		Totals: stats.Totals{Primary: stats.ConnectorStat{TokensIn: 1000, CachedTokensIn: 700, CacheWriteTokensIn: 200}},
		Models: []stats.ModelStat{{
			Name:      "anthropic-claude",
			Connector: stats.ConnectorStat{TokensIn: 500, CachedTokensIn: 300, CacheWriteTokensIn: 50},
		}},
	}
	got := buildOverallStats(report, 0, 0, nil, "anthropic-claude", nil)
	// 50 writes / 500 input = 10%; sourced from the model, not the 20% cluster aggregate.
	if got.CacheWritePct != 10 {
		t.Errorf("model-scoped CacheWritePct = %d, want 10 (50/500 from ms.Connector, not 20%% cluster)",
			got.CacheWritePct)
	}
	if got.CacheWriteTokens != 50 {
		t.Errorf("model-scoped CacheWriteTokens = %d, want 50 (ms.Connector.CacheWriteTokensIn)", got.CacheWriteTokens)
	}
	if got.CacheReadTokens != 300 {
		t.Errorf("model-scoped CacheReadTokens = %d, want 300 (ms.Connector.CachedTokensIn)", got.CacheReadTokens)
	}
}

// TestBuildOverallStatsCacheWriteModelNotFound pins the absent-model path: selecting a model
// with no recorded usage zeroes every cache field — it does NOT fall back to the cluster
// aggregate. Without this, a typo'd or stale model selection would silently show cluster-wide
// write spend attributed to a model that did nothing.
func TestBuildOverallStatsCacheWriteModelNotFound(t *testing.T) {
	report := stats.Report{Totals: stats.Totals{
		Primary: stats.ConnectorStat{TokensIn: 1000, CachedTokensIn: 700, CacheWriteTokensIn: 200},
	}}
	got := buildOverallStats(report, 0, 0, nil, "never-used-model", nil)
	if got.CacheWritePct != 0 || got.CacheReadTokens != 0 || got.CacheWriteTokens != 0 {
		t.Errorf("absent-model cache fields = pct=%d read=%d write=%d, want all 0 (no aggregate leak)",
			got.CacheWritePct, got.CacheReadTokens, got.CacheWriteTokens)
	}
}

// ----------------------------------------------------------------------------
// formatOverallStats: the two-row read/write breakdown.
// ----------------------------------------------------------------------------

// TestFormatOverallStatsCacheReadWrite pins the populated breakdown exactly: the read row shows
// the read share and the read-token magnitude, the write row the write share and write magnitude.
// This mirrors the issue's illustrative "78% read (12.4k) · 9% write" on two narrow rows.
func TestFormatOverallStatsCacheReadWrite(t *testing.T) {
	lines := formatOverallStats(overallStats{
		CacheHitPct: 78, CacheReadTokens: 12400,
		CacheWritePct: 9, CacheWriteTokens: 1100,
	})
	want := map[int]string{
		6: "cache rd   78% 12.4k",
		7: "cache wr   9% 1.1k",
	}
	for i, w := range want {
		if i >= len(lines) {
			t.Fatalf("only %d lines rendered, need index %d", len(lines), i)
		}
		if lines[i] != w {
			t.Errorf("line %d = %q, want %q", i, lines[i], w)
		}
	}
}

// TestFormatOverallStatsCacheRowsDegradeToZero is the criterion-3 degrade gate: for a provider
// that never writes (and the empty first frame), both cache rows render cleanly with 0% and a
// zero magnitude — never blank, never a stray "NaN"/"%!".
func TestFormatOverallStatsCacheRowsDegradeToZero(t *testing.T) {
	lines := formatOverallStats(overallStats{})
	if lines[6] != "cache rd   0% 0" {
		t.Errorf("read row = %q, want %q (degrade)", lines[6], "cache rd   0% 0")
	}
	if lines[7] != "cache wr   0% 0" {
		t.Errorf("write row = %q, want %q (degrade)", lines[7], "cache wr   0% 0")
	}
	// Guard against a format-string typo leaking a "%!" or "(MISSING)" into the rendered row.
	for i, l := range lines {
		if strings.Contains(l, "%!") || strings.Contains(l, "(MISSING)") {
			t.Errorf("line %d = %q has a Printf formatting error", i, l)
		}
	}
}

// ----------------------------------------------------------------------------
// Criterion 3 (no regressions): row count, band height and error-row index stay in sync.
// ----------------------------------------------------------------------------

// TestOverallCacheRowsPreserveBandInvariants pins that adding the second cache row kept the
// band layout honest: the emitted row count equals overallMetricLines, the derived band height
// matches, the "errors" row is still at overallErrLineIdx, and both cache rows sit strictly
// below it (so the red error highlight never lands on a cache row).
func TestOverallCacheRowsPreserveBandInvariants(t *testing.T) {
	lines := formatOverallStats(overallStats{})
	if len(lines) != overallMetricLines {
		t.Fatalf("row count = %d, want overallMetricLines %d", len(lines), overallMetricLines)
	}
	if got := overallSeparatorLines + overallSelectorLines + overallMetricLines + 1; got != overallBandHeight {
		t.Errorf("derived band height = %d, want overallBandHeight %d", got, overallBandHeight)
	}
	if !strings.HasPrefix(lines[overallErrLineIdx], "errors") {
		t.Fatalf("line %d = %q, want the errors row", overallErrLineIdx, lines[overallErrLineIdx])
	}
	// The two cache rows are indices 6 and 7, both strictly after the errors row (index 5).
	for _, ci := range []int{6, 7} {
		if ci <= overallErrLineIdx {
			t.Errorf("cache row index %d must be after the errors row (%d)", ci, overallErrLineIdx)
		}
		if !strings.HasPrefix(lines[ci], "cache ") {
			t.Errorf("line %d = %q, want a cache row", ci, lines[ci])
		}
	}
}

// ----------------------------------------------------------------------------
// Criterion 2 (usability): the cache rows fit the 24-col minimum sidebar without clipping.
// ----------------------------------------------------------------------------

// TestOverallCacheRowsFitAtMinSidebarWidth guards the width budget that forced the two-row /
// "rd"/"wr" design: at the draggable sidebar floor (minSidebarWidth=24) the clip ceiling is
// contentW = 24-3 = 21, and each cache row — padded label (overallLabelWidth=10) + space + value
// — must stay within it for realistic magnitudes so the user sees the full percentage and token
// count (the write premium is the point of the issue; clipping its value would hide the spend).
func TestOverallCacheRowsFitAtMinSidebarWidth(t *testing.T) {
	const minContentW = minSidebarWidth - 3 // 21 — drawOverall's clip ceiling at the floor
	for _, tc := range []struct {
		name string
		s    overallStats
	}{
		{"degrade zeros", overallStats{}},
		{"typical anthropic", overallStats{CacheHitPct: 78, CacheReadTokens: 12400, CacheWritePct: 9, CacheWriteTokens: 1100}},
		{"design worst-case value 100% + 9.9M", overallStats{CacheHitPct: 100, CacheReadTokens: 9_900_000}},
	} {
		lines := formatOverallStats(tc.s)
		for _, idx := range []int{6, 7} {
			row := []rune(lines[idx])
			if len(row) > minContentW {
				t.Errorf("%s: cache row %d = %q is %d cols, exceeds the %d-col floor clip ceiling (would truncate)",
					tc.name, idx, lines[idx], len(row), minContentW)
			}
		}
	}
}

// TestOverallCacheRowExtremeMagnitudeClipsGracefully documents the one width edge the
// "value <=9 cols" comment understates: formatTokens has no billions tier, so a ≥10M read/write
// magnitude renders as e.g. "500.0M" (6 cols) and "100% 500.0M" (11) pushes the row past the
// 21-col floor. This is the same scaling the existing "tokens in"/"tokens out" rows already have,
// and drawOverall clips every row via truncateRunes — so the degradation is a clean truncation,
// never a panic or a Printf error. This pins that graceful behaviour rather than hiding the edge.
func TestOverallCacheRowExtremeMagnitudeClipsGracefully(t *testing.T) {
	const minContentW = minSidebarWidth - 3 // 21
	lines := formatOverallStats(overallStats{CacheHitPct: 100, CacheReadTokens: 500_000_000})
	rd := lines[6]
	// The raw formatter row exceeds the floor's clip ceiling at this extreme magnitude.
	if len([]rune(rd)) <= minContentW {
		t.Fatalf("precondition: expected the 500M read row to exceed %d cols, got %d (%q)",
			minContentW, len([]rune(rd)), rd)
	}
	// drawOverall's actual render path truncates to the clip ceiling — graceful, not corrupting.
	clipped := truncateRunes(rd, minContentW)
	if got := len([]rune(clipped)); got > minContentW {
		t.Errorf("truncated read row = %d cols, want <= %d (drawOverall must clip)", got, minContentW)
	}
	if !strings.HasPrefix(clipped, "cache rd") {
		t.Errorf("clipped read row = %q, want to keep the 'cache rd' label", clipped)
	}
	if strings.Contains(clipped, "%!") || strings.Contains(clipped, "(MISSING)") {
		t.Errorf("clipped read row = %q has a Printf formatting error", clipped)
	}
}

// ----------------------------------------------------------------------------
// writeConnector (Statistics dialog Overview): the per-backend cache write figures.
// ----------------------------------------------------------------------------

// TestWriteConnectorCacheWriteRow pins that the Statistics dialog's per-backend block renders
// the cache write token count and its share next to the existing read figures. The dialog is
// the cluster-aggregate surface for the write breakdown (writeConnector is called only for
// Totals.Primary/Totals.Fast, never per session).
func TestWriteConnectorCacheWriteRow(t *testing.T) {
	rep := stats.Report{Totals: stats.Totals{
		// 700 reads / 200 writes of 1000 input.
		Primary: stats.ConnectorStat{Requests: 10, Success: 10, TokensIn: 1000, TokensOut: 500,
			CachedTokensIn: 700, CacheWriteTokensIn: 200},
	}}
	got := renderStatsOverview(rep)
	// Read figures (unchanged) still present.
	if !strings.Contains(got, "Cache hit: 70%") {
		t.Errorf("overview missing read 'Cache hit: 70%%'\n--- overview ---\n%s", got)
	}
	// New write figures.
	if !strings.Contains(got, "Cache wr:") {
		t.Errorf("overview missing 'Cache wr:' row\n--- overview ---\n%s", got)
	}
	if !strings.Contains(got, "Cache wr %: 20%") {
		t.Errorf("overview missing write share 'Cache wr %%: 20%%' (200/1000)\n--- overview ---\n%s", got)
	}
	// formatTokens(200) = "200".
	if !strings.Contains(got, "Cache wr:") || !strings.Contains(got, "200") {
		t.Errorf("overview write row should carry the 200 write-token magnitude\n--- overview ---\n%s", got)
	}
}

// TestWriteConnectorCacheWriteRowDegradesToZero is the degrade gate for the dialog: a provider
// with no cache writes renders "Cache wr %: 0%" cleanly, matching the panel's degrade behaviour.
func TestWriteConnectorCacheWriteRowDegradesToZero(t *testing.T) {
	rep := stats.Report{Totals: stats.Totals{
		Primary: stats.ConnectorStat{Requests: 10, TokensIn: 1000, TokensOut: 500, CachedTokensIn: 700},
	}}
	got := renderStatsOverview(rep)
	if !strings.Contains(got, "Cache wr %: 0%") {
		t.Errorf("non-write provider overview should degrade to 'Cache wr %%: 0%%'\n--- overview ---\n%s", got)
	}
	if strings.Contains(got, "%!") || strings.Contains(got, "(MISSING)") {
		t.Errorf("write row has a Printf formatting error\n--- overview ---\n%s", got)
	}
}

// ----------------------------------------------------------------------------
// Per-session reachability (goal #2): the write count flows into the per-session CSV rows.
// ----------------------------------------------------------------------------

// TestReportCSVCacheWritePerSession pins that cache_write_tokens_in is emitted on the per-session
// rows (writeConnectorCSV is called per "session:<id>"), so a user can reach a per-session write
// figure via the CSV export even though the dialog renders it aggregated. This is the issue's
// stated minimum for goal #2 ("per-session numbers reachable").
func TestReportCSVCacheWritePerSession(t *testing.T) {
	rep := stats.Report{Sessions: []stats.SessionRow{{
		ID:      "s1",
		Primary: stats.ConnectorStat{Requests: 5, TokensIn: 1000, CachedTokensIn: 600, CacheWriteTokensIn: 42},
	}}}
	out, err := rep.CSV()
	if err != nil {
		t.Fatalf("CSV: %v", err)
	}
	if !strings.Contains(out, "session:s1:primary,,cache_write_tokens_in,42") {
		t.Errorf("per-session CSV missing cache_write_tokens_in,42 (goal #2 reachability)\n--- CSV ---\n%s", out)
	}
}
