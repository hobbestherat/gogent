package stats

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"gogent/internal/model"
)

// TestFromSnapshot verifies every model.StatsSnapshot field maps onto the
// corresponding ConnectorStat field (the neutral type the UI/export code uses).
func TestFromSnapshot(t *testing.T) {
	in := model.StatsSnapshot{
		RequestCount:               7,
		SuccessCount:               5,
		ErrorCount:                 2,
		TotalTokensIn:              1000,
		TotalCachedTokensIn:        400,
		TotalTokensOut:             300,
		TotalTimeMs:                14000,
		TimeoutCount:               1,
		ContextWindowOverflowCount: 3,
		RefusalCount:               4,
		GenericErrorCount:          6,
	}
	got := FromSnapshot(in)
	want := ConnectorStat{
		Requests: 7, Success: 5, Errors: 2,
		TokensIn: 1000, CachedTokensIn: 400, TokensOut: 300, TotalTimeMs: 14000,
		Timeouts: 1, ContextOverflows: 3, Refusals: 4, GenericErrors: 6,
	}
	if got != want {
		t.Errorf("FromSnapshot = %+v, want %+v", got, want)
	}
}

// TestConnectorStatAddAndAvg covers element-wise aggregation and the
// per-request average latency (and its zero-guard when there are no requests).
func TestConnectorStatAddAndAvg(t *testing.T) {
	a := ConnectorStat{Requests: 2, Success: 2, TotalTimeMs: 1000, TokensIn: 10}
	b := ConnectorStat{Requests: 3, Errors: 1, TotalTimeMs: 500, TokensIn: 20}
	sum := a.Add(b)
	if sum.Requests != 5 || sum.Success != 2 || sum.Errors != 1 || sum.TokensIn != 30 || sum.TotalTimeMs != 1500 {
		t.Errorf("Add = %+v, want aggregated totals", sum)
	}
	if got, want := sum.AvgLatencyMs(), int64(300); got != want { // 1500/5
		t.Errorf("AvgLatencyMs = %d, want %d", got, want)
	}
	if got := (ConnectorStat{}).AvgLatencyMs(); got != 0 {
		t.Errorf("AvgLatencyMs with no requests = %d, want 0", got)
	}
}

// TestConnectorStatCacheHitPercent covers the prompt-cache hit-rate helper,
// including its zero-guard and that cached tokens aggregate under Add.
func TestConnectorStatCacheHitPercent(t *testing.T) {
	c := ConnectorStat{TokensIn: 1000, CachedTokensIn: 800}
	if got, want := c.CacheHitPercent(), 80; got != want {
		t.Errorf("CacheHitPercent = %d, want %d", got, want)
	}
	if got := (ConnectorStat{}).CacheHitPercent(); got != 0 {
		t.Errorf("CacheHitPercent with no tokens = %d, want 0", got)
	}
	sum := ConnectorStat{TokensIn: 100, CachedTokensIn: 40}.
		Add(ConnectorStat{TokensIn: 100, CachedTokensIn: 60})
	if sum.CachedTokensIn != 100 {
		t.Errorf("Add CachedTokensIn = %d, want 100", sum.CachedTokensIn)
	}
	if got, want := sum.CacheHitPercent(), 50; got != want {
		t.Errorf("aggregated CacheHitPercent = %d, want %d", got, want)
	}
}

// TestToolStatAvgMs covers the average-duration helper and its zero-guard.
func TestToolStatAvgMs(t *testing.T) {
	if got := (ToolStat{}).AvgMs(); got != 0 {
		t.Errorf("AvgMs with no invocations = %d, want 0", got)
	}
	ts := ToolStat{Invocations: 4, TotalMs: 1000}
	if got, want := ts.AvgMs(), int64(250); got != want {
		t.Errorf("AvgMs = %d, want %d", got, want)
	}
}

