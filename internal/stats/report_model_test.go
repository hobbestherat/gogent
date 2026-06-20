package stats

import (
	"strings"
	"testing"
)

// TestReportModelByName covers the per-model lookup the Overall panel's selector
// uses to scope metrics (issue #191): it returns the matching ModelStat by config
// name, reports found=false for an unknown or empty name, and picks the right one
// when several models are present.
func TestReportModelByName(t *testing.T) {
	rep := Report{Models: []ModelStat{
		{Name: "groq", TokensIn: 100, Sessions: 1},
		{Name: "glm", TokensIn: 250, Sessions: 2, SubAgents: 3,
			Connector: ConnectorStat{Requests: 9, Errors: 1, TokensIn: 250, CachedTokensIn: 50}},
		{Name: "haiku", TokensIn: 40},
	}}

	got, ok := rep.ModelByName("glm")
	if !ok {
		t.Fatal("ModelByName(glm) = false, want true")
	}
	if got.Sessions != 2 || got.SubAgents != 3 || got.Connector.Requests != 9 {
		t.Errorf("glm row = %+v, want {Sessions:2 SubAgents:3 Requests:9}", got)
	}

	// Unknown model.
	if _, ok := rep.ModelByName("nope"); ok {
		t.Error("ModelByName(unknown) = true, want false")
	}

	// Empty name (the "all models" aggregate option) never matches a real entry;
	// the caller must fall back to the report's grand totals for the aggregate.
	if _, ok := rep.ModelByName(""); ok {
		t.Error("ModelByName(\"\") = true, want false (aggregate uses Totals, not a model row)")
	}

	// Correct disambiguation when several models exist.
	if got, ok := rep.ModelByName("haiku"); !ok || got.TokensIn != 40 {
		t.Errorf("ModelByName(haiku) = %+v ok=%v, want TokensIn 40", got, ok)
	}
}

// TestReportModelByName_EmptyReport ensures a report with no model breakdown
// degrades cleanly (found=false) rather than panicking.
func TestReportModelByName_EmptyReport(t *testing.T) {
	rep := Report{}
	if _, ok := rep.ModelByName("anything"); ok {
		t.Error("ModelByName on empty report = true, want false")
	}
}

// TestReportCSV_IncludesModelConnectorRows pins the new per-model CSV rows added
// for issue #191: sessions, sub_agents, and the connector breakdown must all be
// emitted under the model section so a model-scoped export is complete.
func TestReportCSV_IncludesModelConnectorRows(t *testing.T) {
	rep := Report{Models: []ModelStat{{
		Name: "glm", TokensIn: 250, TokensOut: 60, Sessions: 2, SubAgents: 4,
		Connector: ConnectorStat{Requests: 9, Errors: 1, TokensIn: 250, CachedTokensIn: 50},
	}}}

	out, err := rep.CSV()
	if err != nil {
		t.Fatalf("CSV: %v", err)
	}
	// Token/session rows use the (section,name,metric,value) shape; the connector
	// breakdown is emitted by writeConnectorCSV as section "model:<name>:connector"
	// with an empty second column, hence the ",,".
	wantSubstrings := []string{
		"model,glm,tokens_in,250",
		"model,glm,tokens_out,60",
		"model,glm,sessions,2",
		"model,glm,sub_agents,4",
		"model:glm:connector,,requests,9",
		"model:glm:connector,,errors,1",
		"model:glm:connector,,tokens_in,250",
		"model:glm:connector,,cached_tokens_in,50",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(out, s) {
			t.Errorf("CSV missing %q\n--- CSV ---\n%s", s, out)
		}
	}
}
