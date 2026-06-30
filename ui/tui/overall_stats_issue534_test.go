package ui

import (
	"strings"
	"testing"

	"gogent/internal/config"
	"gogent/internal/stats"
)

// This file pins issue #534: in the "Overall" band, the aggregate "All models"
// view (selectedModel == "") must render the model and api rows EMPTY — blank,
// not the focused session's backend, not the "-" placeholder — while keeping the
// rows present (panel height / row count unchanged) and leaving the specific-model
// view rendering the model's display name + endpoint exactly as before.
//
// The build layer (buildOverallStats) leaves the two fields empty in aggregate by
// gating their population on a non-empty selectedModel; the format layer
// (formatOverallStats) renders an empty value as a blank column (the old
// overallValue "-" placeholder was removed because it had only these two call
// sites). The tests are grouped by the four design criteria (goal match,
// usability, no regressions, holistic scope) and exercise both halves of the fix,
// the edge cases the guard introduces, and the full refreshOverall integration path.

// groqIssue534 is a representative focused-session model config: a display name,
// an explicit endpoint (so formatEndpoint yields a host) and the OpenAI-compatible
// api type. Passing it alongside selectedModel == "" is exactly the pre-#534
// aggregate scenario that wrongly filled the rows from a single session's backend.
func groqIssue534() *config.ModelConfig {
	return &config.ModelConfig{
		Name:        "groq-free",
		DisplayName: "Groq",
		Connection:  "groq",
	}
}

// issue534Report is a report with non-zero grand totals and a per-model breakdown,
// so the aggregate-vs-scoped difference is observable and the numeric rows carry
// real values (used by the "numeric rows never blanked" regression guard).
func issue534Report() stats.Report {
	return stats.Report{
		Totals: stats.Totals{Primary: stats.ConnectorStat{
			Requests: 42, Errors: 3, TokensIn: 1000, TokensOut: 500, CachedTokensIn: 250,
		}},
		Models: []stats.ModelStat{{
			Name: "groq-free",
			Connector: stats.ConnectorStat{
				Requests: 42, Errors: 3, TokensIn: 1000, TokensOut: 500, CachedTokensIn: 250,
			},
		}},
	}
}

// overallRowValue extracts the value column of an Overall metric row: the text
// after the overallLabelWidth-wide padded label and its single separator space. It
// is the structural way to assert "the value is blank" without depending on the
// invisible trailing spaces a blank row ends in. For a blank value it returns "";
// for "model      Groq" it returns "Groq".
func overallRowValue(line string) string {
	return line[overallLabelWidth+1:]
}

// ---------------------------------------------------------------------------
// Criterion 1 — Goal match.
// ---------------------------------------------------------------------------

// TestIssue534_AggregateBlanksModelAndAPI is the core goal pin: with "All models"
// selected (selectedModel == ""), buildOverallStats leaves Model/APIEndpoint empty
// EVEN THOUGH a non-nil focused-session model config is passed — exactly the input
// refreshOverall feeds it. A cluster-wide grand total names no single backend.
func TestIssue534_AggregateBlanksModelAndAPI(t *testing.T) {
	got := buildOverallStats(issue534Report(), 3, 5, groqIssue534(), "")

	if got.Model != "" {
		t.Errorf("aggregate Model = %q, want %q (no backend for a cluster total, issue #534)", got.Model, "")
	}
	if got.APIEndpoint != "" {
		t.Errorf("aggregate APIEndpoint = %q, want %q (issue #534)", got.APIEndpoint, "")
	}
	// The aggregate numeric metrics are unaffected and still come from the grand total.
	if got.Requests != 42 || got.TokensIn != 1000 || got.Sessions != 3 {
		t.Errorf("aggregate numeric metrics regressed = %+v", got)
	}
}

