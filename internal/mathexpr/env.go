package mathexpr

import (
	"fmt"
	"math"
	"math/big"
	"sort"
)

// mathEnv builds the curated environment exposed to evaluated expressions: a flat
// map of math FUNCTIONS (thin, side-effect-free wrappers over math / math/big)
// and CONSTANTS. It is the ONLY surface the evaluator exposes — no host
// functions, no I/O, no mutation — so evaluation stays read-only, deterministic
// and panic-free.
//
// Conventions:
//   - Functions take and return float64. expr coerces integer literals to
//     float64 at the call site, so sqrt(2) works as written.
//   - Functions that have an undefined result for some inputs (asin(2), log(-1),
//     sqrt(-1)) return NaN rather than erroring — the formatter renders it as
//     "NaN". Functions with a hard domain requirement (factorial, gcd, lcm)
//     return a descriptive error, which expr surfaces from Run without panicking.
//   - Identifiers are case-sensitive and cannot carry subscripts, so constants
//     like epsilon0/mu0/hbar/echarge are spelled out as flat names.
//
// The map is rebuilt per Eval call (it is cheap — a few dozen entries) so the
// environment is never shared mutable state between evaluations.
func mathEnv() map[string]interface{} {
	env := make(map[string]interface{}, 80)

	// --- Roots and powers ---
	env["sqrt"] = math.Sqrt
	env["cbrt"] = math.Cbrt
	env["pow"] = math.Pow     // pow(x, y) = x**y
	env["hypot"] = math.Hypot // hypot(x, y) = sqrt(x*x + y*y)
	env["exp"] = math.Exp
	env["exp2"] = math.Exp2
	env["expm1"] = math.Expm1

	// --- Logarithms ---
	env["log"] = math.Log // natural log
	env["log2"] = math.Log2
	env["log10"] = math.Log10
	env["log1p"] = math.Log1p
	env["logb"] = math.Logb

	// --- Trigonometry (radians) ---
	env["sin"] = math.Sin
	env["cos"] = math.Cos
	env["tan"] = math.Tan
	env["asin"] = math.Asin
	env["acos"] = math.Acos
	env["atan"] = math.Atan
	env["atan2"] = math.Atan2
	// Angle-unit convenience wrappers so the model can work in either unit.
	env["deg"] = func(rad float64) float64 { return rad * 180 / math.Pi } // radians -> degrees
	env["rad"] = func(deg float64) float64 { return deg * math.Pi / 180 } // degrees -> radians

	// --- Hyperbolic ---
	env["sinh"] = math.Sinh
	env["cosh"] = math.Cosh
	env["tanh"] = math.Tanh
	env["asinh"] = math.Asinh
	env["acosh"] = math.Acosh
	env["atanh"] = math.Atanh

	// --- Rounding / sign / modulo ---
	// abs/floor/ceil/round also exist as expr builtins; we register explicit
	// math-package versions so the semantics are pinned and documented.
	env["abs"] = math.Abs
	env["floor"] = math.Floor
	env["ceil"] = math.Ceil
	env["round"] = math.Round // half away from zero
	env["trunc"] = math.Trunc
	env["mod"] = math.Mod // mod(x, y), IEEE-754 remainder with the sign of x
	env["sign"] = func(x float64) float64 {
		switch {
		case math.IsNaN(x):
			return math.NaN()
		case x > 0:
			return 1
		case x < 0:
			return -1
		default:
			return 0
		}
	}

	// --- Min / max (variadic) ---
	// expr ships min/max builtins, but — like abs/floor/ceil/round above — we
	// register explicit float64 versions so the semantics are pinned, documented
	// and panic-free (an empty call errors rather than indexing an empty slice).
	env["min"] = minOf
	env["max"] = maxOf

	// --- Combinatorics / integer math ---
	env["fact"] = factorial
	env["factorial"] = factorial
	env["gcd"] = gcd
	env["lcm"] = lcm

	// --- Simple statistics (variadic over the call arguments) ---
	env["sum"] = sum
	env["mean"] = mean
	env["median"] = median

	// --- Pure-math constants ---
	env["pi"] = math.Pi
	env["e"] = math.E
	env["tau"] = 2 * math.Pi
	env["phi"] = math.Phi // golden ratio
	env["sqrt2"] = math.Sqrt2
	env["ln2"] = math.Ln2
	env["log10e"] = math.Log10E
	env["inf"] = math.Inf(1)
	env["nan"] = math.NaN()

	// --- Physics constants (SI, CODATA) ---
	env["c"] = 299792458.0             // speed of light in vacuum, m/s
	env["G"] = 6.67430e-11             // gravitational constant, m^3 kg^-1 s^-2
	env["g"] = 9.80665                 // standard gravity, m/s^2
	env["h"] = 6.62607015e-34          // Planck constant, J*s
	env["hbar"] = 1.054571817e-34      // reduced Planck constant, J*s
	env["k"] = 1.380649e-23            // Boltzmann constant, J/K
	env["Na"] = 6.02214076e23          // Avogadro constant, 1/mol
	env["R"] = 8.314462618             // molar gas constant, J/(mol*K)
	env["sigma"] = 5.670374419e-8      // Stefan-Boltzmann constant, W/(m^2*K^4)
	env["epsilon0"] = 8.8541878128e-12 // vacuum permittivity, F/m
	env["mu0"] = 1.25663706212e-6      // vacuum permeability, N/A^2
	env["echarge"] = 1.602176634e-19   // elementary charge, C (named to avoid clashing with e)
	env["me"] = 9.1093837015e-31       // electron mass, kg
	env["mp"] = 1.67262192369e-27      // proton mass, kg

	return env
}

