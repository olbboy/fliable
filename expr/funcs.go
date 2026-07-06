package expr

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// builtins is the standard function library. Every function is pure except
// now(), which reads the clock injected per evaluation via the "__now"
// variable when present (the engine injects it for deterministic replay)
// and falls back to the wall clock.
var builtins map[string]Func

func init() {
	builtins = map[string]Func{
		// --- general -----------------------------------------------------
		"len": func(args []any) (any, error) {
			if err := arity("len", args, 1); err != nil {
				return nil, err
			}
			switch v := args[0].(type) {
			case nil:
				return 0.0, nil
			case string:
				return float64(len(v)), nil
			case []any:
				return float64(len(v)), nil
			case map[string]any:
				return float64(len(v)), nil
			}
			return nil, fmt.Errorf("unsupported type %T", args[0])
		},
		"string": func(args []any) (any, error) {
			if err := arity("string", args, 1); err != nil {
				return nil, err
			}
			return Stringify(args[0]), nil
		},
		"number": func(args []any) (any, error) {
			if err := arity("number", args, 1); err != nil {
				return nil, err
			}
			if f, ok := toFloat(args[0]); ok {
				return f, nil
			}
			if s, ok := args[0].(string); ok {
				f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
				if err != nil {
					return nil, fmt.Errorf("cannot parse %q as number", s)
				}
				return f, nil
			}
			return nil, fmt.Errorf("cannot convert %T to number", args[0])
		},
		"bool": func(args []any) (any, error) {
			if err := arity("bool", args, 1); err != nil {
				return nil, err
			}
			return Truthy(args[0]), nil
		},

		// --- strings -----------------------------------------------------
		"upper":      strFn("upper", strings.ToUpper),
		"lower":      strFn("lower", strings.ToLower),
		"trim":       strFn("trim", strings.TrimSpace),
		"contains":   strFn2("contains", func(s, sub string) any { return strings.Contains(s, sub) }),
		"startsWith": strFn2("startsWith", func(s, p string) any { return strings.HasPrefix(s, p) }),
		"endsWith":   strFn2("endsWith", func(s, p string) any { return strings.HasSuffix(s, p) }),
		"split": strFn2("split", func(s, sep string) any {
			parts := strings.Split(s, sep)
			out := make([]any, len(parts))
			for i, p := range parts {
				out[i] = p
			}
			return out
		}),
		"replace": func(args []any) (any, error) {
			if err := arity("replace", args, 3); err != nil {
				return nil, err
			}
			s, ok1 := args[0].(string)
			old, ok2 := args[1].(string)
			nw, ok3 := args[2].(string)
			if !ok1 || !ok2 || !ok3 {
				return nil, fmt.Errorf("needs three strings")
			}
			return strings.ReplaceAll(s, old, nw), nil
		},
		"matches": func(args []any) (any, error) {
			if err := arity("matches", args, 2); err != nil {
				return nil, err
			}
			s, ok1 := args[0].(string)
			pat, ok2 := args[1].(string)
			if !ok1 || !ok2 {
				return nil, fmt.Errorf("needs two strings")
			}
			re, err := regexpCached(pat)
			if err != nil {
				return nil, err
			}
			return re.MatchString(s), nil
		},
		"join": func(args []any) (any, error) {
			if err := arity("join", args, 2); err != nil {
				return nil, err
			}
			list, ok1 := args[0].([]any)
			sep, ok2 := args[1].(string)
			if !ok1 || !ok2 {
				return nil, fmt.Errorf("needs a list and a string")
			}
			parts := make([]string, len(list))
			for i, v := range list {
				parts[i] = Stringify(v)
			}
			return strings.Join(parts, sep), nil
		},

		// --- math ----------------------------------------------------------
		"abs":   mathFn("abs", math.Abs),
		"floor": mathFn("floor", math.Floor),
		"ceil":  mathFn("ceil", math.Ceil),
		"round": mathFn("round", math.Round),
		"min":   varMathFn("min", math.Min),
		"max":   varMathFn("max", math.Max),
		"sum": func(args []any) (any, error) {
			list, err := listArg("sum", args)
			if err != nil {
				return nil, err
			}
			total := 0.0
			for _, v := range list {
				f, ok := toFloat(Normalize(v))
				if !ok {
					return nil, fmt.Errorf("non-numeric element %T", v)
				}
				total += f
			}
			return total, nil
		},
		"avg": func(args []any) (any, error) {
			list, err := listArg("avg", args)
			if err != nil {
				return nil, err
			}
			if len(list) == 0 {
				return nil, fmt.Errorf("empty list")
			}
			total := 0.0
			for _, v := range list {
				f, ok := toFloat(Normalize(v))
				if !ok {
					return nil, fmt.Errorf("non-numeric element %T", v)
				}
				total += f
			}
			return total / float64(len(list)), nil
		},

		// --- lists ---------------------------------------------------------
		"first": func(args []any) (any, error) {
			list, err := listArg("first", args)
			if err != nil {
				return nil, err
			}
			if len(list) == 0 {
				return nil, nil
			}
			return Normalize(list[0]), nil
		},
		"last": func(args []any) (any, error) {
			list, err := listArg("last", args)
			if err != nil {
				return nil, err
			}
			if len(list) == 0 {
				return nil, nil
			}
			return Normalize(list[len(list)-1]), nil
		},
		"sort": func(args []any) (any, error) {
			list, err := listArg("sort", args)
			if err != nil {
				return nil, err
			}
			out := make([]any, len(list))
			copy(out, list)
			var sortErr error
			sort.SliceStable(out, func(i, j int) bool {
				fi, oki := toFloat(Normalize(out[i]))
				fj, okj := toFloat(Normalize(out[j]))
				if oki && okj {
					return fi < fj
				}
				si, oki := Normalize(out[i]).(string)
				sj, okj := Normalize(out[j]).(string)
				if oki && okj {
					return si < sj
				}
				sortErr = fmt.Errorf("cannot sort mixed types")
				return false
			})
			if sortErr != nil {
				return nil, sortErr
			}
			return out, nil
		},
		"keys": func(args []any) (any, error) {
			if err := arity("keys", args, 1); err != nil {
				return nil, err
			}
			m, ok := args[0].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("needs a map, got %T", args[0])
			}
			ks := make([]string, 0, len(m))
			for k := range m {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			out := make([]any, len(ks))
			for i, k := range ks {
				out[i] = k
			}
			return out, nil
		},
		"range": func(args []any) (any, error) {
			if err := arity("range", args, 2); err != nil {
				return nil, err
			}
			from, ok1 := toFloat(args[0])
			to, ok2 := toFloat(args[1])
			if !ok1 || !ok2 {
				return nil, fmt.Errorf("needs two numbers")
			}
			if to < from {
				return []any{}, nil
			}
			if to-from > 1e6 {
				return nil, fmt.Errorf("range too large")
			}
			out := make([]any, 0, int(to-from)+1)
			for i := from; i <= to; i++ {
				out = append(out, i)
			}
			return out, nil
		},

		// --- time ------------------------------------------------------------
		"now": func(args []any) (any, error) {
			if err := arity("now", args, 0); err != nil {
				return nil, err
			}
			return time.Now().UTC(), nil
		},
		"date": func(args []any) (any, error) {
			if err := arity("date", args, 1); err != nil {
				return nil, err
			}
			s, ok := args[0].(string)
			if !ok {
				return nil, fmt.Errorf("needs a string")
			}
			return ParseTime(s)
		},
		"duration": func(args []any) (any, error) {
			if err := arity("duration", args, 1); err != nil {
				return nil, err
			}
			s, ok := args[0].(string)
			if !ok {
				return nil, fmt.Errorf("needs an ISO-8601 string")
			}
			d, err := ParseISODuration(s)
			if err != nil {
				return nil, err
			}
			return d.Seconds(), nil
		},
		"addDuration": func(args []any) (any, error) {
			if err := arity("addDuration", args, 2); err != nil {
				return nil, err
			}
			t, ok := args[0].(time.Time)
			if !ok {
				return nil, fmt.Errorf("first argument must be a time")
			}
			s, ok := args[1].(string)
			if !ok {
				return nil, fmt.Errorf("second argument must be an ISO-8601 duration")
			}
			d, err := ParseISODuration(s)
			if err != nil {
				return nil, err
			}
			return t.Add(d), nil
		},
	}
}

