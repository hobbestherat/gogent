package tool

import (
	"strings"
	"testing"

	"gogent/internal/mathexpr"
)

// callCalc runs the calc tool's Execute directly and returns the result map.
func callCalc(t *testing.T, expr string) (map[string]interface{}, error) {
	t.Helper()
	reg := NewToolRegistry()
	reg.RegisterCalcTool()
	tool := reg.Get("calc")
	if tool == nil {
		t.Fatal("calc tool not registered")
	}
	res, err := tool.Execute(map[string]interface{}{"expression": expr}, ToolContext{})
	if err != nil {
		return nil, err
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("calc(%q) returned %T, want map[string]interface{}", expr, res)
	}
	return m, nil
}

// TestCalcToolResultShape confirms the tool returns {"expression", "result"}
// with the result as a pre-formatted string the model can read directly.
func TestCalcToolResultShape(t *testing.T) {
	m, err := callCalc(t, "2+2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := m["expression"]; got != "2+2" {
		t.Errorf("expression field = %v, want %q", got, "2+2")
	}
	result, ok := m["result"].(string)
	if !ok {
		t.Fatalf("result field = %T (%v), want a pre-formatted string", m["result"], m["result"])
	}
	if result != "4" {
		t.Errorf("calc(\"2+2\") result = %q, want %q (the old forced %%.4f truncation must be gone)", result, "4")
	}
}

// TestCalcToolCleanFormatting is the headline behaviour change: integers print
// without a trailing ".0000" and fractionals keep full precision.
func TestCalcToolCleanFormatting(t *testing.T) {
	tests := []struct {
		expr string
		want string
	}{
		{"2+2", "4"},
		{"10/2", "5"},
		{"1/3", "0.3333333333333333"},
		{"sqrt(2)", "1.4142135623730951"},
		{"(1+2)*(3+4)", "21"},
		{"1/0", "+Inf"},
		{"0/0", "NaN"},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			m, err := callCalc(t, tc.expr)
			if err != nil {
				t.Fatalf("calc(%q) errored: %v", tc.expr, err)
			}
			if got := m["result"]; got != tc.want {
				t.Errorf("calc(%q) result = %q, want %q", tc.expr, got, tc.want)
			}
			// Regression guard against the removed %.4f formatting.
			if s, _ := m["result"].(string); strings.HasSuffix(s, ".0000") {
				t.Errorf("calc(%q) result %q still uses the old %%.4f formatting", tc.expr, s)
			}
		})
	}
}

// TestCalcToolMatchesEvalFormatted verifies the tool and the shared evaluator
// produce identical output, so the calc tool and the /calc command agree.
func TestCalcToolMatchesEvalFormatted(t *testing.T) {
	for _, expr := range []string{"2+2", "1/3", "sqrt(2)", "sin(pi/2)", "factorial(5)", "G*5.97e24/(6.371e6)^2", "1/0"} {
		want, err := mathexpr.EvalFormatted(expr)
		if err != nil {
			t.Fatalf("EvalFormatted(%q) errored: %v", expr, err)
		}
		m, err := callCalc(t, expr)
		if err != nil {
			t.Fatalf("calc(%q) errored: %v", expr, err)
		}
		if got := m["result"]; got != want {
			t.Errorf("calc(%q) result = %q, EvalFormatted = %q (must agree)", expr, got, want)
		}
	}
}

// TestCalcToolErrors confirms malformed/invalid expressions surface as an error
// from Execute rather than a bogus success or a panic.
func TestCalcToolErrors(t *testing.T) {
	for _, expr := range []string{"", "sqrt(", "abc", "5x", "(1+2", "2>1", `"a"+"b"`} {
		t.Run(expr, func(t *testing.T) {
			if _, err := callCalc(t, expr); err == nil {
				t.Errorf("calc(%q) returned nil error, want an error", expr)
			}
		})
	}
}

// TestCalcToolMissingExpression confirms a missing/invalid expression argument
// is reported, not panicked on.
func TestCalcToolMissingExpression(t *testing.T) {
	reg := NewToolRegistry()
	reg.RegisterCalcTool()
	tool := reg.Get("calc")
	if _, err := tool.Execute(map[string]interface{}{}, ToolContext{}); err == nil {
		t.Error("calc with no expression argument should error")
	}
	if _, err := tool.Execute(map[string]interface{}{"expression": 42}, ToolContext{}); err == nil {
		t.Error("calc with a non-string expression should error")
	}
}

// TestCalcToolIsReadOnly is the safety invariant: calc must stay ReadOnly so it
// keeps running in the parallel fast-path without a permission prompt.
func TestCalcToolIsReadOnly(t *testing.T) {
	reg := NewToolRegistry()
	reg.RegisterCalcTool()
	tool := reg.Get("calc")
	if !tool.ReadOnly {
		t.Error("calc tool must be ReadOnly")
	}
}

// TestCalcToolDescriptionAdvertisesCapability guards the contract update: the
// tool description must advertise the new operators, functions and constants so
// the model knows it can reach for them.
func TestCalcToolDescriptionAdvertisesCapability(t *testing.T) {
	reg := NewToolRegistry()
	reg.RegisterCalcTool()
	desc := reg.Get("calc").Description
	for _, token := range []string{"sqrt", "sin", "log", "pi", "**", "%", "factorial"} {
		if !strings.Contains(desc, token) {
			t.Errorf("calc description should advertise %q; got: %s", token, desc)
		}
	}
}

// TestCalcToolNeverPanics drives the full ExecuteToolCall path (which has its own
// recover) over pathological inputs to confirm the calc tool never crashes the
// process — the ReadOnly fast-path safety guarantee.
func TestCalcToolNeverPanics(t *testing.T) {
	reg := NewToolRegistry()
	reg.RegisterCalcTool()
	inputs := []string{
		"", "()", "5!", "5!!", "1e-3!", "!=", "**", "factorial(", "sqrt(((",
		strings.Repeat("(", 500) + "1" + strings.Repeat(")", 500), "💥", "\x00",
	}
	for _, in := range inputs {
		resp, err := reg.ExecuteToolCall(&ToolCall{Tool: "calc", Args: map[string]interface{}{"expression": in}}, ToolContext{})
		// Either a clean success or a contained error; never a panic (which the
		// loop would surface as `tool "calc" panicked: ...`).
		if err != nil && strings.Contains(err.Error(), "panicked") {
			t.Errorf("calc(%q) panicked: %v", in, err)
		}
		if resp == nil {
			t.Errorf("calc(%q) returned nil response", in)
		}
	}
}