// factorial returns n! for a non-negative integer n. It computes the product
// with math/big so large intermediate values do not overflow before being
// rounded to the nearest float64 (e.g. fact(20) is the correctly-rounded
// 2432902008176640000). Non-integer, negative, non-finite or excessively large
// arguments return a descriptive error rather than a wrong number or a panic.
func factorial(n float64) (float64, error) {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("factorial: argument must be finite")
	}
	if n < 0 || n != math.Trunc(n) {
		return 0, fmt.Errorf("factorial: requires a non-negative integer, got %v", n)
	}
	// 171! already overflows float64 to +Inf, so short-circuit there: it gives the
	// correct float64 result (+Inf) without spinning a huge, ultimately-discarded
	// big.Int multiply for a hostile argument like factorial(1e9).
	if n > 170 {
		return math.Inf(1), nil
	}
	r := big.NewInt(1)
	for i := int64(2); i <= int64(n); i++ {
		r.Mul(r, big.NewInt(i))
	}
	f, _ := new(big.Float).SetInt(r).Float64()
	return f, nil
}

// gcd returns the greatest common divisor of two integers (given as float64).
// Both arguments must be integral; the result is always non-negative.
func gcd(a, b float64) (float64, error) {
	ia, ib, err := asInt64Pair("gcd", a, b)
	if err != nil {
		return 0, err
	}
	return float64(gcdInt(ia, ib)), nil
}

// lcm returns the least common multiple of two integers (given as float64).
// lcm(0, x) is 0. Both arguments must be integral.
func lcm(a, b float64) (float64, error) {
	ia, ib, err := asInt64Pair("lcm", a, b)
	if err != nil {
		return 0, err
	}
	if ia == 0 || ib == 0 {
		return 0, nil
	}
	g := gcdInt(ia, ib)
	// Compute via big.Int to avoid int64 overflow, then round to float64.
	l := new(big.Int).Abs(big.NewInt(ia))
	l.Div(l, big.NewInt(g))
	l.Mul(l, new(big.Int).Abs(big.NewInt(ib)))
	f, _ := new(big.Float).SetInt(l).Float64()
	return f, nil
}

// asInt64Pair validates that both arguments are finite integers and returns them
// as int64. name labels the error for the caller (gcd/lcm).
func asInt64Pair(name string, a, b float64) (int64, int64, error) {
	for _, v := range [2]float64{a, b} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
			return 0, 0, fmt.Errorf("%s: arguments must be integers, got %v and %v", name, a, b)
		}
		if math.Abs(v) >= math.MaxInt64 {
			return 0, 0, fmt.Errorf("%s: argument out of range", name)
		}
	}
	return int64(a), int64(b), nil
}

// gcdInt is the Euclidean gcd on int64, returning a non-negative result.
func gcdInt(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// minOf returns the smallest of its arguments. A NaN argument is ignored unless
// it is the only value seen so far. minOf() with no arguments is an error.
func minOf(xs ...float64) (float64, error) {
	if len(xs) == 0 {
		return 0, fmt.Errorf("min: requires at least one argument")
	}
	m := xs[0]
	for _, x := range xs[1:] {
		if x < m {
			m = x
		}
	}
	return m, nil
}

// maxOf returns the largest of its arguments. maxOf() with no arguments is an
// error.
func maxOf(xs ...float64) (float64, error) {
	if len(xs) == 0 {
		return 0, fmt.Errorf("max: requires at least one argument")
	}
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m, nil
}

// sum returns the total of its arguments. sum() with no arguments is 0, the
// natural identity, so it never errors.
func sum(xs ...float64) float64 {
	var total float64
	for _, x := range xs {
		total += x
	}
	return total
}

// mean returns the arithmetic mean of its arguments. mean() with no arguments is
// an error.
func mean(xs ...float64) (float64, error) {
	if len(xs) == 0 {
		return 0, fmt.Errorf("mean: requires at least one argument")
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs)), nil
}

// median returns the median of its arguments (the mean of the two middle values
// for an even count). It sorts a copy, leaving the caller's slice untouched.
func median(xs ...float64) (float64, error) {
	if len(xs) == 0 {
		return 0, fmt.Errorf("median: requires at least one argument")
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2], nil
	}
	return (s[n/2-1] + s[n/2]) / 2, nil
}
