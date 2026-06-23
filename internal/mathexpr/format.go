package mathexpr

import (
	"math"
	"strconv"
)

// intExactBound is the largest magnitude below which an integer-valued float64
// is still exactly representable as an integer (2^53). Below it, an
// integer-valued result is printed in plain decimal form; at or above it, the
// value is past float64's exact-integer range, so the general %g formatting
// (which may use scientific notation) is the honest rendering.
const intExactBound = 1 << 53 // 9007199254740992

// Format renders an evaluated result as a clean, human- and model-readable
// string. It replaces the old forced "%.4f" truncation:
//
//   - NaN and ±Inf print symbolically ("NaN", "+Inf", "-Inf") instead of
//     erroring or printing a misleading number.
//   - An integer-valued result within float64's exact-integer range prints
//     without a decimal part: 2+2 -> "4", 1000*1000 -> "1000000", -5 -> "-5".
//   - Any other finite number prints with full float64 precision and the
//     shortest round-trippable representation, dropping trailing zeros and using
//     scientific notation only when appropriate: 1/3 -> "0.3333333333333333",
//     Na -> "6.02214076e+23".
//
// Negative zero is normalised to "0".
func Format(x float64) string {
	switch {
	case math.IsNaN(x):
		return "NaN"
	case math.IsInf(x, 1):
		return "+Inf"
	case math.IsInf(x, -1):
		return "-Inf"
	}
	if x == 0 { // also folds -0 into "0"
		return "0"
	}
	if x == math.Trunc(x) && math.Abs(x) < intExactBound {
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return strconv.FormatFloat(x, 'g', -1, 64)
}