// sampleReport is a small, representative report reused across the format tests.
func sampleReport() Report {
	return Report{
		GeneratedAt: 1_700_000_000,
		Totals: Totals{
			Sessions: 2, Turns: 5, TokensIn: 1000, TokensOut: 200,
			ToolCalls: 9, Compactions: 1,
			Primary: ConnectorStat{Requests: 10, Success: 9, Errors: 1, TokensIn: 1000, TokensOut: 200, TotalTimeMs: 8000},
			Fast:    ConnectorStat{Requests: 1, TokensIn: 50, TokensOut: 10},
		},
		Sessions: []SessionRow{
			{ID: "session-1", Turns: 3, TokensIn: 600, TokensOut: 120, ToolCalls: 6, Compactions: 1,
				Primary: ConnectorStat{Requests: 6}},
			{ID: "session-2", Turns: 2, TokensIn: 400, TokensOut: 80, ToolCalls: 3,
				Primary: ConnectorStat{Requests: 4, Errors: 1}},
		},
		Tools: []ToolStat{
			{Name: "calc", Invocations: 4, Success: 4, TotalMs: 80},
			{Name: "shell", Invocations: 5, Success: 4, Failure: 1, TotalMs: 1200},
		},
		Skills: []SkillStat{
			{Name: "writer", Success: 2, Failure: 0, TotalCalls: 2},
		},
		Models: []ModelStat{
			{Name: "opus", TokensIn: 900, TokensOut: 180},
			{Name: "haiku", TokensIn: 100, TokensOut: 20},
		},
	}
}

// TestReportJSONRoundTrip verifies the JSON export round-trips back to an equal
// report (the structured export format).
func TestReportJSONRoundTrip(t *testing.T) {
	orig := sampleReport()
	s, err := orig.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var back Report
	if err := json.Unmarshal([]byte(s), &back); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, s)
	}
	if len(back.Sessions) != len(orig.Sessions) || len(back.Tools) != len(orig.Tools) ||
		len(back.Skills) != len(orig.Skills) || len(back.Models) != len(orig.Models) {
		t.Fatalf("JSON round-trip changed slice lengths:\n%s", s)
	}
	if back.Totals != orig.Totals {
		t.Errorf("Totals mismatch: got %+v want %+v", back.Totals, orig.Totals)
	}
	if back.Models[0] != orig.Models[0] {
		t.Errorf("Models[0] mismatch: got %+v want %+v", back.Models[0], orig.Models[0])
	}
}

// TestReportCSV covers the long-format CSV export: header row, presence of every
// section, and that it parses as valid CSV with the expected column count.
func TestReportCSV(t *testing.T) {
	s, err := sampleReport().CSV()
	if err != nil {
		t.Fatalf("CSV: %v", err)
	}
	r := csv.NewReader(strings.NewReader(s))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("CSV does not parse: %v\n%s", err, s)
	}
	if len(records) < 2 {
		t.Fatalf("expected header + rows, got %d records", len(records))
	}
	if len(records[0]) != 4 {
		t.Fatalf("header has %d columns, want 4: %v", len(records[0]), records[0])
	}

	// Collect section names so we can assert every section is represented.
	haveSection := map[string]bool{}
	for _, rec := range records[1:] {
		haveSection[strings.SplitN(rec[0], ":", 2)[0]] = true
	}
	for _, want := range []string{"total", "session", "tool", "skill", "model"} {
		if !haveSection[want] {
			t.Errorf("CSV missing section %q\n%s", want, s)
		}
	}

	// A specific known value should appear verbatim.
	if !strings.Contains(s, ",opus,tokens_in,900") {
		t.Errorf("CSV missing opus tokens_in=900 row\n%s", s)
	}
	// avg_ms is derived (TotalMs/Invocations): calc = 80/4 = 20.
	if !strings.Contains(s, ",calc,avg_ms,20") {
		t.Errorf("CSV missing derived calc avg_ms=20 row\n%s", s)
	}
}

// TestReportCSVMultiLineValue ensures a tool/skill/model name containing a comma
// or quote is escaped correctly by the csv writer (no broken rows).
func TestReportCSVMultiLineValue(t *testing.T) {
	r := Report{Tools: []ToolStat{{Name: "weird,name", Invocations: 1, Success: 1}}}
	s, err := r.CSV()
	if err != nil {
		t.Fatalf("CSV: %v", err)
	}
	records, err := csv.NewReader(strings.NewReader(s)).ReadAll()
	if err != nil {
		t.Fatalf("CSV with comma in name does not parse: %v\n%s", err, s)
	}
	found := false
	for _, rec := range records {
		if len(rec) == 4 && rec[1] == "weird,name" {
			found = true
		}
	}
	if !found {
		t.Errorf("comma-quoted name not preserved:\n%s", s)
	}
}
