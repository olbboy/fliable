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
| Persistence | built-in crash-safe journal + snapshots, zero config; **SQL store included** (Postgres/MySQL/SQLite via `database/sql`, driver injected) | external RDBMS + schema migrations required |
| Disaster recovery | **hot-standby journal replication built in** — snapshot bootstrap, auto-resync, ~2 ms promotion | database-level tooling only |
| Live migration | **activity-mapped migration of running instances with dry-run validation** | chronic pain point |
| Concurrency | goroutines + striped per-instance locks, no DB row-lock contention | thread pools serialized through database locking |
| Expressions | sandboxed, deterministic language — no reflection, no method calls, no I/O, compiled & cached | JUEL/scripting engines with a history of sandbox escapes |
| Service tasks | type-safe Go handlers **and** Zeebe-style external workers over REST — both built in | Java delegates in-JVM; external tasks bolted on |
| Failure handling | retries with exponential backoff → **incidents** (never silent), one-call resolve & resume | dead-letter jobs buried in DB tables |
| History | event-sourced stream, queryable **and** live via server-sent events | history tables, polling |
| Decisions (DMN) | 5 hit policies + aggregations, same sandboxed expressions | separate engine module |
| Model migration | reads Flowable/Camunda/Activiti extension attributes and `${...}` expressions as-is | — |
| AI agents | agent task is **just another token** — provider-agnostic, MCP-native, governed by the same event log; in-process or external AI worker | Spring AI, vendored providers, bolted on |
| UI | headless-first: typed TypeScript SDK + React hooks (no markup) bind to any framework — shadcn, Base UI, Vue, Svelte | web apps you adopt wholesale |
| Multi-tenancy & auth | tenant isolation, pluggable auth chain (API key / bearer / signed token / **OIDC+JWKS**), per-route RBAC, CORS, **encrypted secrets vault** — built in | add-on / commercial |
| Observability | Prometheus + SSE + **zero-dep OpenTelemetry trace export** | metrics, weaker tracing |
| Validation | aggregate: every model problem reported in one pass | fail-fast, one error at a time |
| Dependencies | **zero** (Go standard library only) | large dependency trees |

The trade-off Fliable makes: execution is **single-node embedded-first** (like SQLite is to databases) — but the storage story scales past that: a built-in SQL store shares one database across replicas, and built-in hot-standby replication gives DR with ~2 ms promotion. Active-active clustering is the one thing deliberately left out.

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

## Durability — journal or SQL, your choice

`store.OpenJournal` gives you crash-safe persistence from the binary alone: every mutation appends to a JSON write-ahead journal, compacted periodically into an atomic snapshot. Torn final writes from a crash are detected and dropped; recovery is snapshot + replay. `kill -9` tested.

Prefer a database? `store.NewSQL(db, ...)` runs the same engine on **Postgres, MySQL or SQLite** through `database/sql` — you import the driver, Fliable's `go.mod` stays empty. Optimistic single-row claims mean several engine replicas can share one database.

```go
db, _ := sql.Open("pgx", dsn) // driver lives in YOUR go.mod
st, _ := store.NewSQL(db, store.SQLOptions{Dialect: store.DialectPostgres})
eng := engine.New(st)
```

## Disaster recovery — warm standby built in

A leader journal streams every entry to a standby over HTTP (`replicate` package or `--replicate-to` / `--standby` flags): snapshot bootstrap, ordered streaming, automatic full resync after any hiccup. Promotion is starting an engine on the follower's store — Fliable's normal ~2 ms boot. RPO is the in-flight batch; no external replication stack.

## Live instance migration

Deployed v2 of a process while a thousand v1 instances sleep on user tasks and timers? `POST /v1/instances/{id}/migrate` (or `eng.MigrateInstance`) moves them: element IDs map through an activity map, variables can be derived with expressions, and the plan **validates completely before anything is written** — with `dryRun` for a safe preflight. Open tasks, timers and message/signal subscriptions carry over; renamed messages re-correlate against the new model.

## Forms — schema in, your components out

JSON form definitions (typed fields, required/min/max/pattern rules, expression-driven conditional visibility, defaults) bind to user tasks by `formKey`. The engine rejects invalid submissions with a 422 **before** any state changes; `GET /v1/tasks/{id}/form` hands any client the schema plus prefill variables. Rendering stays yours — the same headless philosophy as the SDK.

## Webhook event channels

`PUT /v1/webhooks/{name}` maps inbound HTTP events onto messages or signals: per-channel secrets (external systems never hold platform credentials), payload-expression correlation and variable mapping, and idempotency via header or a configured dedupe expression. A process with no user task is an automation — webhook channels make Fliable an n8n-style automation engine on BPMN semantics.

## Observability

- `GET /metrics` — Prometheus counters for instances, tasks, jobs, timers, incidents
- **OpenTelemetry** — `--otel http://collector:4318` exports a span per instance and per executed element (OTLP/HTTP JSON, no SDK dependency), parented under the caller's W3C `traceparent`
- `GET /v1/analytics/processes` — cycle times (avg/p50/p95), state counts, open incidents/tasks and 24h throughput per definition
- `GET /v1/events` — the entire engine event stream over SSE, filterable by instance and type
- `GET /v1/instances/{id}/history` — complete event-sourced audit trail per instance
- `engine.OnEvent(...)` — the same stream in-process
- structured logging via `log/slog`

## AI agents as first-class tasks

