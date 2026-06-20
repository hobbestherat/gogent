package tool

import (
	"errors"
	"strings"
	"testing"
)

// TestExecuteToolCallRecoversPanic verifies that a tool whose Execute panics is
// contained: ExecuteToolCall returns a failed response plus an error describing
// the panic, the process does not crash, and the panic is counted as a failed
// invocation (issue #8). It is table-driven over the kinds of values a tool may
// panic with.
func TestExecuteToolCallRecoversPanic(t *testing.T) {
	tests := []struct {
		name    string
		panicV  interface{}
		wantSub string
	}{
		{"string", "kaboom", "kaboom"},
		{"error", errors.New("bad assertion"), "bad assertion"},
		{"int", 42, "42"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewToolRegistry()
			reg.Register(&Tool{
				Name:        "boom",
				Description: "always panics",
				InputSchema: nil,
				Execute: func(map[string]interface{}, ToolContext) (interface{}, error) {
					panic(tc.panicV)
				},
			})

			resp, err := reg.ExecuteToolCall(&ToolCall{Tool: "boom", Args: map[string]interface{}{}}, ToolContext{})
			if err == nil {
				t.Fatal("expected an error from a panicking tool")
			}
			if resp == nil {
				t.Fatal("expected a non-nil response so callers don't deref nil")
			}
			if resp.Success {
				t.Fatal("a panicking tool must not report success")
			}
			if !strings.Contains(resp.Error, tc.wantSub) || !strings.Contains(resp.Error, "panicked") {
				t.Fatalf("error %q should mention the panic and %q", resp.Error, tc.wantSub)
			}

			// The panic is still a single, failed invocation in the stats.
			stats := reg.GetAllToolStats()
			if len(stats) != 1 || stats[0].Name != "boom" {
				t.Fatalf("expected one boom row, got %+v", stats)
			}
			if stats[0].Invocations != 1 || stats[0].Success != 0 || stats[0].Failure != 1 {
				t.Errorf("boom stats = %+v, want 1 invocation / 0 success / 1 failure", stats[0])
			}
		})
	}
}

// TestExecuteToolCallNoPanicUnaffected is a guard that the recover path does not
// disturb the normal success path: a well-behaved tool still returns its result.
func TestExecuteToolCallNoPanicUnaffected(t *testing.T) {
	reg := NewToolRegistry()
	reg.RegisterCalcTool()
	resp, err := reg.ExecuteToolCall(&ToolCall{Tool: "calc", Args: map[string]interface{}{"expression": "2+2"}}, ToolContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || !resp.Success {
		t.Fatalf("expected a successful calc response, got %+v", resp)
	}
}
