package expr

import (
	"strings"
	"testing"
	"time"
)

func evalT(t *testing.T, src string, vars map[string]any) any {
	t.Helper()
	v, err := Eval(src, vars)
	if err != nil {
		t.Fatalf("Eval(%q): %v", src, err)
	}
	return v
}

func TestArithmeticAndPrecedence(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{"1 + 2 * 3", 7.0},
		{"(1 + 2) * 3", 9.0},
		{"10 / 4", 2.5},
		{"10 % 3", 1.0},
		{"-5 + 3", -2.0},
		{"2 * -3", -6.0},
		{"1.5 + 2.25", 3.75},
	}
	for _, tc := range cases {
		if got := evalT(t, tc.src, nil); got != tc.want {
			t.Errorf("%q = %v, want %v", tc.src, got, tc.want)
		}
	}
}

func TestComparisonAndLogic(t *testing.T) {
	vars := map[string]any{"amount": 250, "status": "open", "vip": true}
	cases := []struct {
		src  string
		want bool
	}{
		{"amount > 100", true},
		{"amount >= 250", true},
		{"amount < 100", false},
		{"status == 'open'", true},
		{"status != 'closed'", true},
		{"amount > 100 && status == 'open'", true},
		{"amount > 1000 || vip", true},
		{"!(amount > 1000)", true},
		{"amount > 100 and status == 'open'", true},
		{"amount > 1000 or vip", true},
		{"not (amount > 1000)", true},
		{"'pen' in status", true},
		{"5 in [1, 2, 5]", true},
		{"7 in [1, 2, 5]", false},
		{"amount > 200 ? true : false", true},
	}
	for _, tc := range cases {
		got, err := EvalBool(tc.src, vars)
		if err != nil {
			t.Fatalf("EvalBool(%q): %v", tc.src, err)
		}
		if got != tc.want {
			t.Errorf("%q = %v, want %v", tc.src, got, tc.want)
		}
	}
}

func TestUnknownVariablesAreNil(t *testing.T) {
	if got := evalT(t, "missing", nil); got != nil {
		t.Errorf("missing var = %v, want nil", got)
	}
	got, err := EvalBool("approved == true", map[string]any{})
	if err != nil || got {
		t.Errorf("approved == true on empty vars = %v, %v", got, err)
	}
	// Safe navigation through nil.
	if got := evalT(t, "customer.address.city", nil); got != nil {
		t.Errorf("nil navigation = %v", got)
	}
}

func TestMemberAndIndexAccess(t *testing.T) {
	vars := map[string]any{
		"order": map[string]any{
			"customer": map[string]any{"name": "Ada", "tier": "gold"},
			"items":    []any{map[string]any{"sku": "A1", "qty": 2}, map[string]any{"sku": "B2", "qty": 1}},
		},
	}
	if got := evalT(t, "order.customer.name", vars); got != "Ada" {
		t.Errorf("member = %v", got)
	}
	if got := evalT(t, "order.items[0].qty", vars); got != 2.0 {
		t.Errorf("index+member = %v", got)
	}
	if got := evalT(t, "order.items[-1].sku", vars); got != "B2" {
		t.Errorf("negative index = %v", got)
	}
	if got := evalT(t, `order["customer"]["tier"]`, vars); got != "gold" {
		t.Errorf("map index = %v", got)
	}
	if got := evalT(t, "order.items[99]", vars); got != nil {
		t.Errorf("out of range = %v, want nil", got)
	}
}

func TestStringOps(t *testing.T) {
	vars := map[string]any{"name": "World", "n": 3}
	if got := evalT(t, `"Hello, " + name + "!"`, vars); got != "Hello, World!" {
		t.Errorf("concat = %v", got)
	}
	if got := evalT(t, `"attempt " + n`, vars); got != "attempt 3" {
		t.Errorf("number concat = %v", got)
	}
	if got := evalT(t, `upper("go")`, nil); got != "GO" {
		t.Errorf("upper = %v", got)
	}
	if got := evalT(t, `matches("ORD-123", "^ORD-[0-9]+$")`, nil); got != true {
		t.Errorf("matches = %v", got)
	}
	if got := evalT(t, `join(split("a,b,c", ","), "-")`, nil); got != "a-b-c" {
		t.Errorf("split/join = %v", got)
	}
}

func TestBuiltinFunctions(t *testing.T) {
	vars := map[string]any{"xs": []any{3, 1, 2}}
	if got := evalT(t, "len(xs)", vars); got != 3.0 {
		t.Errorf("len = %v", got)
	}
	if got := evalT(t, "sum(xs)", vars); got != 6.0 {
		t.Errorf("sum = %v", got)
	}
	if got := evalT(t, "avg(xs)", vars); got != 2.0 {
		t.Errorf("avg = %v", got)
	}
	if got := evalT(t, "min(xs)", vars); got != 1.0 {
		t.Errorf("min = %v", got)
	}
	if got := evalT(t, "max(5, 9, 2)", nil); got != 9.0 {
		t.Errorf("max = %v", got)
	}
	if got := evalT(t, "first(sort(xs))", vars); got != 1.0 {
		t.Errorf("first(sort) = %v", got)
	}
	if got := evalT(t, "number('42') + 1", nil); got != 43.0 {
		t.Errorf("number = %v", got)
	}
	if got := evalT(t, "len(range(1, 5))", nil); got != 5.0 {
		t.Errorf("range = %v", got)
	}
}

