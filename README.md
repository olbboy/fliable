# Fliable

**The compact and highly efficient workflow and Business Process Management (BPM) platform.**

Fliable executes BPMN 2.0 processes and DMN decision tables from a **single ~10 MB Go binary** — no JVM, no application server, no database required. Embed it as a Go library or run it as a service with a complete REST API, durable crash-safe storage, live event streaming and Prometheus metrics built in.

```
$ fliable serve --data ./data --deploy ./processes
INFO storage: durable journal dir=./data
INFO deployed process key=orderFulfillment version=1
INFO fliable ready addr=:8080 startup=2ms
```

Two milliseconds from `exec` to serving traffic — including crash recovery of every in-flight process instance.

## Why Fliable instead of a Java BPM platform?

Fliable was designed feature-by-feature to beat the classic Java engines (Flowable, Camunda, Activiti) on the dimensions that matter in modern deployments:

| | **Fliable** | Java BPM platforms (Flowable et al.) |
|---|---|---|
| Deployment | one static binary, ~10 MB | JVM + WAR/Spring Boot, hundreds of MB |
| Startup (with recovery) | **~2 ms** | 10–30 s |
| Memory floor | ~15 MB RSS | 512 MB–2 GB heap |
| Persistence | built-in crash-safe journal + snapshots, zero config; pluggable `Store` interface for SQL/anything | external RDBMS + schema migrations required |
| Concurrency | goroutines + striped per-instance locks, no DB row-lock contention | thread pools serialized through database locking |
| Expressions | sandboxed, deterministic language — no reflection, no method calls, no I/O, compiled & cached | JUEL/scripting engines with a history of sandbox escapes |
| Service tasks | type-safe Go handlers **and** Zeebe-style external workers over REST — both built in | Java delegates in-JVM; external tasks bolted on |
| Failure handling | retries with exponential backoff → **incidents** (never silent), one-call resolve & resume | dead-letter jobs buried in DB tables |
| History | event-sourced stream, queryable **and** live via server-sent events | history tables, polling |
| Decisions (DMN) | 5 hit policies + aggregations, same sandboxed expressions | separate engine module |
| Model migration | reads Flowable/Camunda/Activiti extension attributes and `${...}` expressions as-is | — |
| Validation | aggregate: every model problem reported in one pass | fail-fast, one error at a time |
| Dependencies | **zero** (Go standard library only) | large dependency trees |

The trade-off Fliable makes: it is a **single-node embedded-first engine** (like SQLite is to databases). The `Store` interface is the seam for teams that need SQL-backed or replicated deployments.

## Quick start — server

```bash
go install github.com/olbboy/fliable/cmd/fliable@latest

fliable serve --addr :8080 --data ./fliable-data --deploy ./processes
```

Drive it with any HTTP client:

```bash
# Deploy a process (bare BPMN XML body)
curl -X POST localhost:8080/v1/definitions --data-binary @order.bpmn

# Start an instance
curl -X POST localhost:8080/v1/instances \
  -d '{"definitionKey":"orderFulfillment","businessKey":"ord-1","variables":{"amount":900}}'

# Work the task queue
curl "localhost:8080/v1/tasks?candidateGroup=fraud-team"
curl -X POST localhost:8080/v1/tasks/{id}/complete -d '{"user":"alice","variables":{"approved":true}}'

# Correlate a message
curl -X POST localhost:8080/v1/messages -d '{"name":"paymentReceived","correlationKey":"ord-1"}'

# External workers (Zeebe-style job polling)
curl -X POST localhost:8080/v1/external-tasks/fetch -d '{"topic":"shipping","workerId":"w1"}'
curl -X POST localhost:8080/v1/external-tasks/{id}/complete -d '{"workerId":"w1","variables":{"trackingId":"T1"}}'

# Watch everything live (server-sent events)
curl -N "localhost:8080/v1/events?type=task."
```

## Quick start — embedded in Go

```go
eng := engine.New(store.NewMemory()) // or store.OpenJournal("./data", ...) for durability

eng.RegisterHandler("accounts.create", func(ctx engine.Context) (map[string]any, error) {
    return map[string]any{"email": createAccount(ctx.Variables["name"])}, nil
})

eng.Deploy(bpmnXML, "")
inst, _ := eng.StartInstance("onboarding", "emp-42", map[string]any{"name": "dat"})

tasks, _ := eng.ListTasks(store.TaskFilter{CandidateGroup: "managers"})
eng.CompleteTask(tasks[0].ID, map[string]any{"buddy": "alice"}, "bob")
```

