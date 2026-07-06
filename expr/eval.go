package expr

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// Program is a compiled expression, immutable and safe for concurrent use.
type Program struct {
	src  string
	root node
}

// Source returns the original expression text.
func (p *Program) Source() string { return p.src }

// EvalError describes a runtime evaluation failure.
type EvalError struct {
	Src string
	Msg string
}

func (e *EvalError) Error() string { return fmt.Sprintf("expr: %s in %q", e.Msg, e.Src) }

// Func is a pure function callable from expressions.
type Func func(args []any) (any, error)

// envFrame carries evaluation state.
type envFrame struct {
	vars  map[string]any
	funcs map[string]Func
	src   string
}

func (e *envFrame) errf(format string, args ...any) error {
	return &EvalError{Src: e.src, Msg: fmt.Sprintf(format, args...)}
}

// Eval evaluates the program against vars using the built-in functions.
func (p *Program) Eval(vars map[string]any) (any, error) {
	return p.EvalWith(vars, nil)
}

// EvalWith evaluates with extra functions layered over the built-ins.
// Extra functions win on name collision.
func (p *Program) EvalWith(vars map[string]any, funcs map[string]Func) (any, error) {
	fr := &envFrame{vars: vars, funcs: funcs, src: p.src}
	v, err := p.root.eval(fr)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// EvalBool evaluates the program and coerces the result to a boolean using
// the language's truthiness rules.
func (p *Program) EvalBool(vars map[string]any) (bool, error) {
	v, err := p.Eval(vars)
	if err != nil {
		return false, err
	}
	return Truthy(v), nil
}

var progCache sync.Map // src -> *Program

// Cached compiles src with a global cache. It is the entry point the engine
// uses on hot paths.
func Cached(src string) (*Program, error) {
	if p, ok := progCache.Load(src); ok {
		return p.(*Program), nil
	}
	p, err := Compile(src)
	if err != nil {
		return nil, err
	}
	progCache.Store(src, p)
	return p, nil
}

// Eval is a convenience: compile (cached) + evaluate.
func Eval(src string, vars map[string]any) (any, error) {
	p, err := Cached(src)
	if err != nil {
		return nil, err
	}
	return p.Eval(vars)
}

// EvalBool is a convenience: compile (cached) + evaluate as boolean.
func EvalBool(src string, vars map[string]any) (bool, error) {
	p, err := Cached(src)
	if err != nil {
		return false, err
	}
	return p.EvalBool(vars)
}

// Truthy implements the language's boolean coercion: false, nil, 0, "",
// empty list/map are false; everything else is true.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case time.Time:
		return !x.IsZero()
	default:
		if f, ok := toFloat(v); ok {
			return f != 0
		}
		return true
	}
}

