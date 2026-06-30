package gogent

import (
	"strings"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/model"
	"gogent/internal/tool"
)

// TestStatisticsAggregates covers gogent.Statistics joining the per-session
// counters, per-model token attribution, per-tool usage and per-skill usage into
// one report with correct totals.
func TestStatisticsAggregates(t *testing.T) {
	g := NewGogent("/tmp/test-stats")

	// Two sessions with known token/turn/tool-call figures.
	us1 := makeStatsSession(g, "session-1")
	us1.SetPrimaryModel("opus")
	us1.AddTokenUsage(500, 100)
	us1.AddTokenUsage(300, 60)
	us1.IncrementToolCall()
	us1.IncrementToolCall()

	us2 := makeStatsSession(g, "session-2")
	us2.SetPrimaryModel("haiku")
	us2.AddTokenUsage(200, 40)
	us2.IncrementToolCall()

	// Exercise a tool via the global registry so its counters populate.
	reg := g.GetToolRegistry()
	calcCall := &tool.ToolCall{Tool: "calc", Args: map[string]interface{}{"expression": "1+1"}}
	for i := 0; i < 3; i++ {
		reg.ExecuteToolCall(calcCall, tool.ToolContext{})
	}

	rep := g.Statistics()

	// Totals sum across sessions.
	if rep.Totals.Sessions != 2 {
		t.Errorf("Totals.Sessions = %d, want 2", rep.Totals.Sessions)
	}
	if rep.Totals.TokensIn != 1000 || rep.Totals.TokensOut != 200 {
		t.Errorf("Totals tokens = %d/%d, want 1000/200", rep.Totals.TokensIn, rep.Totals.TokensOut)
	}
	if rep.Totals.ToolCalls != 3 {
		t.Errorf("Totals.ToolCalls = %d, want 3", rep.Totals.ToolCalls)
	}

	// Per-session rows are present, oldest first.
	if len(rep.Sessions) != 2 {
		t.Fatalf("Sessions = %d, want 2: %+v", len(rep.Sessions), rep.Sessions)
	}
	if rep.Sessions[0].ID != "session-1" || rep.Sessions[1].ID != "session-2" {
		t.Errorf("session order = %s,%s, want session-1,session-2",
			rep.Sessions[0].ID, rep.Sessions[1].ID)
	}

	// Per-model attribution aggregates across sessions.
	wantModels := map[string][2]int{"opus": {800, 160}, "haiku": {200, 40}}
	gotModels := map[string][2]int{}
	for _, m := range rep.Models {
		gotModels[m.Name] = [2]int{m.TokensIn, m.TokensOut}
	}
	for name, want := range wantModels {
		if gotModels[name] != want {
			t.Errorf("model %s = %v, want %v", name, gotModels[name], want)
		}
	}

	// Per-tool row for calc reflects the three executions.
	found := false
	for _, ts := range rep.Tools {
		if ts.Name == "calc" {
			found = true
			if ts.Invocations != 3 || ts.Success != 3 {
				t.Errorf("calc stats = %+v, want 3 invocations / 3 success", ts)
			}
		}
	}
	if !found {
		t.Error("expected a calc tool row in Tools")
	}
}

// TestStatisticsExport exercises the CSV/JSON formatters via the report returned
// by Statistics, ensuring the export path runs end to end against real data.
func TestStatisticsExport(t *testing.T) {
	g := NewGogent("/tmp/test-stats-export")
	us := makeStatsSession(g, "session-1")
	us.SetPrimaryModel("opus")
	us.AddTokenUsage(1000, 200)

	rep := g.Statistics()

	csv, err := rep.CSV()
	if err != nil {
		t.Fatalf("CSV: %v", err)
	}
	if !strings.Contains(csv, "section,name,metric,value") {
		t.Errorf("CSV missing header:\n%s", csv)
	}
	if !strings.Contains(csv, "total,,tokens_in,1000") {
		t.Errorf("CSV missing total tokens_in=1000:\n%s", csv)
	}
	if !strings.Contains(csv, ",opus,tokens_in,1000") {
		t.Errorf("CSV missing per-model opus row:\n%s", csv)
	}

	js, err := rep.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if !strings.Contains(js, `"generated_at"`) || !strings.Contains(js, `"opus"`) {
		t.Errorf("JSON missing expected fields:\n%s", js)
	}
}

// TestStatisticsOrderStable verifies the session rows come back in creation order
// regardless of map iteration order.
func TestStatisticsOrderStable(t *testing.T) {
	g := NewGogent("/tmp/test-stats-order")
	ids := []string{"session-a", "session-b", "session-c", "session-d"}
	for i, id := range ids {
		us := makeStatsSession(g, id)
		us.CreatedAt = int64(1000 + i) // deterministic order independent of map iteration
	}

	rep := g.Statistics()
	got := make([]string, len(rep.Sessions))
	for i, s := range rep.Sessions {
		got[i] = s.ID
	}
	if strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Errorf("session order = %v, want %v (creation order)", got, ids)
	}
}

// TestStatisticsEmpty verifies a fresh gogent with no sessions produces a valid,
// zeroed report rather than panicking.
func TestStatisticsEmpty(t *testing.T) {
	g := NewGogent("/tmp/test-stats-empty")
	rep := g.Statistics()
	if rep.Totals.Sessions != 0 || len(rep.Sessions) != 0 {
		t.Errorf("empty report = %+v, want zero sessions", rep.Totals)
	}
	// Tools are still listed (registered but unused) with zero counters.
	for _, ts := range rep.Tools {
		if ts.Invocations != 0 {
			t.Errorf("unused tool %s has invocations %d, want 0", ts.Name, ts.Invocations)
		}
	}
}

// makeStatsSession builds and registers a user session backed by a fresh model
// session, mirroring how gogent.CreateUserSession wires one.
func makeStatsSession(g *Gogent, id string) *agent.UserSession {
	conn := testConn()
	sess := model.NewModelSession(id, conn)
	root := agent.NewAgent("root", sess)
	root.SetState(agent.StateIdle)
	return g.CreateUserSession(id, root)
}
