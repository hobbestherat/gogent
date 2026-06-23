// Package mathexpr evaluates arithmetic and math expressions.
//
// It is a thin, hardened wrapper around github.com/expr-lang/expr (an eval-safe,
// non-Turing-complete, OSS-Fuzz'd expression engine) exposing a curated math
// environment — a fixed set of FUNCTIONS (thin wrappers over math / math/big)
// and CONSTANTS, and nothing else. No host functions, I/O, or mutation are
// reachable, so evaluation is read-only, side-effect-free and deterministic.
//
// Supported out of the box (from expr): the operators + - * / %, power (** and
// ^), unary minus, comparison and ternary, and parentheses for grouping, with
// correct precedence. The curated environment adds roots/powers, logarithms,
// trigonometric and hyperbolic functions, rounding helpers, combinatorics
// (factorial via math/big, gcd, lcm), basic statistics, and a library of math
// and physics constants. See env.go for the full list.
//
// The evaluator is hardened against malformed input: it never panics and reports
// a descriptive error for anything it cannot evaluate. It is the single shared
// implementation used by the internal "calc" command and the "calc" tool.
package mathexpr

import (
	"errors"
	"fmt"
	"strings"

	"github.com/expr-lang/expr"
)

// ErrEmpty is returned when an expression contains no tokens after stripping
// whitespace.
var ErrEmpty = errors.New("empty expression")

// maxLen bounds the input size. Realistic expressions are tiny; the cap is a
// cheap defence-in-depth guard against a pathologically large expression
// stressing the parser/compiler.
const maxLen = 10000

// Eval parses and evaluates expr against the curated math environment and
// returns the result as a float64. Leading/trailing whitespace is ignored.
//
// It never panics: a compile or run failure (syntax error, unknown name, domain
// error from a function) is returned as a descriptive error, and a defensive
// recover() converts any unexpected internal panic into an error as well. Empty
// input returns ErrEmpty.
func Eval(input string) (result float64, err error) {
	if strings.TrimSpace(input) == "" {
		return 0, ErrEmpty
	}
	if len(input) > maxLen {
		return 0, fmt.Errorf("expression too long (max %d characters)", maxLen)
	}

	// Defence in depth: the contract is "never panic on any input". expr is
	// memory-safe and guards its own recursion, but a recover() here guarantees
	// the contract regardless of the engine's internals.
	defer func() {
		if r := recover(); r != nil {
			result = 0
			err = fmt.Errorf("evaluate expression: %v", r)
		}
	}()

	env := mathEnv()
	program, cerr := expr.Compile(desugarFactorial(input), expr.Env(env), expr.AsFloat64())
	if cerr != nil {
		return 0, fmt.Errorf("invalid expression: %w", cerr)
	}
	out, rerr := expr.Run(program, env)
	if rerr != nil {
		return 0, fmt.Errorf("evaluate expression: %w", rerr)
	}
	f, ok := out.(float64)
	if !ok {
		// expr.AsFloat64() guarantees a float64 result on success; this guards the
		// impossible case rather than asserting and risking a panic.
		return 0, fmt.Errorf("evaluate expression: non-numeric result %v", out)
	}
	return f, nil
}

// EvalFormatted evaluates expr and returns the result rendered with Format: an
// integer-valued result prints without a decimal part, other finite numbers
// print with full float64 precision, and NaN/±Inf print symbolically. It is the
// entry point both calc consumers (the tool and the /calc command) use so they
// agree on formatting. The error path is identical to Eval.
func EvalFormatted(input string) (string, error) {
	v, err := Eval(input)
	if err != nil {
		return "", err
	}
	return Format(v), nil
}

// desugarFactorial rewrites postfix factorial notation ("5!", "(2+3)!",
// "sqrt(4)!") into calls to the factorial() function, which the engine
// understands. expr has no postfix "!" operator — it uses "!" only for logical
// NOT and "!=" — so without this pass "5!" is a syntax error.
//
// It is purely textual, bounded and panic-free on any input: a "!" immediately
// followed by "=" is left untouched (so "!=" survives), and a "!" that is not
// preceded by an operand is left untouched (so a prefix "!" survives). Each pass
// rewrites the first postfix "!" and strictly reduces the count of "!", so the
// loop always terminates.
func desugarFactorial(input string) string {
	s := input
	for k := 0; k <= len(input); k++ {
		i := indexPostfixBang(s)
		if i < 0 {
			return s
		}
		start := operandStart(s, i)
		s = s[:start] + "factorial(" + s[start:i] + ")" + s[i+1:]
	}
	return s
}

// indexPostfixBang returns the index of the first "!" that denotes a postfix
// factorial — one immediately preceded by an operand-terminating byte and not
// part of "!=" — or -1 if there is none.
func indexPostfixBang(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '!' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '=' {
			continue // part of "!="
		}
		if i == 0 {
			continue // a leading "!" is a (logical) prefix, not a factorial
		}
		if isOperandEnd(s[i-1]) {
			return i
		}
	}
	return -1
}

// operandStart returns the index at which the operand immediately to the left of
// the "!" at index i begins. For a parenthesised group it walks back to the
// matching "(" and absorbs any preceding function name (so "sqrt(4)!" keeps
// sqrt); for a bare number/identifier it walks back over the identifier run.
func operandStart(s string, i int) int {
	j := i - 1
	if j < 0 {
		return i
	}
	if s[j] == ')' {
		depth := 0
		for ; j >= 0; j-- {
			switch s[j] {
			case ')':
				depth++
			case '(':
				depth--
			}
			if depth == 0 {
				break
			}
		}
		if j < 0 {
			return 0 // unbalanced; absorb the whole prefix (will error at compile)
		}
		k := j - 1
		for k >= 0 && isIdentByte(s[k]) {
			k--
		}
		return k + 1
	}
	k := j
	for k >= 0 && isIdentByte(s[k]) {
		k--
	}
	// Keep a scientific-notation exponent sign attached to its literal: the run
	// above stops at the '-'/'+' of "1e-3", which would otherwise split the
	// number ("1e-" + factorial(3)). Only absorb a sign that directly follows an
	// 'e'/'E' that is itself part of a number (digit or '.' before it), so an
	// ordinary subtraction like "2-3!" is left as 2 - factorial(3).
	if k >= 2 && (s[k] == '-' || s[k] == '+') && (s[k-1] == 'e' || s[k-1] == 'E') {
		if c := s[k-2]; c >= '0' && c <= '9' || c == '.' {
			k-- // step onto the 'e', which the ident run below re-consumes
			for k >= 0 && isIdentByte(s[k]) {
				k--
			}
		}
	}
	return k + 1
}

// isOperandEnd reports whether b can be the last byte of an operand that a
// postfix "!" could apply to.
func isOperandEnd(b byte) bool {
	return b == ')' || isIdentByte(b)
}

// isIdentByte reports whether b is a byte that can appear inside a numeric or
// identifier operand (digits, letters, '_' and '.'; letters also cover the 'e'
// of scientific notation and constant/function names).
func isIdentByte(b byte) bool {
	return b >= '0' && b <= '9' ||
		b >= 'a' && b <= 'z' ||
		b >= 'A' && b <= 'Z' ||
		b == '_' || b == '.'
}