func TestNullCoalescing(t *testing.T) {
	if got := evalT(t, "missing ?? 'fallback'", nil); got != "fallback" {
		t.Errorf("?? = %v", got)
	}
	if got := evalT(t, "x ?? 10", map[string]any{"x": 5}); got != 5.0 {
		t.Errorf("?? with value = %v", got)
	}
}

func TestTimeSupport(t *testing.T) {
	vars := map[string]any{
		"deadline": time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		"created":  time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC),
	}
	got, err := EvalBool("deadline > created", vars)
	if err != nil || !got {
		t.Errorf("time compare = %v, %v", got, err)
	}
	if got := evalT(t, "deadline - created", vars); got != 86400.0 {
		t.Errorf("time diff = %v", got)
	}
	if got := evalT(t, "deadline.year", vars); got != 2026.0 {
		t.Errorf("time member = %v", got)
	}
	if got := evalT(t, `date("2026-01-02").day`, nil); got != 2.0 {
		t.Errorf("date() = %v", got)
	}
	if got := evalT(t, `addDuration(created, "P1DT2H").hour`, vars); got != 14.0 {
		t.Errorf("addDuration = %v", got)
	}
	if got := evalT(t, `duration("PT90S")`, nil); got != 90.0 {
		t.Errorf("duration = %v", got)
	}
}

func TestSandboxing(t *testing.T) {
	// Unknown functions must error, not panic or reach into the runtime.
	if _, err := Eval("exec('rm -rf /')", nil); err == nil || !strings.Contains(err.Error(), "unknown function") {
		t.Errorf("expected unknown function error, got %v", err)
	}
	// Custom functions are available only when explicitly registered.
	p, err := Compile("double(21)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Eval(nil); err == nil {
		t.Error("double() should be unknown without registration")
	}
	v, err := p.EvalWith(nil, map[string]Func{
		"double": func(args []any) (any, error) {
			f, _ := toFloat(args[0])
			return f * 2, nil
		},
	})
	if err != nil || v != 42.0 {
		t.Errorf("custom func = %v, %v", v, err)
	}
}

func TestErrors(t *testing.T) {
	for _, src := range []string{"1 +", "(1", "foo(", "a.[b]", "'unterminated", "1 @ 2", "[1,"} {
		if _, err := Compile(src); err == nil {
			t.Errorf("Compile(%q) should fail", src)
		}
	}
	for _, src := range []string{"1 / 0", "10 % 0", "'a' - 1"} {
		if _, err := Eval(src, nil); err == nil {
			t.Errorf("Eval(%q) should fail", src)
		}
	}
}

func TestISODuration(t *testing.T) {
	cases := []struct {
		src  string
		want time.Duration
	}{
		{"PT5M", 5 * time.Minute},
		{"PT1H30M", 90 * time.Minute},
		{"P1D", 24 * time.Hour},
		{"P2W", 14 * 24 * time.Hour},
		{"P1DT12H", 36 * time.Hour},
		{"PT0.5S", 500 * time.Millisecond},
		{"-PT10S", -10 * time.Second},
		{"P1M", 30 * 24 * time.Hour},
	}
	for _, tc := range cases {
		got, err := ParseISODuration(tc.src)
		if err != nil {
			t.Errorf("ParseISODuration(%q): %v", tc.src, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseISODuration(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
	for _, bad := range []string{"", "P", "PT", "5M", "PT5X", "R3/PT5M"} {
		if _, err := ParseISODuration(bad); err == nil {
			t.Errorf("ParseISODuration(%q) should fail", bad)
		}
	}

	reps, interval, err := ParseTimerCycle("R3/PT10S")
	if err != nil || reps != 3 || interval != 10*time.Second {
		t.Errorf("ParseTimerCycle = %d, %v, %v", reps, interval, err)
	}
	reps, _, err = ParseTimerCycle("R/PT1H")
	if err != nil || reps != -1 {
		t.Errorf("unbounded cycle = %d, %v", reps, err)
	}
}

func TestStringify(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{3.0, "3"},
		{3.5, "3.5"},
		{nil, ""},
		{true, "true"},
		{[]any{1, "a"}, "[1, a]"},
	}
	for _, tc := range cases {
		if got := Stringify(tc.in); got != tc.want {
			t.Errorf("Stringify(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func BenchmarkEvalCondition(b *testing.B) {
	vars := map[string]any{"amount": 250.0, "status": "open"}
	p, err := Compile("amount > 100 && status == 'open'")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := p.Eval(vars); err != nil {
			b.Fatal(err)
		}
	}
}
