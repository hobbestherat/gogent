package mathexpr

import (
	"math"
	"strings"
	"testing"
)

// tol is the relative tolerance for comparing non-exact float results.
const tol = 1e-12

// closeEnough reports whether got and want agree to within an absolute or
// relative tolerance. Exact integers compare exactly via ==; this is for the
// transcendental cases where the last ULP is not worth pinning.
func closeEnough(got, want float64) bool {
	if got == want {
		return true
	}
	diff := math.Abs(got - want)
	if diff <= tol {
		return true
	}
	return diff <= tol*math.Max(math.Abs(got), math.Abs(want))
}

func mustEval(t *testing.T, expr string) float64 {
	t.Helper()
	got, err := Eval(expr)
	if err != nil {
		t.Fatalf("Eval(%q) returned unexpected error: %v", expr, err)
	}
	return got
}

// TestEvalOperators covers every operator the issue requires: unary minus/plus,
// power (** and ^), modulo, and correct precedence between them.
func TestEvalOperators(t *testing.T) {
	tests := []struct {
		expr string
		want float64
	}{
		{"-5", -5},
		{"+5", 5},
		{"3*-2", -6},
		{"-3*-2", 6},
		{"-(2+3)", -5},
		{"2**3", 8},
		{"2^3", 8},
		{"2**0.5", math.Sqrt2},
		{"2^10", 1024},
		{"-2**2", -4},    // unary minus vs power precedence
		{"2**3**2", 512}, // power is right-associative: 2**(3**2)=2**9
		{"10%3", 1},
		{"10 % 3", 1},
		{"7%4", 3},
		{"2+3*4", 14},    // mul before add
		{"2+3*4**2", 50}, // power before mul before add: 2 + 3*16
		{"(2+3)*4", 20},
		{"2**3+1", 9}, // power before add
		{"2*3%4", 2},  // %/* same level, left-to-right: (2*3)%4=6%4=2
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			if got := mustEval(t, tc.expr); !closeEnough(got, tc.want) {
				t.Errorf("Eval(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// TestEvalFunctions exercises one representative call from every function family
// in the curated env against a known value.
func TestEvalFunctions(t *testing.T) {
	tests := []struct {
		expr string
		want float64
	}{
		// roots / powers
		{"sqrt(2)", math.Sqrt2},
		{"sqrt(16)", 4},
		{"cbrt(27)", 3},
		{"pow(2,10)", 1024},
		{"hypot(3,4)", 5},
		{"exp(0)", 1},
		{"exp2(10)", 1024},
		{"expm1(0)", 0},
		// logs
		{"log(e)", 1},
		{"log2(8)", 3},
		{"log10(1000)", 3},
		{"log1p(0)", 0},
		{"logb(8)", 3},
		// trig (radians)
		{"sin(0)", 0},
		{"cos(0)", 1},
		{"tan(0)", 0},
		{"sin(pi/2)", 1},
		{"asin(1)", math.Pi / 2},
		{"acos(1)", 0},
		{"atan(1)", math.Pi / 4},
		{"atan2(1,1)", math.Pi / 4},
		{"deg(pi)", 180},
		{"rad(180)", math.Pi},
		// hyperbolic
		{"sinh(0)", 0},
		{"cosh(0)", 1},
		{"tanh(0)", 0},
		{"asinh(0)", 0},
		{"acosh(1)", 0},
		{"atanh(0)", 0},
		// rounding / sign / modulo
		{"abs(-5)", 5},
		{"floor(2.9)", 2},
		{"ceil(2.1)", 3},
		{"round(2.5)", 3},
		{"round(-2.5)", -3},
		{"trunc(-2.9)", -2},
		{"sign(-3)", -1},
		{"sign(3)", 1},
		{"sign(0)", 0},
		{"mod(10,3)", 1},
		{"mod(10.5,3)", 1.5}, // float modulo via the function (the % operator cannot do this)
		// min / max (expr builtins, variadic)
		{"min(3,1,2)", 1},
		{"max(3,1,2)", 3},
		// combinatorics / integer math
		{"factorial(5)", 120},
		{"fact(5)", 120},
		{"factorial(0)", 1},
		{"gcd(12,8)", 4},
		{"gcd(-12,8)", 4},
		{"lcm(4,6)", 12},
		{"lcm(0,5)", 0},
		// stats
		{"mean(1,2,3,4)", 2.5},
		{"median(1,2,3,4)", 2.5},
		{"median(1,2,3)", 2},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			if got := mustEval(t, tc.expr); !closeEnough(got, tc.want) {
				t.Errorf("Eval(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// TestEvalConstants checks every constant resolves to the expected value.
func TestEvalConstants(t *testing.T) {
	tests := []struct {
		expr string
		want float64
	}{
		{"pi", math.Pi},
		{"e", math.E},
		{"tau", 2 * math.Pi},
		{"phi", math.Phi},
		{"sqrt2", math.Sqrt2},
		{"ln2", math.Ln2},
		{"log10e", math.Log10E},
		{"c", 299792458.0},
		{"G", 6.67430e-11},
		{"g", 9.80665},
		{"h", 6.62607015e-34},
		{"hbar", 1.054571817e-34},
		{"k", 1.380649e-23},
		{"Na", 6.02214076e23},
		{"R", 8.314462618},
		{"sigma", 5.670374419e-8},
		{"epsilon0", 8.8541878128e-12},
		{"mu0", 1.25663706212e-6},
		{"echarge", 1.602176634e-19},
		{"me", 9.1093837015e-31},
		{"mp", 1.67262192369e-27},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			if got := mustEval(t, tc.expr); got != tc.want {
				t.Errorf("Eval(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// TestEvalNonFiniteConstants checks the special-value constants and that they
// flow through as the expected IEEE-754 specials.
func TestEvalNonFiniteConstants(t *testing.T) {
	if got := mustEval(t, "inf"); !math.IsInf(got, 1) {
		t.Errorf("Eval(\"inf\") = %v, want +Inf", got)
	}
	if got := mustEval(t, "nan"); !math.IsNaN(got) {
		t.Errorf("Eval(\"nan\") = %v, want NaN", got)
	}
}

// TestEvalAcceptanceExpressions is the issue's acceptance list: every one of
// these must evaluate without a parse error.
func TestEvalAcceptanceExpressions(t *testing.T) {
	exprs := []string{
		"-5", "3*-2", "2**3", "2^3", "sqrt(2)", "sin(0)", "10%3", "pi", "5!",
		"0.1+0.2", "log(e)", "deg(180)", "G*5.97e24/(6.371e6)^2",
	}
	for _, e := range exprs {
		t.Run(e, func(t *testing.T) {
			if _, err := Eval(e); err != nil {
				t.Errorf("Eval(%q) returned error, want success: %v", e, err)
			}
		})
	}
}

// TestEvalPostfixFactorial covers the textual factorial desugaring on the forms
// it is meant to handle.
func TestEvalPostfixFactorial(t *testing.T) {
	tests := []struct {
		expr string
		want float64
	}{
		{"5!", 120},
		{"0!", 1},
		{"1!", 1},
		{"(2+3)!", 120}, // parenthesised operand
		{"sqrt(4)!", 2}, // factorial of sqrt(4)=2 -> 2! = 2
		{"3!+1", 7},     // factorial binds to its operand, then +1
		{"2*3!", 12},    // 2 * (3!) = 2*6
		// "!!" desugars to nested calls: 3!! -> factorial(factorial(3)) =
		// factorial(6) = 720. (5!! would be factorial(120) -> +Inf, hence 3 here.)
		{"3!!", 720},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			got, err := Eval(tc.expr)
			if err != nil {
				t.Fatalf("Eval(%q) returned error: %v", tc.expr, err)
			}
			if !closeEnough(got, tc.want) {
				t.Errorf("Eval(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// TestNotEqualOperatorPreserved confirms the factorial desugaring does not
// corrupt the "!=" operator: "5!=3" is parsed as a comparison (5 != 3), which
// yields a bool and therefore an AsFloat64 error rather than a wrong number.
func TestNotEqualOperatorPreserved(t *testing.T) {
	// It must NOT be rewritten to factorial(5)=3. The result is a bool, which the
	// float64-typed evaluator rejects -> an error, never a panic.
	if _, err := Eval("5!=3"); err == nil {
		t.Error("Eval(\"5!=3\") should error (bool result), got nil")
	}
}

// TestEvalDomainErrorsReturnNaN documents that functions with a mathematically
// undefined result return NaN (rendered "NaN") rather than erroring.
func TestEvalDomainErrorsReturnNaN(t *testing.T) {
	for _, e := range []string{"sqrt(-1)", "log(-1)", "asin(2)", "acos(2)", "log(0)"} {
		t.Run(e, func(t *testing.T) {
			got, err := Eval(e)
			if err != nil {
				t.Fatalf("Eval(%q) returned error, want NaN/Inf result: %v", e, err)
			}
			if !math.IsNaN(got) && !math.IsInf(got, 0) {
				t.Errorf("Eval(%q) = %v, want NaN or Inf", e, got)
			}
		})
	}
}

// TestEvalNonFiniteArithmetic documents that division by zero and 0/0 produce
// IEEE-754 specials (not errors), per the new "non-finite prints symbolically"
// contract.
func TestEvalNonFiniteArithmetic(t *testing.T) {
	if got := mustEval(t, "1/0"); !math.IsInf(got, 1) {
		t.Errorf("Eval(\"1/0\") = %v, want +Inf", got)
	}
	if got := mustEval(t, "-1/0"); !math.IsInf(got, -1) {
		t.Errorf("Eval(\"-1/0\") = %v, want -Inf", got)
	}
	if got := mustEval(t, "0/0"); !math.IsNaN(got) {
		t.Errorf("Eval(\"0/0\") = %v, want NaN", got)
	}
}

// TestEvalHardDomainErrors covers the functions that error (rather than return
// NaN) on bad input. Each must surface as an error and never panic.
func TestEvalHardDomainErrors(t *testing.T) {
	exprs := []string{
		"factorial(-1)",  // negative
		"factorial(2.5)", // non-integer
		"factorial(inf)", // non-finite
		"gcd(2.5,3)",     // non-integer
		"lcm(1.5,2)",     // non-integer
		"mean()",         // no args
		"median()",       // no args
	}
	for _, e := range exprs {
		t.Run(e, func(t *testing.T) {
			if _, err := Eval(e); err == nil {
				t.Errorf("Eval(%q) returned nil error, want an error", e)
			}
		})
	}
}

// TestEvalFactorialIsExactForSmallN checks the math/big path gives the exact
// integer for values still representable as float64.
func TestEvalFactorialIsExactForSmallN(t *testing.T) {
	// 20! = 2432902008176640000, exactly representable as float64.
	if got := mustEval(t, "factorial(20)"); got != 2432902008176640000 {
		t.Errorf("Eval(\"factorial(20)\") = %v, want 2432902008176640000", got)
	}
	// Beyond ~170! the value overflows float64 to +Inf rather than panicking.
	if got := mustEval(t, "factorial(200)"); !math.IsInf(got, 1) {
		t.Errorf("Eval(\"factorial(200)\") = %v, want +Inf", got)
	}
}

// TestEvalRejectsNonNumericResults confirms expressions whose result is not a
// number (bool, string, array) error out instead of returning a bogus float or
// panicking.
func TestEvalRejectsNonNumericResults(t *testing.T) {
	for _, e := range []string{"2>1", "true", `"a"+"b"`, "[1,2,3]", "1==1"} {
		t.Run(e, func(t *testing.T) {
			if _, err := Eval(e); err == nil {
				t.Errorf("Eval(%q) returned nil error, want a type error", e)
			}
		})
	}
}

// TestEvalTernary confirms ternary works when the condition is a real bool
// (a comparison), which is the documented use.
func TestEvalTernary(t *testing.T) {
	if got := mustEval(t, "2>1?10:20"); got != 10 {
		t.Errorf("Eval(\"2>1?10:20\") = %v, want 10", got)
	}
	if got := mustEval(t, "1>2?10:20"); got != 20 {
		t.Errorf("Eval(\"1>2?10:20\") = %v, want 20", got)
	}
}

// TestEvalLengthCap ensures an oversized expression is rejected (not parsed).
func TestEvalLengthCap(t *testing.T) {
	long := strings.Repeat("1+", maxLen) + "1"
	if _, err := Eval(long); err == nil {
		t.Error("Eval of an over-long expression should error")
	}
	// Just at the boundary should still be handled without panic (may error from
	// the engine, but must not crash).
	_, _ = Eval(strings.Repeat("(", maxLen))
}

// TestEvalScientificNotation confirms scientific-notation literals parse as
// numbers and are not mistaken for "<number> * e * <number>".
func TestEvalScientificNotation(t *testing.T) {
	tests := []struct {
		expr string
		want float64
	}{
		{"1e3", 1000},
		{"1.5e2", 150},
		{"2e-3", 0.002},
		{"6.022e23", 6.022e23},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			if got := mustEval(t, tc.expr); got != tc.want {
				t.Errorf("Eval(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// TestEvalNeverPanics is a direct (non-fuzz) panic-safety guard over a curated
// set of pathological inputs, mirroring the spirit of tool_panic_test.go for the
// evaluator itself. Any input must yield (value, nil) or (_, error), never a
// panic, and never a non-error NaN-from-crash.
func TestEvalNeverPanics(t *testing.T) {
	inputs := []string{
		"", " ", "()", "(((((((((((((((", ")))))))))))))",
		"!", "!!", "!!!", "5!", "5!!", "!=", "!=!=", "5!=", "=!5",
		"1e-3!", "1e+!", "e!", "pi!", "(!)", "()!", "(2+3)!", "sqrt(4)!",
		"**", "^^", "%%", "//", "--", "++", "*/", "/*", "2**", "**2", "2^", "^2",
		"2%", "%2", "factorial(", "factorial()", "factorial(((", "sqrt", "sqrt(",
		"sin(sin(sin(", "pow(", "pow(1", "pow(1,", "min(", "max(", "mean(",
		"gcd(", "lcm(", "deg(", "1 2 3", "1,2,3", ",", "()()()", "((1)+(2))",
		strings.Repeat("sqrt(", 200) + "2" + strings.Repeat(")", 200),
		strings.Repeat("(", 200) + "1" + strings.Repeat(")", 200),
		strings.Repeat("9", 400), "1" + strings.Repeat("!", 100),
		"\x00", "\xff\xfe", "💥", "sin(💥)", "\t\n\r", "1\x001",
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Eval(%q) panicked: %v", in, r)
				}
			}()
			_, _ = Eval(in)
		}()
	}
}

// --- Known limitations / latent defects, pinned so regressions are visible. ---

// TestModuloOperatorIsIntegerOnly documents that the "%" operator only works on
// integer literals: any float operand errors. This is a USABILITY GAP — the tool
// description advertises "%" as a general modulo operator, but a model writing
// "7.5 % 2" gets an error. mod(x, y) is the float-safe alternative (and works,
// see TestEvalFunctions). Tracked as a finding for the driver.
func TestModuloOperatorIsIntegerOnly(t *testing.T) {
	if got := mustEval(t, "10%3"); got != 1 { // integer literals: OK
		t.Errorf("Eval(\"10%%3\") = %v, want 1", got)
	}
	for _, e := range []string{"10.5%3", "pi%2", "(10+0.5)%3", "2.0%1.0"} {
		if _, err := Eval(e); err == nil {
			t.Errorf("Eval(%q): expected the integer-only %% limitation to error; "+
				"if this now succeeds the limitation was fixed — update this test", e)
		}
	}
}

// TestFactorialDesugarCorruptsScientificNotation pins a latent defect: the
// textual factorial desugaring splits a scientific-notation operand at the
// exponent sign, so "1e-3!" is rewritten to the invalid "1e-factorial(3)" and
// errors. It must still NOT panic (the safety contract holds); the wrong rewrite
// is a correctness bug reported to the driver.
func TestFactorialDesugarCorruptsScientificNotation(t *testing.T) {
	if _, err := Eval("1e-3!"); err == nil {
		t.Log("Eval(\"1e-3!\") no longer errors — the desugar defect may be fixed; review this test")
	}
	// The non-negotiable part: it must not panic (covered broadly by
	// TestEvalNeverPanics; asserted here for documentation).
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Eval(\"1e-3!\") panicked: %v", r)
		}
	}()
	_, _ = Eval("1e-3!")
}

// TestBareComparisonNotUsableStandalone documents that although the tool
// description lists "comparison" as supported, a bare comparison returns a bool
// and is rejected by the float64 evaluator. Comparisons are only usable as the
// condition of a ternary. Reported as a description/contract mismatch.
func TestBareComparisonNotUsableStandalone(t *testing.T) {
	for _, e := range []string{"2>1", "2<1", "2>=2", "2==2", "2!=3"} {
		if _, err := Eval(e); err == nil {
			t.Errorf("Eval(%q): expected bare comparison to error (bool result); "+
				"if it now succeeds the contract changed — update this test", e)
		}
	}
}