// TestIssue534_AggregateRowsRenderBlankNotDashNorBackend pins the format layer: the
// aggregate-built struct renders the two rows with an EMPTY value column — not the
// old "-" placeholder, and not the focused session's backend name/host.
func TestIssue534_AggregateRowsRenderBlankNotDashNorBackend(t *testing.T) {
	lines := formatOverallStats(buildOverallStats(issue534Report(), 3, 5, groqIssue534(), ""))
	modelRow, apiRow := lines[len(lines)-2], lines[len(lines)-1]

	// Value column is empty.
	if v := overallRowValue(modelRow); v != "" {
		t.Errorf("aggregate model row value = %q, want blank", v)
	}
	if v := overallRowValue(apiRow); v != "" {
		t.Errorf("aggregate api row value = %q, want blank", v)
	}
	// Labels survive so the rows stay present and aligned.
	if !strings.HasPrefix(modelRow, "model") || !strings.HasPrefix(apiRow, "api") {
		t.Errorf("aggregate rows lost labels: model=%q api=%q", modelRow, apiRow)
	}
	// Not the focused backend, not the old placeholder.
	for _, row := range []string{modelRow, apiRow} {
		if strings.Contains(row, "Groq") || strings.Contains(row, "api.groq.com") {
			t.Errorf("aggregate row %q leaked the focused backend (issue #534)", row)
		}
		if overallRowValue(row) == "-" {
			t.Errorf("aggregate row %q rendered the stale \"-\" placeholder", row)
		}
	}
}

// TestIssue534_SpecificModelShowsNameAndEndpoint is the regression guard for the
// other half of the goal: selecting a specific model still fills the rows with that
// model's display name (falling back to the config Name) and endpoint, both at
// build time and in the rendered band.
func TestIssue534_SpecificModelShowsNameAndEndpoint(t *testing.T) {
	cfg := groqIssue534()
	got := buildOverallStats(issue534Report(), 3, 5, cfg, cfg.Name)

	if got.Model != "Groq" {
		t.Errorf("specific Model = %q, want display name %q", got.Model, "Groq")
	}
	if got.APIEndpoint != "groq" {
		t.Errorf("specific APIEndpoint = %q, want connection %q", got.APIEndpoint, "groq")
	}

	lines := formatOverallStats(got)
	if overallRowValue(lines[len(lines)-2]) != "Groq" {
		t.Errorf("specific model row = %q, want value %q", lines[len(lines)-2], "Groq")
	}
	if overallRowValue(lines[len(lines)-1]) != "groq" {
		t.Errorf("specific api row = %q, want value %q", lines[len(lines)-1], "groq")
	}
}

// TestIssue534_FocusedAggregateBlanksBothHalves pins both halves of the fix
// together (the focused test the design requires): the aggregate build yields empty
// Model/APIEndpoint with a non-nil config, the formatter emits the two rows blank
// while still returning exactly overallMetricLines lines, and the same config scoped
// to itself fills them. This catches a build/format divergence a single-half test
// would miss.
func TestIssue534_FocusedAggregateBlanksBothHalves(t *testing.T) {
	cfg := groqIssue534()

	agg := buildOverallStats(issue534Report(), 4, 6, cfg, "")
	aggLines := formatOverallStats(agg)

	if agg.Model != "" || agg.APIEndpoint != "" {
		t.Fatalf("aggregate build = %+v, want empty Model/APIEndpoint", agg)
	}
	if len(aggLines) != overallMetricLines {
		t.Fatalf("aggregate format produced %d lines, want overallMetricLines %d", len(aggLines), overallMetricLines)
	}
	modelRow, apiRow := aggLines[overallMetricLines-2], aggLines[overallMetricLines-1]
	if !strings.HasPrefix(modelRow, "model") || overallRowValue(modelRow) != "" {
		t.Errorf("aggregate model row = %q, want label + blank value", modelRow)
	}
	if !strings.HasPrefix(apiRow, "api") || overallRowValue(apiRow) != "" {
		t.Errorf("aggregate api row = %q, want label + blank value", apiRow)
	}

	// Sanity: the same config scoped to itself fills the rows.
	scoped := formatOverallStats(buildOverallStats(issue534Report(), 4, 6, cfg, cfg.Name))
	if overallRowValue(scoped[overallMetricLines-2]) != "Groq" ||
		overallRowValue(scoped[overallMetricLines-1]) != "groq" {
		t.Errorf("scoped rows = %q / %q, want Groq / groq",
			scoped[overallMetricLines-2], scoped[overallMetricLines-1])
	}
}

// ---------------------------------------------------------------------------
// Criterion 2 — Usability: height / row count / alignment unchanged.
// ---------------------------------------------------------------------------