An **agent task** runs an LLM (with tools) as an ordinary step in a process
or case — same token model, same retries, same incident handling as any
other task. The engine stays **provider-agnostic and zero-dependency**: no
model SDK lives in the core. You supply the intelligence two ways:

- **In-process** — implement the `AgentInvoker` interface and register it.
- **External AI worker** — poll `/v1/agent-jobs` (fetch-and-lock, like
  external tasks) from a separate binary that calls Claude, GPT, or any
  MCP toolchain, then complete the job over REST.

Every invocation is **governed by the event log**: `agent.invoked` and
`agent.completed` history events capture the prompt, tool calls and token
usage, so decisions are auditable and reproducible. A **human-in-the-loop**
approval gate can park an agent's proposal as a user task and release it
only when an `approved` variable is set. Tool descriptors are **MCP-native**,
so the same agent works against any Model Context Protocol server.

```go
eng := engine.New(st, engine.WithDefaultAgent(myInvoker)) // or leave agents to external workers
```

## Multi-tenancy, auth & security

- **Tenancy** — every record carries a `TenantID`; definitions version per
  `(tenant, key)`; queries and the REST layer confine callers to their tenant.
- **AuthN** — a pluggable `Authenticator` chain: API keys, static bearer
  tokens, HMAC-SHA256 signed tokens (`MintToken`/`ParseToken`), or **OIDC**
  — RS256/ES256 JWTs verified against your IdP's JWKS (Keycloak, Auth0,
  Entra ID, Okta) with role mapping and tenant claims, stdlib crypto only.
- **Secrets** — a built-in vault: AES-256-GCM encrypted at rest, master key
  from the environment, values reachable from service handlers via
  `ctx.Secret(name)` — never through variables, expressions or history.
- **AuthZ** — per-route RBAC across `viewer` / `operator` / `admin`.
- **CORS** — configurable middleware for browser clients.
- **Retention** — history TTL housekeeping and `PurgeInstance` for
  GDPR-style deletes.
- **Tracing** — W3C `traceparent` is ingested and propagated across
  external and agent workers.

## Query API

Instances and tasks accept variable predicates, time windows, keyset
(cursor) pagination and sort direction — no external index required:

```bash
curl "localhost:8080/v1/tasks?candidateGroup=fraud-team&var=amount:gte:500&limit=20"
curl "localhost:8080/v1/instances?state=active&cursor=<nextCursor>&desc=true"
```

Responses are paged envelopes (`{ items, nextCursor }`). The full endpoint
list is served as an **OpenAPI 3.1** document at `/openapi.json` with a
dependency-free reference at `/docs`.

## Any UI framework — headless TypeScript SDK

Fliable ships no opinion about your frontend. The
[`@fliable/sdk`](packages/sdk-ts) package is a **framework-agnostic** typed
client plus optional **headless React hooks** that return *data and handlers
only, no markup* — so shadcn/ui, Base UI, Radix, Vue, Svelte and Solid all
bind to the same data layer.

```tsx
import { useTasks, useCompleteTask } from "@fliable/sdk/react";
// bring your own <Button/> and <Card/> — the hook holds no markup
const { data } = useTasks({ candidateGroup: "sales" }, { live: true });
```

See [packages/sdk-ts/README.md](packages/sdk-ts/README.md) for shadcn/ui and
Base UI examples.

## Failures are never silent

A failing service task retries with exponential backoff; exhausted retries raise an **incident** that parks the token, keeps the instance alive and shows up in `GET /v1/incidents`. Fix the cause, `POST /v1/incidents/{id}/resolve`, and execution resumes exactly where it stopped. Unhandled BPMN errors terminate the instance *with* an incident record — nothing disappears.

## Performance

`go test ./engine/ -bench .` on a 4-core cloud VM (Intel Xeon 2.8 GHz):

```
BenchmarkProcessExecution-4    ~8,000 full instance lifecycles/sec/core
```

One "lifecycle" is the complete journey — start event, script task with expression evaluation, exclusive gateway, service task handler, end event, ~10 event-sourced history writes — through the in-memory store. There is no database round-trip and no lock-contention cliff: instances are serialized only against themselves.

## Documentation

- [Architecture](docs/architecture.md) — execution model, locking, persistence, determinism
- [BPMN coverage & semantics](docs/bpmn-coverage.md)
- [Expression language](docs/expressions.md)
- [REST API reference](docs/rest-api.md) — plus live OpenAPI 3.1 at `/openapi.json` and `/docs`
- [Roadmap alignment](docs/roadmap-alignment.md) — production, UI compatibility & AI integration mapped to capabilities
- [TypeScript SDK](packages/sdk-ts/README.md) — headless client + React hooks for any UI framework

## Repository layout

```
bpmn/      BPMN 2.0 model + XML parser + validation
expr/      sandboxed expression language
store/     persistence: interface, memory, durable journal, SQL (database/sql)
engine/    the token-based process engine + live migration
dmn/       decision tables
form/      schema-driven form engine (headless)
vault/     encrypted secrets store (AES-256-GCM)
otel/      zero-dep OpenTelemetry OTLP/HTTP trace exporter
replicate/ hot-standby journal replication (leader/follower)
rest/      HTTP API + SSE + metrics + auth/OIDC/RBAC + CORS + webhooks + OpenAPI
cmd/       the fliable binary
deploy/    Dockerfile companion: Helm chart + Kubernetes manifests
packages/  headless TypeScript SDK (@fliable/sdk) for any UI framework
examples/  runnable examples and sample models
docs/      architecture & reference documentation
```

## License

Apache License 2.0 — same license as Flowable, so evaluation and migration are friction-free.
