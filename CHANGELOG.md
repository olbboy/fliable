# Changelog

All notable changes to Fliable are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/), and the engine follows
[Semantic Versioning](https://semver.org/).

## [1.2.0] — 2026-07-07

The remaining production pillars, still without a single entry in
`go.mod`: SQL persistence, disaster recovery, enterprise identity,
secrets, OpenTelemetry, forms, live migration, event channels and
Kubernetes packaging.

### Persistence & high availability
- `store.SQL` — the full Store contract over `database/sql`
  (Postgres/CockroachDB, MySQL, SQLite dialects). The embedding app
  imports its driver; Fliable stays zero-dependency. Optimistic
  single-row claims let several engine replicas share one database.
  `store/sqltest` ships an in-memory driver so it all tests without a
  running database.
- Hot-standby replication (`replicate` package): the journal streams to
  a warm follower over HTTP (snapshot bootstrap + ordered line
  streaming, automatic full resync on any error, history deduplicated by
  sequence). Promotion = start an engine on the follower's store.
  `fliable serve --replicate-to` / `--standby`.

### Identity, secrets
- OIDC/JWT bearer auth against any identity provider: RS256/384/512 and
  ES256/384/512 via fetched-and-cached or static JWKS, issuer/audience
  checks, nested role claims with mapping, tenant-claim confinement.
  Stdlib crypto only.
- Encrypted secrets vault (`vault` package + `/v1/secrets`): AES-256-GCM
  at rest, master key from the environment, values reachable from
  service handlers via `Context.Secret` — never through process
  variables or history. `FLIABLE_MASTER_KEY` enables it in the binary.

### Operations
- Live instance migration with dry-run validation
  (`POST /v1/instances/{id}/migrate`): activity mapping, variable
  transforms, wait-state compatibility checks; open tasks, timers and
  message/signal subscriptions carry over (names refreshed from the
  target model).
- OpenTelemetry trace export (`otel` package, `--otel` flag): spans per
  instance and element derived from the history stream, parented under
  the caller's W3C traceparent, OTLP/HTTP JSON to any collector.
- `GET /v1/analytics/processes`: cycle times (avg/p50/p95), state
  counts, open incidents/tasks, 24h throughput per definition.

### Forms & event channels
- Schema-driven form engine (`form` package): typed fields, validation
  rules, conditional visibility, defaults; bound to user tasks by
  formKey; invalid completions rejected with 422 before any state
  change; `GET /v1/tasks/{id}/form` feeds any headless renderer.
- Webhook event channels (`/v1/webhooks/{name}`): per-channel secret,
  payload-expression correlation and variable mapping onto messages or
  signals, idempotency via header or configured dedupe expression.

### Packaging & SDK
- Dockerfile (scratch image, ~8 MB static binary), Helm chart with
  optional warm standby, plain Kubernetes manifests (`deploy/`).
- `@fliable/sdk` 1.2.0: migration, forms, webhooks, secrets and
  analytics APIs added to the typed client.

## [1.1.0] — 2026-07-07

Production-hardening, universal UI compatibility, and first-class AI agent
orchestration — all added without pulling a single dependency into
`go.mod` (Go standard library only, unchanged).

### AI agents
- Agent task type: an LLM-with-tools step that participates in the same
  token model, retries and incident handling as any other task. New
  `TokenWaitAgent` wait state with cancellation and resume.
- Provider-agnostic `AgentInvoker` interface with MCP-native tool
  descriptors — no model SDK in the core. Runs in-process **or** as an
  external AI worker polling `/v1/agent-jobs` (fetch-and-lock).
- Governance by event log: `agent.invoked` / `agent.completed` history
  events capture prompt, tool calls and token usage.
- Human-in-the-loop approval gate; structured output; BPMN-error routing;
  per-agent invocation and token metrics.

### Production & operations
- Rich query API: variable predicates (`eq/ne/gt/gte/lt/lte/contains/exists`),
  time windows, keyset (cursor) pagination and sort direction; paged
  `{ items, nextCursor }` envelopes. No external index required.
- Multi-tenancy: `TenantID` on every record; per-`(tenant, key)`
  definition versioning; tenant-confined queries and REST.
- Auth: pluggable `Authenticator` chain (API key / static bearer /
  HMAC-SHA256 signed tokens via `MintToken`/`ParseToken`); per-route RBAC
  (viewer / operator / admin); CORS middleware.
- Suspend / resume instances; bulk operations.
- History TTL housekeeping loop; `PurgeInstance` for GDPR-style deletes.
- W3C Trace Context (`traceparent`) ingestion and propagation across
  external and agent workers.

### API & SDK
- OpenAPI 3.1 document at `/openapi.json` with a dependency-free `/docs`
  reference page.
- New `@fliable/sdk` TypeScript package: framework-agnostic typed client
  plus optional headless React hooks (data + handlers, no markup) so
  shadcn/ui, Base UI, Vue, Svelte and Solid all bind to the same layer.
  Zero runtime dependencies; React is an optional peer dep.
- `docs/roadmap-alignment.md` maps delivered capabilities to the
  Beat-Flowable roadmap domains and NFRs.

## [1.0.0] — 2026-07-07

First stable release: a compact, high-efficiency BPMN 2.0 / DMN workflow
& BPM platform in pure Go. Single ~7 MB static binary, zero dependencies
(Go standard library only).

### Engine
- BPMN 2.0 token runtime: user / service / script / business-rule / send
  / receive / manual tasks; exclusive, parallel, inclusive and
  event-based gateways; timer / message / signal / error / terminate
  events across start, intermediate (catch & throw), boundary
  (interrupting & non-interrupting) and end positions; embedded and
  event sub-processes; call activities with variable mappings; parallel
  and sequential multi-instance with completion conditions and output
  collections.
- Retry-with-backoff → incident failure model (failures are never
  silent); one-call incident resolution resumes the parked token.
- Startup crash reconciliation repairs at-least-once windows (lost
  call-activity children, unreported child completions, lost timer/retry
  jobs, orphaned task records) into resolvable incidents.
- Striped per-instance locking; cross-instance effects deferred past the
  lock so lock-order deadlocks are structurally impossible.
- Event-sourced history with in-process listeners and a live SSE feed.

### Expression language
- Sandboxed, deterministic, compiled-and-cached. JSON value model plus
  `time.Time`, safe navigation, null coalescing, `in` membership, ~40
  pure builtin functions. No reflection, no method calls, no I/O.

### Storage
- `Store` interface with an in-memory implementation (deep-copy
  isolation) and a durable, crash-safe journal store (append-only JSON
  WAL + atomic snapshot compaction) that survives torn writes and
  `kill -9`.

### Decisions (DMN)
- Decision tables with UNIQUE / FIRST / ANY / PRIORITY / COLLECT hit
  policies (+ SUM/MIN/MAX/COUNT aggregation) and FEEL-style unary tests;
  loadable from DMN XML or built programmatically; wired into business
  rule tasks via `engine.WithDecisionEvaluator`.

### API & tooling
- Pure `net/http` REST API: deploy, instances, tasks, messages/signals,
  external worker fetch-and-lock, incidents, history, live server-sent
  events, Prometheus metrics, health.
- `fliable` CLI: `serve` (durable storage, startup auto-deploy),
  `validate`, `version`.

### Migration
- Parser reads Flowable / Camunda / Activiti extension attributes and
  JUEL-style `${...}` expressions, so existing models deploy unchanged.

[1.0.0]: https://github.com/olbboy/fliable/releases/tag/v1.0.0