// TestIssue534_RowCountStableAcrossSelections asserts the panel always emits
// exactly overallMetricLines rows and that the last two are always the model/api
// rows — whether the view is aggregate (blank), specific (filled) or empty. No row
// may be added, removed or reordered by the change.
func TestIssue534_RowCountStableAcrossSelections(t *testing.T) {
	cfg := groqIssue534()
	rep := issue534Report()
	cases := []struct {
		name string
		s    overallStats
	}{
		{"aggregate (blank backend)", buildOverallStats(rep, 3, 5, cfg, "")},
		{"specific model (filled)", buildOverallStats(rep, 3, 5, cfg, cfg.Name)},
		{"empty / first-frame", overallStats{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := formatOverallStats(tc.s)
			if len(lines) != overallMetricLines {
				t.Fatalf("produced %d lines, want %d", len(lines), overallMetricLines)
			}
			if !strings.HasPrefix(lines[len(lines)-2], "model") {
				t.Errorf("penultimate row = %q, want the model row", lines[len(lines)-2])
			}
			if !strings.HasPrefix(lines[len(lines)-1], "api") {
				t.Errorf("last row = %q, want the api row", lines[len(lines)-1])
			}
		})
	}
}

// TestIssue534_ValueColumnStaysAligned pins that blanking the value does not shift
// the value column: the padded-label + separator prefix is byte-identical whether
// the value is empty (aggregate) or filled (specific), so the value column starts
// at the same offset in both views and only the value text differs.
func TestIssue534_ValueColumnStaysAligned(t *testing.T) {
	cfg := groqIssue534()
	rep := issue534Report()

	blankLines := formatOverallStats(buildOverallStats(rep, 3, 5, cfg, ""))
	filledLines := formatOverallStats(buildOverallStats(rep, 3, 5, cfg, cfg.Name))
	prefixLen := overallLabelWidth + 1 // padded label + single separator space

	for _, idx := range []int{len(blankLines) - 2, len(blankLines) - 1} {
		blank, filled := blankLines[idx], filledLines[idx]
		if blank[:prefixLen] != filled[:prefixLen] {
			t.Errorf("row %d value-column prefix drifted between aggregate and specific: blank=%q filled=%q", idx, blank, filled)
		}
		if blank[prefixLen:] != "" {
			t.Errorf("row %d aggregate value = %q, want empty", idx, blank[prefixLen:])
		}
		if filled[prefixLen:] == "" {
			t.Errorf("row %d specific value unexpectedly empty", idx)
		}
	}
}

// TestIssue534_BandHeightInvariantHolds re-asserts the band-height invariant for
// the aggregate case: formatOverallStats returns overallMetricLines rows and the
// reserved band height still decomposes as separator + selector + metrics + title.
func TestIssue534_BandHeightInvariantHolds(t *testing.T) {
	aggLines := formatOverallStats(buildOverallStats(issue534Report(), 3, 5, groqIssue534(), ""))
	if len(aggLines) != overallMetricLines {
		t.Fatalf("aggregate format = %d lines, want %d", len(aggLines), overallMetricLines)
	}
	want := overallSeparatorLines + overallSelectorLines + overallMetricLines + 1
	if overallBandHeight != want {
		t.Fatalf("overallBandHeight = %d, want %d (separator+selector+metric+title)", overallBandHeight, want)
	}
}

// ---------------------------------------------------------------------------
// Criterion 3 — No regressions.
// ---------------------------------------------------------------------------

// TestIssue534_NumericRowsNeverBlankedInAggregate is the key regression guard for
// removing the overallValue placeholder: the numeric metric rows must keep rendering
// their real values in the aggregate view. overallValue had only the model/api call
// sites, so a numeric row cannot be blanked — but this makes that structural
// guarantee explicit rather than relying on the call-graph.
func TestIssue534_NumericRowsNeverBlankedInAggregate(t *testing.T) {
	lines := formatOverallStats(buildOverallStats(issue534Report(), 7, 9, groqIssue534(), ""))

	// The first overallMetricLines-2 rows are numeric; none may be blank or carry "-".
	for i := 0; i < overallMetricLines-2; i++ {
		v := overallRowValue(lines[i])
		if v == "" {
			t.Errorf("numeric row %d (%q) is blank in aggregate — must render its value", i, lines[i])
		}
		if v == "-" {
			t.Errorf("numeric row %d (%q) rendered \"-\" — placeholder leaked past model/api", i, lines[i])
		}
	}
	// Spot-check the figures so the guard is meaningful, not just non-empty.
	if overallRowValue(lines[0]) != "7" { // sessions
		t.Errorf("sessions row = %q, want value 7", lines[0])
	}
	if overallRowValue(lines[1]) != "9" { // sub-agents
		t.Errorf("sub-agents row = %q, want value 9", lines[1])
	}
	if overallRowValue(lines[4]) != "42" { // requests
		t.Errorf("requests row = %q, want value 42", lines[4])
	}
}

