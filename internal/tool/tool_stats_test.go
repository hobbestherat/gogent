package tool

import (
	"encoding/json"
	"reflect"
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

func TestToolStatsResultBytes(t *testing.T) {
	reg := NewToolRegistry()
	result := map[string]interface{}{
		"message": "hello",
		"items":   []interface{}{"a", "b"},
	}
	reg.Register(&Tool{
		Name:        "payload",
		Description: "returns a structured payload",
		InputSchema: nil,
		Execute: func(map[string]interface{}, ToolContext) (interface{}, error) {
			return result, nil
		},
	})

	resp, err := reg.ExecuteToolCall(&ToolCall{Tool: "payload", Args: map[string]interface{}{}}, ToolContext{})
	if err != nil {
		t.Fatalf("ExecuteToolCall: %v", err)
	}
	if !resp.Success {
		t.Fatalf("payload failed: %s", resp.Error)
	}
	wantBytes := jsonByteLen(t, result)
	if got := reg.ResultBytes("payload"); got != wantBytes {
		t.Fatalf("ResultBytes(payload) = %d, want %d", got, wantBytes)
	}
	stats := reg.GetAllToolStats()
	if len(stats) != 1 {
		t.Fatalf("stats rows = %d, want 1", len(stats))
	}
	if got := stats[0].ResultBytes; got != wantBytes {
		t.Fatalf("stats[0].ResultBytes = %d, want %d", got, wantBytes)
	}

	resp, err = reg.ExecuteToolCall(&ToolCall{Tool: "payload", Args: map[string]interface{}{}}, ToolContext{})
	if err != nil || !resp.Success {
		t.Fatalf("second ExecuteToolCall = resp %+v err %v", resp, err)
	}
	if got, want := reg.ResultBytes("payload"), wantBytes*2; got != want {
		t.Fatalf("ResultBytes after two calls = %d, want %d", got, want)
	}
}

func TestToolStatsResultBytesFailureAndPanicEdges(t *testing.T) {
	reg := NewToolRegistry()
	errorResult := map[string]interface{}{"partial": "diagnostic"}
	reg.Register(&Tool{
		Name:        "error_with_payload",
		Description: "returns a payload and an error",
		InputSchema: nil,
		Execute: func(map[string]interface{}, ToolContext) (interface{}, error) {
			return errorResult, errSentinel
		},
	})
	reg.Register(&Tool{
		Name:        "panic",
		Description: "panics",
		InputSchema: nil,
		Execute: func(map[string]interface{}, ToolContext) (interface{}, error) {
			panic("boom")
		},
	})

	resp, err := reg.ExecuteToolCall(&ToolCall{Tool: "error_with_payload", Args: map[string]interface{}{}}, ToolContext{})
	if err == nil {
		t.Fatal("expected error_with_payload to return an error")
	}
	if resp == nil || resp.Success {
		t.Fatalf("error_with_payload response = %+v, want failure", resp)
	}
	if got, want := reg.ResultBytes("error_with_payload"), jsonByteLen(t, errorResult); got != want {
		t.Fatalf("ResultBytes(error_with_payload) = %d, want %d", got, want)
	}

	resp, err = reg.ExecuteToolCall(&ToolCall{Tool: "panic", Args: map[string]interface{}{}}, ToolContext{})
	if err == nil {
		t.Fatal("expected panic to return an error")
	}
	if resp == nil || resp.Success {
		t.Fatalf("panic response = %+v, want failure", resp)
	}
	if got := reg.ResultBytes("panic"); got != 0 {
		t.Fatalf("ResultBytes(panic) = %d, want 0", got)
	}

	stats := statsByName(reg.GetAllToolStats())
	if got := stats["error_with_payload"]; got.Failure != 1 || got.ResultBytes != jsonByteLen(t, errorResult) {
		t.Fatalf("error_with_payload stats = %+v", got)
	}
	if got := stats["panic"]; got.Failure != 1 || got.ResultBytes != 0 {
		t.Fatalf("panic stats = %+v", got)
	}
}

func TestToolStatsResultBytesNotRecordedForRejectedCalls(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&Tool{
		Name: "needs_arg",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"value": map[string]interface{}{"type": "string"},
			},
			"required": []string{"value"},
		},
		Execute: func(map[string]interface{}, ToolContext) (interface{}, error) {
			return "should not run", nil
		},
	})

	resp, err := reg.ExecuteToolCall(&ToolCall{Tool: "needs_arg", Args: map[string]interface{}{}}, ToolContext{})
	if err != nil {
		t.Fatalf("ExecuteToolCall: %v", err)
	}
	if resp == nil || resp.Success {
		t.Fatalf("invalid args response = %+v, want failure", resp)
	}
	stats := reg.GetAllToolStats()
	if len(stats) != 1 {
		t.Fatalf("stats rows = %d, want 1", len(stats))
	}
	if stats[0].Invocations != 0 || stats[0].ResultBytes != 0 {
		t.Fatalf("rejected call stats = %+v, want zero counters", stats[0])
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

func TestCloneSharesResultBytes(t *testing.T) {
	parent := NewToolRegistry()
	result := map[string]interface{}{"text": "from clone"}
	parent.Register(&Tool{
		Name:        "payload",
		Description: "returns a payload",
		InputSchema: nil,
		Execute: func(map[string]interface{}, ToolContext) (interface{}, error) {
			return result, nil
		},
	})
	parent.Register(&Tool{Name: "spawn_subagent", Execute: func(map[string]interface{}, ToolContext) (interface{}, error) {
		return nil, nil
	}})

	clone := parent.CloneWithout("spawn_subagent")
	resp, err := clone.ExecuteToolCall(&ToolCall{Tool: "payload", Args: map[string]interface{}{}}, ToolContext{})
	if err != nil || !resp.Success {
		t.Fatalf("clone ExecuteToolCall = resp %+v err %v", resp, err)
	}
	if got, want := parent.ResultBytes("payload"), jsonByteLen(t, result); got != want {
		t.Fatalf("parent ResultBytes(payload) = %d, want %d", got, want)
	}
}

func TestToolRegistryListMethodsAreSorted(t *testing.T) {
	reg := NewToolRegistry()
	for _, name := range []string{"zeta", "alpha", "middle", "hidden"} {
		name := name
		reg.Register(&Tool{
			Name: name,
			Execute: func(map[string]interface{}, ToolContext) (interface{}, error) {
				return nil, nil
			},
		})
	}
	reg.SetEnabled("hidden", false)

	if got, want := toolNames(reg.List()), []string{"alpha", "hidden", "middle", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List names = %v, want sorted %v", got, want)
	}
	if got, want := toolNames(reg.ListEnabled()), []string{"alpha", "middle", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListEnabled names = %v, want sorted %v", got, want)
	}
}

func jsonByteLen(t *testing.T, v interface{}) int64 {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return int64(len(b))
}

func statsByName(stats []ToolStats) map[string]ToolStats {
	out := make(map[string]ToolStats, len(stats))
	for _, s := range stats {
		out[s.Name] = s
	}
	return out
}

func toolNames(tools []*Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Name)
	}
	return out
}

// errSentinel is a trivial error for the failure-path test tool.
var errSentinel = simpleErr("boom")

type simpleErr string

func (e simpleErr) Error() string { return string(e) }