Run the full example: `go run ./examples/embedded`

## BPMN 2.0 coverage

**Tasks** user, service (Go handler / expression / external topic), script, business rule (DMN), send, receive, manual · **Gateways** exclusive, parallel, inclusive, event-based · **Events** none/timer/message/signal starts, intermediate catch & throw (timer/message/signal), interrupting & non-interrupting boundary events (timer/message/signal/error), error & terminate & message/signal end events · **Scopes** embedded sub-processes, event sub-processes, call activities with variable mappings · **Multi-instance** parallel & sequential, collections, cardinality, completion conditions, output collections · **Plus** async continuations, retry policies, ISO-8601 timers with cycles (`R3/PT10M`), input/output variable mappings.

Details and semantics: [docs/bpmn-coverage.md](docs/bpmn-coverage.md)

## The expression language

Conditions, scripts, mappings and DMN cells use one sandboxed language:

```
amount > 1000 && customer.tier == 'gold'
order.items[0].qty * unitPrice
approvers[loopCounter]
missing ?? 'fallback'
addDuration(now(), 'P2D') > deadline
```

Deterministic, compiled once and cached, JSON value model, safe navigation, ~40 pure builtin functions — and structurally incapable of I/O, reflection or engine mutation. [docs/expressions.md](docs/expressions.md)

## Decisions (DMN)

Decision tables with `UNIQUE`, `FIRST`, `ANY`, `PRIORITY` and `COLLECT` (+ SUM/MIN/MAX/COUNT) hit policies, FEEL-style unary tests (`< 100`, `[10..20)`, `"gold","silver"`, `not(...)`, `? > limit * 2`), loadable from DMN XML or built programmatically. Wire into processes with `businessRuleTask` or call directly:

```go
reg := dmn.NewRegistry()
reg.RegisterXML(dmnXML)
eng := engine.New(st, engine.WithDecisionEvaluator(reg))
```

## Durability without a database

`store.OpenJournal` gives you crash-safe persistence from the binary alone: every mutation appends to a JSON write-ahead journal, compacted periodically into an atomic snapshot. Torn final writes from a crash are detected and dropped; recovery is snapshot + replay. `kill -9` tested. Need Postgres or multi-node? Implement the `store.Store` interface — the engine is storage-agnostic.

## Observability

- `GET /metrics` — Prometheus counters for instances, tasks, jobs, timers, incidents
- `GET /v1/events` — the entire engine event stream over SSE, filterable by instance and type
- `GET /v1/instances/{id}/history` — complete event-sourced audit trail per instance
- `engine.OnEvent(...)` — the same stream in-process
- structured logging via `log/slog`

## Failures are never silent

A failing service task retries with exponential backoff; exhausted retries raise an **incident** that parks the token, keeps the instance alive and shows up in `GET /v1/incidents`. Fix the cause, `POST /v1/incidents/{id}/resolve`, and execution resumes exactly where it stopped. Unhandled BPMN errors terminate the instance *with* an incident record — nothing disappears.

## Performance

`go test ./engine/ -bench .` on a 4-core cloud VM (Intel Xeon 2.8 GHz):

```
BenchmarkProcessExecution-4    ~8,000 full instance lifecycles/sec/core
```

One "lifecycle" is the complete journey — start event, script task with expression evaluation, exclusive gateway, service task handler, end event, ~10 event-sourced history writes — through the in-memory store. There is no database round-trip and no lock-contention cliff: instances are serialized only against themselves.

## Repository layout

```
bpmn/     BPMN 2.0 model + XML parser + validation
expr/     sandboxed expression language
store/    persistence: interface, memory store, durable journal store
engine/   the token-based process engine
dmn/      decision tables
rest/     HTTP API + SSE + metrics
cmd/      the fliable binary
examples/ runnable examples and sample models
docs/     architecture & reference documentation
```

## License

Apache License 2.0 — same license as Flowable, so evaluation and migration are friction-free.