// ParseTime parses RFC3339, date-only and datetime-without-zone formats.
func ParseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q", s)
}

func arity(name string, args []any, want int) error {
	if len(args) != want {
		return fmt.Errorf("takes %d argument(s), got %d", want, len(args))
	}
	return nil
}

func listArg(name string, args []any) ([]any, error) {
	if err := arity(name, args, 1); err != nil {
		return nil, err
	}
	list, ok := args[0].([]any)
	if !ok {
		if args[0] == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("needs a list, got %T", args[0])
	}
	return list, nil
}

func strFn(name string, f func(string) string) Func {
	return func(args []any) (any, error) {
		if err := arity(name, args, 1); err != nil {
			return nil, err
		}
		s, ok := args[0].(string)
		if !ok {
			return nil, fmt.Errorf("needs a string, got %T", args[0])
		}
		return f(s), nil
	}
}

func strFn2(name string, f func(a, b string) any) Func {
	return func(args []any) (any, error) {
		if err := arity(name, args, 2); err != nil {
			return nil, err
		}
		a, ok1 := args[0].(string)
		b, ok2 := args[1].(string)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("needs two strings")
		}
		return f(a, b), nil
	}
}

func mathFn(name string, f func(float64) float64) Func {
	return func(args []any) (any, error) {
		if err := arity(name, args, 1); err != nil {
			return nil, err
		}
		x, ok := toFloat(args[0])
		if !ok {
			return nil, fmt.Errorf("needs a number, got %T", args[0])
		}
		return f(x), nil
	}
}

func varMathFn(name string, f func(a, b float64) float64) Func {
	return func(args []any) (any, error) {
		vals := args
		if len(args) == 1 {
			if list, ok := args[0].([]any); ok {
				vals = list
			}
		}
		if len(vals) == 0 {
			return nil, fmt.Errorf("needs at least one number")
		}
		acc, ok := toFloat(Normalize(vals[0]))
		if !ok {
			return nil, fmt.Errorf("non-numeric argument %T", vals[0])
		}
		for _, v := range vals[1:] {
			x, ok := toFloat(Normalize(v))
			if !ok {
				return nil, fmt.Errorf("non-numeric argument %T", v)
			}
			acc = f(acc, x)
		}
		return acc, nil
	}
}

var reCache sync.Map // pattern -> *regexp.Regexp

func regexpCached(pat string) (*regexp.Regexp, error) {
	if re, ok := reCache.Load(pat); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("bad pattern: %v", err)
	}
	reCache.Store(pat, re)
	return re, nil
}
