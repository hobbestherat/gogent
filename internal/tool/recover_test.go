package tool

import (
	"errors"
	"strings"
	"testing"
)

// panickingRegistry builds a registry with a single tool whose Execute panics
// with panicVal, so the recovery in ExecuteToolCall can be exercised without a
// real crashing code path.
func panickingRegistry(name string, panicVal interface{}) *ToolRegistry {
	reg := NewToolRegistry()
	reg.Register(&Tool{
		Name:        name,
		Description: "always panics",
		InputSchema: map[string]interface{}{"type": "object"},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			panic(panicVal)
		},
	})
	return reg
}

// TestExecuteToolCallRecoversPanic verifies that a panicking tool (or the math
// parser a tool may invoke) is contained to a single tool call (issue #8): the
// panic is converted into a failure response and a Go error and recorded as a
// failure, rather than propagating up and crashing the process. The table spans
// the panic value kinds Go permits.
func TestExecuteToolCallRecoversPanic(t *testing.T) {
	sentinel := errors.New("sentinel panic")
	tests := []struct {
		name     string
		panicVal interface{}
		wantSub  string // expected substring in the surfaced error text
	}{
		{"string panic", "boom", "boom"},
		{"error panic", sentinel, "sentinel panic"},
		{"non-string panic", 42, "42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := panickingRegistry("bomb", tc.panicVal)
			resp, err := reg.ExecuteToolCall(&ToolCall{
				Tool: "bomb",
				Args: map[string]interface{}{},
			}, ToolContext{})

			// The call must return normally rather than panic the test process.
			if err == nil {
				t.Fatal("expected a non-nil error from a panicking tool")
			}
			if resp == nil {
				t.Fatal("expected a failure response, got nil")
			}
			if resp.Success {
				t.Fatal("a panicking tool must not report success")
			}
			if !strings.Contains(resp.Error, "panicked") {
				t.Errorf("response error should mention the panic, got %q", resp.Error)
			}
			if !strings.Contains(resp.Error, tc.wantSub) {
				t.Errorf("response error should contain %q, got %q", tc.wantSub, resp.Error)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("returned Go error should contain %q, got %q", tc.wantSub, err.Error())
			}

			// A recovered call still counts as one invocation recorded as a failure.
			if got := reg.Invocations("bomb"); got != 1 {
				t.Errorf("expected 1 invocation, got %d", got)
			}
			stats := reg.GetAllToolStats()
			var row *ToolStats
			for i := range stats {
				if stats[i].Name == "bomb" {
					row = &stats[i]
				}
			}
			if row == nil {
				t.Fatal("missing stats row for bomb")
			}
			if row.Failure != 1 || row.Success != 0 {
				t.Errorf("expected 1 failure and 0 successes, got failure=%d success=%d", row.Failure, row.Success)
			}
		})
	}
}

// TestExecuteToolCallRecoversPanicDoesNotPoisonRegistry verifies the registry is
// usable after a recovered panic: a later, well-behaved call still runs and
// succeeds, proving the crash did not corrupt shared registry state.
func TestExecuteToolCallRecoversPanicDoesNotPoisonRegistry(t *testing.T) {
	reg := panickingRegistry("bomb", "kaboom")
	if _, err := reg.ExecuteToolCall(&ToolCall{Tool: "bomb", Args: map[string]interface{}{}}, ToolContext{}); err == nil {
		t.Fatal("first call should have returned a panic error")
	}

	reg.RegisterCalcTool()
	resp, err := reg.ExecuteToolCall(&ToolCall{
		Tool: "calc",
		Args: map[string]interface{}{"expression": "6*7"},
	}, ToolContext{})
	if err != nil || !resp.Success {
		t.Fatalf("registry must still execute a healthy tool after a recovered panic, err=%v resp=%+v", err, resp)
	}
}
