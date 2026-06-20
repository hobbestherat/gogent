// Package mathexpr evaluates a small subset of arithmetic expressions.
//
// It supports the four basic operators (+, -, *, /), parentheses for grouping,
// and decimal numbers. Expressions are evaluated as IEEE-754 float64.
//
// The evaluator is hardened against malformed input: it never panics and reports
// a descriptive error for anything it cannot evaluate. It is the single shared
// implementation used by the internal "calc" command and the "calc" tool.
package mathexpr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrEmpty is returned when an expression contains no tokens after stripping
// whitespace.
var ErrEmpty = errors.New("empty expression")

// maxDepth bounds the recursive descent so that deeply nested (or otherwise
// pathological) input cannot overflow the call stack. Realistic arithmetic
// expressions are far shallower than this.
const maxDepth = 100000

// Eval parses and evaluates expr. Whitespace is ignored.
func Eval(expr string) (float64, error) {
	expr = strings.ReplaceAll(expr, " ", "")
	if expr == "" {
		return 0, ErrEmpty
	}
	return evalExpr(expr, 0)
}

// evalExpr performs recursive-descent evaluation. depth tracks the recursion
// depth and is bounded by maxDepth to prevent stack overflows.
func evalExpr(expr string, depth int) (float64, error) {
	if depth > maxDepth {
		return 0, errors.New("expression too complex")
	}
	if expr == "" {
		return 0, ErrEmpty
	}

	// Lowest precedence: split on the last top-level + or -.
	level := 0
	for i := len(expr) - 1; i >= 0; i-- {
		c := expr[i]
		if c == ')' {
			level++
		} else if c == '(' {
			level--
		} else if level == 0 && (c == '+' || c == '-') {
			left, err := evalExpr(expr[:i], depth+1)
			if err != nil {
				return 0, err
			}
			right, err := evalExpr(expr[i+1:], depth+1)
			if err != nil {
				return 0, err
			}
			if c == '+' {
				return left + right, nil
			}
			return left - right, nil
		}
	}

	// Peel a single matched pair of surrounding parentheses: "(...)".
	// wrapped reports whether the whole expression is enclosed in one
	// balanced pair, so inputs like "(1+2)*(3+4)" are not mistaken for a wrap.
	if wrapped(expr) {
		return evalExpr(expr[1:len(expr)-1], depth+1)
	}

	// Higher precedence: split on the last top-level * or /.
	level = 0
	for i := len(expr) - 1; i >= 0; i-- {
		c := expr[i]
		if c == ')' {
			level++
		} else if c == '(' {
			level--
		} else if level == 0 && (c == '*' || c == '/') {
			left, err := evalExpr(expr[:i], depth+1)
			if err != nil {
				return 0, err
			}
			right, err := evalExpr(expr[i+1:], depth+1)
			if err != nil {
				return 0, err
			}
			if c == '*' {
				return left * right, nil
			}
			if right == 0 {
				return 0, errors.New("division by zero")
			}
			return left / right, nil
		}
	}

	// Atom: a single number.
	v, err := strconv.ParseFloat(expr, 64)
	if err != nil {
		return 0, fmt.Errorf("parse number: %w", err)
	}
	return v, nil
}

// wrapped reports whether expr is fully enclosed in a single balanced pair of
// parentheses — that is, the opening '(' at the start is matched by the ')' at
// the very end. It never indexes expr out of range and is safe for short or
// empty strings.
func wrapped(expr string) bool {
	if len(expr) < 2 || expr[0] != '(' || expr[len(expr)-1] != ')' {
		return false
	}
	level := 0
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '(':
			level++
		case ')':
			level--
		}
		// The first time nesting returns to zero must be the final byte for the
		// expression to be one continuous wrap.
		if level == 0 {
			return i == len(expr)-1
		}
	}
	return false
}