// Normalize converts arbitrary Go values into the expression value space.
// Numeric types become float64; other values pass through.
func Normalize(v any) any {
	switch x := v.(type) {
	case nil, bool, float64, string, time.Time, []any, map[string]any:
		return v
	default:
		if f, ok := toFloat(x); ok {
			return f
		}
		return v
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}

// ---- node evaluation -------------------------------------------------------

func (n *litNode) eval(*envFrame) (any, error) { return n.v, nil }

func (n *varNode) eval(e *envFrame) (any, error) {
	if v, ok := e.vars[n.name]; ok {
		return Normalize(v), nil
	}
	// Unknown variables evaluate to nil rather than erroring: conditions
	// like `approved == true` must not crash before `approved` is set.
	return nil, nil
}

func (n *listNode) eval(e *envFrame) (any, error) {
	out := make([]any, len(n.items))
	for i, it := range n.items {
		v, err := it.eval(e)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (n *unaryNode) eval(e *envFrame) (any, error) {
	v, err := n.x.eval(e)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "!":
		return !Truthy(v), nil
	case "-":
		f, ok := toFloat(v)
		if !ok {
			return nil, e.errf("cannot negate %T", v)
		}
		return -f, nil
	}
	return nil, e.errf("unknown unary operator %q", n.op)
}

func (n *ternaryNode) eval(e *envFrame) (any, error) {
	c, err := n.cond.eval(e)
	if err != nil {
		return nil, err
	}
	if Truthy(c) {
		return n.then.eval(e)
	}
	return n.els.eval(e)
}

func (n *memberNode) eval(e *envFrame) (any, error) {
	x, err := n.x.eval(e)
	if err != nil {
		return nil, err
	}
	switch m := x.(type) {
	case nil:
		return nil, nil // safe navigation: nil.a == nil
	case map[string]any:
		return Normalize(m[n.name]), nil
	case time.Time:
		return timeMember(m, n.name, e)
	}
	return nil, e.errf("cannot access property %q on %T", n.name, x)
}

func timeMember(t time.Time, name string, e *envFrame) (any, error) {
	switch name {
	case "year":
		return float64(t.Year()), nil
	case "month":
		return float64(t.Month()), nil
	case "day":
		return float64(t.Day()), nil
	case "hour":
		return float64(t.Hour()), nil
	case "minute":
		return float64(t.Minute()), nil
	case "second":
		return float64(t.Second()), nil
	case "weekday":
		return float64(t.Weekday()), nil
	case "unix":
		return float64(t.Unix()), nil
	}
	return nil, e.errf("unknown time property %q", name)
}

func (n *indexNode) eval(e *envFrame) (any, error) {
	x, err := n.x.eval(e)
	if err != nil {
		return nil, err
	}
	idx, err := n.idx.eval(e)
	if err != nil {
		return nil, err
	}
	switch c := x.(type) {
	case nil:
		return nil, nil
	case []any:
		f, ok := toFloat(idx)
		if !ok {
			return nil, e.errf("list index must be a number, got %T", idx)
		}
		i := int(f)
		if i < 0 {
			i += len(c)
		}
		if i < 0 || i >= len(c) {
			return nil, nil
		}
		return Normalize(c[i]), nil
	case map[string]any:
		k, ok := idx.(string)
		if !ok {
			return nil, e.errf("map key must be a string, got %T", idx)
		}
		return Normalize(c[k]), nil
	case string:
		f, ok := toFloat(idx)
		if !ok {
			return nil, e.errf("string index must be a number, got %T", idx)
		}
		i := int(f)
		if i < 0 {
			i += len(c)
		}
		if i < 0 || i >= len(c) {
			return nil, nil
		}
		return string(c[i]), nil
	}
	return nil, e.errf("cannot index %T", x)
}

func (n *callNode) eval(e *envFrame) (any, error) {
	fn := builtins[n.name]
	if e.funcs != nil {
		if f, ok := e.funcs[n.name]; ok {
			fn = f
		}
	}
	if fn == nil {
		return nil, e.errf("unknown function %q", n.name)
	}
	args := make([]any, len(n.args))
	for i, a := range n.args {
		v, err := a.eval(e)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	v, err := fn(args)
	if err != nil {
		return nil, e.errf("%s(): %v", n.name, err)
	}
	return Normalize(v), nil
}

func (n *binaryNode) eval(e *envFrame) (any, error) {
	// Short-circuit operators evaluate the right side lazily.
	switch n.op {
	case "&&":
		l, err := n.l.eval(e)
		if err != nil {
			return nil, err
		}
		if !Truthy(l) {
			return false, nil
		}
		r, err := n.r.eval(e)
		if err != nil {
			return nil, err
		}
		return Truthy(r), nil
	case "||":
		l, err := n.l.eval(e)
		if err != nil {
			return nil, err
		}
		if Truthy(l) {
			return true, nil
		}
		r, err := n.r.eval(e)
		if err != nil {
			return nil, err
		}
		return Truthy(r), nil
	case "??":
		l, err := n.l.eval(e)
		if err != nil {
			return nil, err
		}
		if l != nil {
			return l, nil
		}
		return n.r.eval(e)
	}

	l, err := n.l.eval(e)
	if err != nil {
		return nil, err
	}
	r, err := n.r.eval(e)
	if err != nil {
		return nil, err
	}

	switch n.op {
	case "==":
		return equal(l, r), nil
	case "!=":
		return !equal(l, r), nil
	case "<", "<=", ">", ">=":
		return compare(n.op, l, r, e)
	case "in":
		return contains(r, l, e)
	case "+":
		if ls, ok := l.(string); ok {
			return ls + Stringify(r), nil
		}
		if rs, ok := r.(string); ok {
			return Stringify(l) + rs, nil
		}
		if la, ok := l.([]any); ok {
			if ra, ok := r.([]any); ok {
				out := make([]any, 0, len(la)+len(ra))
				return append(append(out, la...), ra...), nil
			}
		}
		return arith(n.op, l, r, e)
	case "-", "*", "/", "%":
		return arith(n.op, l, r, e)
	}
	return nil, e.errf("unknown operator %q", n.op)
}

func arith(op string, l, r any, e *envFrame) (any, error) {
	lf, ok1 := toFloat(l)
	rf, ok2 := toFloat(r)
	if !ok1 || !ok2 {
		if lt, ok := l.(time.Time); ok && (op == "-") {
			if rt, ok := r.(time.Time); ok {
				return lt.Sub(rt).Seconds(), nil
			}
		}
		return nil, e.errf("operator %q needs numbers, got %T and %T", op, l, r)
	}
	switch op {
	case "+":
		return lf + rf, nil
	case "-":
		return lf - rf, nil
	case "*":
		return lf * rf, nil
	case "/":
		if rf == 0 {
			return nil, e.errf("division by zero")
		}
		return lf / rf, nil
	case "%":
		if rf == 0 {
			return nil, e.errf("division by zero")
		}
		return math.Mod(lf, rf), nil
	}
	return nil, e.errf("unknown arithmetic operator %q", op)
}

func compare(op string, l, r any, e *envFrame) (any, error) {
	if lf, ok := toFloat(l); ok {
		if rf, ok := toFloat(r); ok {
			switch op {
			case "<":
				return lf < rf, nil
			case "<=":
				return lf <= rf, nil
			case ">":
				return lf > rf, nil
			case ">=":
				return lf >= rf, nil
			}
		}
	}
	if ls, ok := l.(string); ok {
		if rs, ok := r.(string); ok {
			switch op {
			case "<":
				return ls < rs, nil
			case "<=":
				return ls <= rs, nil
			case ">":
				return ls > rs, nil
			case ">=":
				return ls >= rs, nil
			}
		}
	}
	if lt, ok := l.(time.Time); ok {
		if rt, ok := r.(time.Time); ok {
			switch op {
			case "<":
				return lt.Before(rt), nil
			case "<=":
				return !lt.After(rt), nil
			case ">":
				return lt.After(rt), nil
			case ">=":
				return !lt.Before(rt), nil
			}
		}
	}
	return nil, e.errf("cannot compare %T with %T", l, r)
}

func contains(container, item any, e *envFrame) (any, error) {
	switch c := container.(type) {
	case nil:
		return false, nil
	case []any:
		for _, v := range c {
			if equal(Normalize(v), item) {
				return true, nil
			}
		}
		return false, nil
	case map[string]any:
		k, ok := item.(string)
		if !ok {
			return false, nil
		}
		_, found := c[k]
		return found, nil
	case string:
		s, ok := item.(string)
		if !ok {
			return false, nil
		}
		return strings.Contains(c, s), nil
	}
	return nil, e.errf("'in' needs a list, map or string on the right, got %T", container)
}

func equal(l, r any) bool {
	l, r = Normalize(l), Normalize(r)
	if lf, ok := toFloat(l); ok {
		if rf, ok := toFloat(r); ok {
			return lf == rf
		}
	}
	switch lv := l.(type) {
	case nil:
		return r == nil
	case bool:
		rv, ok := r.(bool)
		return ok && lv == rv
	case string:
		rv, ok := r.(string)
		return ok && lv == rv
	case time.Time:
		rv, ok := r.(time.Time)
		return ok && lv.Equal(rv)
	case []any:
		rv, ok := r.([]any)
		if !ok || len(lv) != len(rv) {
			return false
		}
		for i := range lv {
			if !equal(lv[i], rv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		rv, ok := r.(map[string]any)
		if !ok || len(lv) != len(rv) {
			return false
		}
		for k, v := range lv {
			rvv, found := rv[k]
			if !found || !equal(v, rvv) {
				return false
			}
		}
		return true
	}
	return false
}

// Stringify renders a value the way the language's string conversion does:
// integers without decimals, RFC3339 for times, JSON-ish for collections.
func Stringify(v any) string {
	switch x := Normalize(v).(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case time.Time:
		return x.Format(time.RFC3339)
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, it := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(Stringify(it))
		}
		b.WriteByte(']')
		return b.String()
	default:
		return fmt.Sprintf("%v", x)
	}
}
