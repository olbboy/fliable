# The Fliable expression language

One language everywhere: sequence flow conditions, script tasks, service
task expressions, gateway conditions, timer values, IO mappings,
multi-instance collections and DMN cells.

## Design

- **Sandboxed.** Expressions read the variables they are given and call
  registered pure functions. No reflection, no method calls, no I/O, no
  engine mutation. `exec('rm -rf /')` is a compile-time unknown-function
  error, not a security incident.
- **Deterministic.** Same input, same output.
- **Fast.** Compiled to an AST once, cached globally, evaluated with
  zero parsing on the hot path.

## Values

`nil`, booleans, numbers (float64; all Go numeric types normalized),
strings, lists (`[]any`), maps (`map[string]any`), `time.Time`.

## Syntax

```
# literals
42   3.14   'single' or "double" quotes   true   false   null   [1, 2, 3]

# arithmetic & comparison
a + b * 2      10 % 3      -x
amount >= 1000      status != 'closed'

# logic (both spellings)
a && b || !c        a and b or not c

# membership & strings
5 in [1, 2, 5]      'pen' in status      "Hello, " + name

# access — safe navigation: nil.x == nil, out-of-range == nil
order.customer.name      items[0].qty      items[-1]      m["key"]

# ternary & null coalescing
amount > 500 ? 'big' : 'small'      missing ?? 'fallback'

# time
now()      date('2026-01-02')      addDuration(now(), 'P2D')
deadline - created        # seconds
deadline.year   .month   .day   .hour   .minute   .weekday   .unix
```

Unknown variables evaluate to `nil` (so `approved == true` is simply
false before `approved` exists), and truthiness follows JSON intuition:
`false`, `nil`, `0`, `""`, empty list/map are false.

## Builtin functions

**General** `len` `string` `number` `bool` ·
**Strings** `upper` `lower` `trim` `contains` `startsWith` `endsWith`
`split` `join` `replace` `matches` ·
**Math** `abs` `floor` `ceil` `round` `min` `max` `sum` `avg` ·
**Lists/maps** `first` `last` `sort` `keys` `range` ·
**Time** `now` `date` `duration` `addDuration`

## Extending

```go
p, _ := expr.Compile("double(21)")
v, _ := p.EvalWith(vars, map[string]expr.Func{
    "double": func(args []any) (any, error) { return args[0].(float64) * 2, nil },
})
```

## Model field conventions

Model attributes like `fliable:assignee` are **literal by default**;
mark them as expressions with a `${...}` / `#{...}` wrapper (Flowable
style) or a leading `=` (Zeebe style): `fliable:assignee="${manager}"`.
Dedicated expression slots (flow conditions, scripts, collections,
timers) take bare expressions, with `${...}` wrappers tolerated for
migrated models.
