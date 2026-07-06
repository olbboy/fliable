package dmn

import (
	"fmt"
	"strings"

	"github.com/olbboy/fliable/expr"
)

// matchUnaryTest evaluates a FEEL-flavored unary test cell against an
// input value. Supported forms:
//
//   - any value (also empty cell)
//     < 10, <= 10, > 1, >= 1, != "x"   comparison against the input
//     [1..10], (0..5], [0..5)          ranges with open/closed bounds
//     "gold", "silver"                 comma list = membership
//     not(<test>)                      negation
//     anything else                    full expression; `?` names the input
//
// Expressions run in the sandboxed Fliable language with the decision
// context available, so cells like `? >= limit * 2` work.
func matchUnaryTest(cell string, input any, vars map[string]any) (bool, error) {
	cell = strings.TrimSpace(cell)
	if cell == "" || cell == "-" {
		return true, nil
	}
	if strings.HasPrefix(cell, "not(") && strings.HasSuffix(cell, ")") {
		ok, err := matchUnaryTest(cell[4:len(cell)-1], input, vars)
		return !ok, err
	}

	// Range: [a..b], (a..b), mixed brackets.
	if len(cell) > 3 && (cell[0] == '[' || cell[0] == '(') && (cell[len(cell)-1] == ']' || cell[len(cell)-1] == ')') && strings.Contains(cell, "..") {
		return matchRange(cell, input, vars)
	}

	// Comma-separated membership list (top-level commas only).
	if parts := splitTop(cell, ','); len(parts) > 1 {
		for _, p := range parts {
			ok, err := matchUnaryTest(p, input, vars)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}

	// Leading comparison operator applies to the input.
	for _, op := range []string{"<=", ">=", "!=", "<", ">"} {
		if strings.HasPrefix(cell, op) {
			return evalWithInput(fmt.Sprintf("__input %s (%s)", op, cell[len(op):]), input, vars)
		}
	}

	// Full expression. `?` refers to the input value.
	src := strings.ReplaceAll(cell, "?", "__input")
	v, err := evalCell(src, input, vars)
	if err != nil {
		return false, err
	}
	// Expressions that reference the input decide on their own; any other
	// value (including boolean literals) means equality with the input.
	if b, ok := v.(bool); ok && strings.Contains(src, "__input") {
		return b, nil
	}
	return equalLoose(input, v), nil
}

func matchRange(cell string, input any, vars map[string]any) (bool, error) {
	openIncl := cell[0] == '['
	closeIncl := cell[len(cell)-1] == ']'
	inner := cell[1 : len(cell)-1]
	parts := strings.SplitN(inner, "..", 2)
	if len(parts) != 2 {
		return false, fmt.Errorf("dmn: bad range %q", cell)
	}
	lowOp, highOp := ">", "<"
	if openIncl {
		lowOp = ">="
	}
	if closeIncl {
		highOp = "<="
	}
	src := fmt.Sprintf("__input %s (%s) && __input %s (%s)", lowOp, parts[0], highOp, parts[1])
	return evalWithInput(src, input, vars)
}

func evalWithInput(src string, input any, vars map[string]any) (bool, error) {
	v, err := evalCell(src, input, vars)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("dmn: test %q did not yield a boolean", src)
	}
	return b, nil
}

func evalCell(src string, input any, vars map[string]any) (any, error) {
	env := make(map[string]any, len(vars)+1)
	for k, v := range vars {
		env[k] = v
	}
	env["__input"] = input
	return expr.Eval(src, env)
}

func equalLoose(a, b any) bool {
	return expr.Stringify(expr.Normalize(a)) == expr.Stringify(expr.Normalize(b))
}

// splitTop splits on sep at nesting depth zero (respecting quotes,
// parens and brackets).
func splitTop(s string, sep byte) []string {
	var parts []string
	depth := 0
	inStr := byte(0)
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr != 0:
			if c == inStr && (i == 0 || s[i-1] != '\\') {
				inStr = 0
			}
		case c == '"' || c == '\'':
			inStr = c
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case c == sep && depth == 0:
			parts = append(parts, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	parts = append(parts, strings.TrimSpace(s[start:]))
	return parts
}
