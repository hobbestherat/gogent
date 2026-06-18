package tool

import (
	"testing"
	"time"
)

// TestGetAllToolStats verifies the per-tool breakdown records invocations, the
// success/failure split and execution duration, sorted by name, and that a
// never-run registered tool reports zero counters.
func TestGetAllToolStats(t *testing.T) {
	reg := NewToolRegistry()
	reg.RegisterCalcTool()
	reg.Register(&Tool{Name: "dummy", Description: "d", InputSchema: nil,
		Execute: func(map[string]interface{}, ToolContext) (interface{}, error) { return nil, nil }})

	// calc: two successful calls; dummy never runs.
	reg.ExecuteToolCall(&ToolCall{Tool: "calc", Args: map[string]interface{}{"expression": "1+1"}}, ToolContext{})
	reg.ExecuteToolCall(&ToolCall{Tool: "calc", Args: map[string]interface{}{"expression": "2+2"}}, ToolContext{})

	stats := reg.GetAllToolStats()
	if len(stats) != 2 {
		t.Fatalf("expected 2 tool stats rows, got %d", len(stats))
	}
	// Sorted by name: calc, dummy.
	if stats[0].Name != "calc" || stats[1].Name != "dummy" {
		t.Fatalf("expected sorted calc,dummy; got %s,%s", stats[0].Name, stats[1].Name)
	}
	calc := stats[0]
	if calc.Invocations != 2 || calc.Success != 2 || calc.Failure != 0 {
		t.Errorf("calc = %+v, want 2 invocations / 2 success / 0 failure", calc)
	}
	if calc.TotalMs < 0 {
		t.Errorf("calc TotalMs = %d, must be non-negative", calc.TotalMs)
	}
	// dummy is registered but never invoked.
	if stats[1].Invocations != 0 {
		t.Errorf("dummy invocations = %d, want 0", stats[1].Invocations)
	}
}

// TestToolStatsRecordsFailure verifies a tool whose Execute returns an error is
// counted as a failure rather than a success.
func TestToolStatsRecordsFailure(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&Tool{
		Name:        "boom",
		Description: "always errors",
		InputSchema: nil,
		Execute: func(map[string]interface{}, ToolContext) (interface{}, error) {
			return nil, errSentinel
		},
	})
	// Pass a non-nil args map so validation passes and Execute actually runs.
	if _, err := reg.ExecuteToolCall(&ToolCall{Tool: "boom", Args: map[string]interface{}{}}, ToolContext{}); err == nil {
		t.Fatal("expected boom to return an error")
	}
	got := reg.GetAllToolStats()
	if len(got) != 1 || got[0].Name != "boom" {
		t.Fatalf("expected one boom row, got %+v", got)
	}
	if got[0].Invocations != 1 || got[0].Success != 0 || got[0].Failure != 1 {
		t.Errorf("boom = %+v, want 1 invocation / 0 success / 1 failure", got[0])
	}
}

// TestCloneSharesCounters is the regression test for the latent bug where a
// session's mode-filtered clone kept its own counters, so invocations recorded
// on the clone never reached the global registry the UI/Statistics view reads.
// After the fix, the clone family shares one counter set.
func TestCloneSharesCounters(t *testing.T) {
	parent := NewToolRegistry()
	parent.RegisterCalcTool()
	parent.Register(&Tool{Name: "spawn_subagent", Description: "s", InputSchema: nil,
		Execute: func(map[string]interface{}, ToolContext) (interface{}, error) { return nil, nil }})

	// A session is handed a clone with the sub-agent tools stripped out.
	clone := parent.CloneWithout("spawn_subagent")
	if clone.Get("spawn_subagent") != nil {
		t.Fatal("clone should have spawn_subagent removed")
	}

	// Calls execute on the clone (as they do in a real session).
	clone.ExecuteToolCall(&ToolCall{Tool: "calc", Args: map[string]interface{}{"expression": "1+1"}}, ToolContext{})
	clone.ExecuteToolCall(&ToolCall{Tool: "calc", Args: map[string]interface{}{"expression": "2+2"}}, ToolContext{})

	// The parent (global) registry must observe them too.
	if got := parent.Invocations("calc"); got != 2 {
		t.Errorf("parent Invocations(calc) = %d, want 2 (clone should share counters)", got)
	}
	pstats := parent.GetAllToolStats()
	for _, s := range pstats {
		if s.Name == "calc" && (s.Invocations != 2 || s.Success != 2) {
			t.Errorf("parent tool stats for calc = %+v, want 2 invocations / 2 success", s)
		}
	}

	// LastUsed on the parent reflects the clone's call.
	if parent.LastUsed("calc").IsZero() {
		t.Error("parent LastUsed(calc) should be set after a clone call")
	}
	if parent.LastUsed("calc").After(time.Now().Add(time.Second)) {
		t.Error("parent LastUsed(calc) is in the future")
	}
}

// errSentinel is a trivial error for the failure-path test tool.
var errSentinel = simpleErr("boom")

type simpleErr string

func (e simpleErr) Error() string { return string(e) }