// TestIssue534_ErrorHighlightIndexUnchanged pins that blanking the backend rows
// does not shift overallErrLineIdx: the errors row still sits at its pinned index
// in the aggregate view and remains the row the band highlights red.
func TestIssue534_ErrorHighlightIndexUnchanged(t *testing.T) {
	lines := formatOverallStats(buildOverallStats(issue534Report(), 3, 5, groqIssue534(), ""))
	if overallErrLineIdx >= len(lines) {
		t.Fatalf("overallErrLineIdx %d out of range (%d lines)", overallErrLineIdx, len(lines))
	}
	if !strings.HasPrefix(lines[overallErrLineIdx], "errors") {
		t.Fatalf("line %d = %q, want the errors row", overallErrLineIdx, lines[overallErrLineIdx])
	}
	// The errors row is distinct from the (now blank) model/api rows.
	if overallErrLineIdx == overallMetricLines-1 || overallErrLineIdx == overallMetricLines-2 {
		t.Fatalf("overallErrLineIdx %d must not collide with the model/api rows", overallErrLineIdx)
	}
}

// TestIssue534_SpecificRenderIsByteIdenticalToPreFix guards the "specific model
// unchanged" requirement with an exact full-output comparison. The #534 layout
// (specific-model rows populated, aggregate-only rows blank) is unchanged; #546
// expanded the single "cache hit" row into the two-row cache read/write breakdown,
// so the panel now emits ten lines (this input records no cache write, so the write
// row degrades to "cache wr   0% 0").
func TestIssue534_SpecificRenderIsByteIdenticalToPreFix(t *testing.T) {
	got := formatOverallStats(overallStats{
		Sessions: 3, SubAgents: 5, TokensIn: 1234, TokensOut: 567,
		Requests: 42, Errors: 0, CacheHitPct: 25,
		Model: "Groq", APIEndpoint: "api.groq.com",
	})
	want := []string{
		"sessions   3",
		"sub-agents 5",
		"tokens in  1.2k",
		"tokens out 567",
		"requests   42",
		"errors     0",
		"cache rd   25% 0",
		"cache wr   0% 0",
		"model      Groq",
		"api        api.groq.com",
	}
	if len(got) != len(want) {
		t.Fatalf("produced %d lines, want %d (%q)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("line %d = %q, want %q", i, got[i], w)
		}
	}
}

