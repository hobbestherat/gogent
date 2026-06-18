package mathexpr

import (
	"math"
	"testing"
)

func TestEval(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want float64
	}{
		{"int", "2+2", 4},
		{"sub", "10-5", 5},
		{"mul", "3*4", 12},
		{"div", "10/2", 5},
		{"precedence add then mul", "2+3*4", 14},
		{"precedence mul then add", "3*4+2", 14},
		{"paren overrides precedence", "(2+3)*4", 20},
		{"grouped product", "(1+2)*(3+4)", 21},
		{"nested parens", "((2+3)*2)", 10},
		{"single in parens", "(42)", 42},
		{"decimal", "1.5+2.5", 4},
		{"whitespace ignored", "  3  +  4  ", 7},
		{"chain", "1+2+3+4+5", 15},
		{"float result", "10/3", 10.0 / 3.0},
		{"single number", "7", 7},
		{"negative result", "3-8", -5},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Eval(tc.expr)
			if err != nil {
				t.Fatalf("Eval(%q) returned unexpected error: %v", tc.expr, err)
			}
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("Eval(%q) returned non-finite result: %v", tc.expr, got)
			}
			if got != tc.want {
				t.Errorf("Eval(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

func TestEvalErrors(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{"empty", ""},
		{"only whitespace", "   "},
		{"empty parens", "()"},
		{"nested empty parens", "(())"},
		{"trailing operator", "5+"},
		{"leading operator", "+5"},
		{"dangling mul", "5*"},
		{"leading div", "/2"},
		{"division by zero", "1/0"},
		{"unbalanced open", "(1+2"},
		{"unbalanced close", "1+2)"},
		{"stray parens", "1)(2"},
		{"letters", "abc"},
		{"number and garbage", "5x"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Eval(tc.expr); err == nil {
				t.Errorf("Eval(%q) returned nil error, want an error", tc.expr)
			}
		})
	}
}

func TestEvalEmptyIsErrEmpty(t *testing.T) {
	if _, err := Eval(""); err != ErrEmpty {
		t.Errorf("Eval(\"\") error = %v, want ErrEmpty", err)
	}
}

// FuzzEval ensures the evaluator never panics on arbitrary input. This is the
// regression target for the index-out-of-range panics that the old, unguarded
// parsers exhibited on malformed expressions like "", "()", and "5+". See
// https://go.dev/doc/security/fuzz/.
func FuzzEval(f *testing.F) {
	seeds := []string{
		"", " ", "1", "2+2", "10-5", "3*4", "10/2", "(2+3)*4", "(1+2)*(3+4)",
		"()", "(())", "5+", "+5", "5*", "/2", "1/0", "(1+2", "1+2)", "1)(2",
		"abc", "5x", "1.5+2.5", "-5", "  3  +  4  ", "((((((", "1+2+3+4+5",
		"(1)", "((1))", "1e3", "2**3", "++", ")(", "*1",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, expr string) {
		// The contract is simply: never panic. Any error is acceptable.
		_, _ = Eval(expr)
	})
}
