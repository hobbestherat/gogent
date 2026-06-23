package mathexpr

import (
	"math"
	"strings"
	"testing"
)

// TestFormat covers the result formatter: clean integers, full-precision
// fractionals, large/small magnitudes, negatives, negative zero, and the
// non-finite specials.
func TestFormat(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want string
	}{
		{"integer", 4, "4"},
		{"negative integer", -5, "-5"},
		{"zero", 0, "0"},
		{"negative zero", math.Copysign(0, -1), "0"},
		{"million", 1000000, "1000000"},
		{"fractional full precision", 1.0 / 3.0, "0.3333333333333333"},
		{"trailing zeros dropped", 1.5, "1.5"},
		{"large scientific", 6.02214076e23, "6.02214076e+23"},
		{"small scientific", 1.602176634e-19, "1.602176634e-19"},
		{"NaN", math.NaN(), "NaN"},
		{"positive infinity", math.Inf(1), "+Inf"},
		{"negative infinity", math.Inf(-1), "-Inf"},
		{"negative fractional", -2.5, "-2.5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Format(tc.in); got != tc.want {
				t.Errorf("Format(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFormatPreservesFloatArtifact confirms Format does not hide float64
// rounding artifacts (the old %.4f masked them). The value is computed at
// runtime via Eval so the Go compiler does not constant-fold 0.1+0.2 to 0.3.
func TestFormatPreservesFloatArtifact(t *testing.T) {
	v, err := Eval("0.1+0.2")
	if err != nil {
		t.Fatalf("Eval(\"0.1+0.2\") errored: %v", err)
	}
	if got := Format(v); got != "0.30000000000000004" {
		t.Errorf("Format(0.1+0.2) = %q, want %q (full precision, not truncated)", got, "0.30000000000000004")
	}
}

// TestFormatIntegerBoundary checks that integer-valued results stay in plain
// decimal form right up to the exact-integer bound (2^53) and switch to %g above
// it (where integer-valued float64s are no longer reliably exact).
func TestFormatIntegerBoundary(t *testing.T) {
	below := float64(intExactBound - 2) // 9007199254740990, exactly representable
	if got := Format(below); strings.ContainsAny(got, "eE.") {
		t.Errorf("Format(%v) = %q, want plain integer form below 2^53", below, got)
	}
	above := float64(intExactBound) * 4 // well past 2^53
	if got := Format(above); !strings.ContainsAny(got, "eE") {
		t.Errorf("Format(%v) = %q, want scientific form at/above 2^53", above, got)
	}
}

// TestFormatRoundTrips confirms the %g path emits a representation that parses
// back to the same float64 (shortest round-trippable form) for a spread of
// fractional values.
func TestFormatRoundTrips(t *testing.T) {
	for _, x := range []float64{1.0 / 3.0, math.Pi, math.E, 0.1 + 0.2, 2.5e-17, 9.87654321e10} {
		s := Format(x)
		got, err := Eval(s)
		if err != nil {
			t.Errorf("Format(%v)=%q did not re-parse: %v", x, s, err)
			continue
		}
		if got != x {
			t.Errorf("round-trip failed: Format(%v)=%q re-parsed to %v", x, s, got)
		}
	}
}

// TestEvalFormatted checks the combined entry point: a clean string on success,
// an error (and empty string) on failure.
func TestEvalFormatted(t *testing.T) {
	tests := []struct {
		expr string
		want string
	}{
		{"2+2", "4"},                  // clean integer, no ".0000"
		{"1/3", "0.3333333333333333"}, // full precision
		{"10/2", "5"},
		{"sqrt(2)", "1.4142135623730951"},
		{"1/0", "+Inf"},
		{"0/0", "NaN"},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			got, err := EvalFormatted(tc.expr)
			if err != nil {
				t.Fatalf("EvalFormatted(%q) returned error: %v", tc.expr, err)
			}
			if got != tc.want {
				t.Errorf("EvalFormatted(%q) = %q, want %q", tc.expr, got, tc.want)
			}
		})
	}

	// Error path: empty string + error, never a panic.
	if s, err := EvalFormatted("sqrt("); err == nil {
		t.Errorf("EvalFormatted(\"sqrt(\") = %q, want an error", s)
	} else if s != "" {
		t.Errorf("EvalFormatted error path returned %q, want empty string", s)
	}

	if _, err := EvalFormatted(""); err != ErrEmpty {
		t.Errorf("EvalFormatted(\"\") error = %v, want ErrEmpty", err)
	}
}