// TestIssue534_NoStaleDashPlaceholderAnywhere confirms the "-" placeholder is fully
// gone from the rendered band in every state (the value column, not the label), so
// removing overallValue left no stray dash — including the first-frame "no model
// yet" path, now blank rather than "-", consistent with the aggregate view.
func TestIssue534_NoStaleDashPlaceholderAnywhere(t *testing.T) {
	cfg := groqIssue534()
	states := []overallStats{
		{}, // first-frame, no session
		buildOverallStats(issue534Report(), 3, 5, cfg, ""),          // aggregate
		buildOverallStats(issue534Report(), 3, 5, cfg, "groq-free"), // specific
	}
	for i, s := range states {
		for j, line := range formatOverallStats(s) {
			if overallRowValue(line) == "-" {
				t.Errorf("state %d line %d = %q has the stale \"-\" placeholder value", i, j, line)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Edge cases around the buildOverallStats guard (selectedModel + model nil-ness).
// ---------------------------------------------------------------------------

// TestIssue534_SelectedModelWithNilConfig_BlanksBackend exercises the && model !=
// nil half of the guard: a selection set but no resolvable config cannot name a
// backend, so the rows stay blank (never crash, never fall back to garbage).
func TestIssue534_SelectedModelWithNilConfig_BlanksBackend(t *testing.T) {
	got := buildOverallStats(issue534Report(), 3, 5, nil, "groq-free")
	if got.Model != "" || got.APIEndpoint != "" {
		t.Errorf("selected-but-nil-config = Model %q API %q, want both empty", got.Model, got.APIEndpoint)
	}
}

// TestIssue534_AggregateNilConfigStillEmpty confirms the pre-fix aggregate path
// (nil model + aggregate) stays empty and still renders the full row count.
func TestIssue534_AggregateNilConfigStillEmpty(t *testing.T) {
	got := buildOverallStats(stats.Report{}, 0, 0, nil, "")
	if got != (overallStats{}) {
		t.Fatalf("nil-model aggregate = %+v, want the zero view", got)
	}
	if lines := formatOverallStats(got); len(lines) != overallMetricLines {
		t.Errorf("nil-model aggregate rendered %d lines, want %d", len(lines), overallMetricLines)
	}
}

// TestIssue534_FirstFrameBlankNotDash pins the accepted consequence that the first
// frame (no session, no selection) now shows blank model/api rows instead of the
// old "-", matching the aggregate view so the two empty states agree.
func TestIssue534_FirstFrameBlankNotDash(t *testing.T) {
	lines := formatOverallStats(overallStats{})
	if v := overallRowValue(lines[len(lines)-2]); v != "" {
		t.Errorf("first-frame model value = %q, want blank (was %q before #534)", v, "-")
	}
	if v := overallRowValue(lines[len(lines)-1]); v != "" {
		t.Errorf("first-frame api value = %q, want blank", v)
	}
}

// TestIssue534_SpecificConfigEmptyName_BlanksGracefully covers a config with neither
// a display name nor a config name: the populate block runs (selectedModel != "" &&
// model != nil) but yields an empty Model, so the row renders blank without error
// while the endpoint still resolves from the config.
func TestIssue534_SpecificConfigEmptyName_BlanksGracefully(t *testing.T) {
	cfg := &config.ModelConfig{Name: "", DisplayName: "", Connection: "groq"}
	got := buildOverallStats(stats.Report{}, 0, 0, cfg, "some-selection")
	if got.Model != "" {
		t.Errorf("empty-name config Model = %q, want %q", got.Model, "")
	}
	if got.APIEndpoint != "groq" {
		t.Errorf("empty-name config APIEndpoint = %q, want groq (connection name)", got.APIEndpoint)
	}
}

// TestIssue534_SpecificAPIRowShowsConnection covers the api row in the specific
// view: it now shows the model's connection name (credentials/endpoint moved to the
// connection in the discovery redesign).
func TestIssue534_SpecificAPIRowShowsConnection(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.ModelConfig
		want string
	}{
		{"connection name shown", &config.ModelConfig{Name: "zai", DisplayName: "ZAI", Connection: "zai"}, "zai"},
		{"blank when no connection", &config.ModelConfig{Name: "oai", DisplayName: "OAI"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildOverallStats(stats.Report{}, 0, 0, tc.cfg, tc.cfg.Name)
			if got.APIEndpoint != tc.want {
				t.Errorf("APIEndpoint = %q, want %q", got.APIEndpoint, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Integration through the real refreshOverall path (tui.go -> sidebar).
// ---------------------------------------------------------------------------

// TestIssue534_WorkbenchRefreshOverall_AggregateBlanksAndSpecificFills drives the
// full refreshOverall path the user hits (model selector -> refresh): the default
// aggregate selection blanks the model/api rows, and selecting a known model fills
// them from that model's config via modelByName.
func TestIssue534_WorkbenchRefreshOverall_AggregateBlanksAndSpecificFills(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetModels([]*config.ModelConfig{{
		Name: "groq-free", DisplayName: "Groq",
		Connection: "groq",
	}})
	w.handlers.GetStatistics = func() stats.Report { return issue534Report() }

	// Default selection is the aggregate ("All models") -> blank backend.
	w.refreshOverall()
	if w.sidebar.overall.Model != "" || w.sidebar.overall.APIEndpoint != "" {
		t.Errorf("aggregate refresh Model/API = %q/%q, want blank (issue #534)",
			w.sidebar.overall.Model, w.sidebar.overall.APIEndpoint)
	}

	// Selecting the known model fills the rows from its config.
	w.sidebar.setSelectedOverallModel("groq-free")
	w.refreshOverall()
	if w.sidebar.overall.Model != "Groq" || w.sidebar.overall.APIEndpoint != "groq" {
		t.Errorf("specific refresh Model/API = %q/%q, want Groq/groq",
			w.sidebar.overall.Model, w.sidebar.overall.APIEndpoint)
	}
}
