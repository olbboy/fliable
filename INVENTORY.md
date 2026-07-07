# INVENTORY — codebase recon

Snapshot of what exists, where it lives, and how to drive it. Kept in
lockstep with the code so a fresh session can resume without re-reading
everything. Updated: 2026-07-07 (v1.2 line + agent guard).

## Build / run / test

| Action | Command |
|---|---|
| Full check | `make all` (vet + gofmt gate + tests + build) |
| Race suite | `make race` (`go test -race ./...`) — the merge gate |
| Benchmarks | `make bench` (engine + expr, `-benchmem`) |
| Server | `make serve` or `go run ./cmd/fliable serve --data ./data --deploy ./examples/processes` |
| Embedded demo | `go run ./examples/embedded` |
| **AI agent demo** | `go run ./examples/ai-agent` (offline, end-to-end governed agent flow) |
| Container | `docker build -t fliable .` / `docker compose up` (leader + standby) |
| Kubernetes | `deploy/helm/fliable` chart or `kubectl apply -f deploy/kubernetes.yaml` |
| TS SDK | `cd packages/sdk-ts && npm install && npx tsc -p tsconfig.json` |

Go 1.24, **zero dependencies** (`go.mod` has no require block — hard
invariant from CLAUDE.md). Engine tests use a virtual clock
(`newHarness`, `h.advance`) — timer tests are instant; never add
`time.Sleep`-based engine tests.

## Packages

| Package | Contents |
|---|---|
| `bpmn/` | Flat engine-oriented BPMN 2.0 model (`Element` type union), XML parser (+ Flowable/Camunda/Activiti extension attrs, `fliable:` agent extensions), aggregate validation |
| `expr/` | Sandboxed deterministic expression language: compiled+cached, float64 numbers, ~40 pure builtins, no I/O/reflection — used by conditions, scripts, mappings, DMN, forms, webhooks |
| `store/` | `Store` interface + records; `Memory` (deep-copy isolation), `Journal` (WAL + snapshot, crash-safe, replication hooks), `SQL` (`database/sql`, Postgres/MySQL/SQLite dialects, driver injected by app), `Blob` records (forms/secrets/webhooks), `sqltest/` in-memory SQL driver for tests |
| `engine/` | Token runtime: `drain()` to quiescence under striped per-instance locks; cross-instance effects via `rt.continuations` (the deadlock seam); wait states = persistence boundaries; retry→incident failure model; crash reconciliation; multi-instance; scopes/boundaries/event subprocesses; tenancy; suspend/resume; housekeeping TTL; **live migration** (`migrate.go`); **agent tasks** (`agent.go`) + **agent guard** (`guard.go`); metrics |
| `dmn/` | Decision tables, 5 hit policies + aggregations, FEEL-style unary tests; plugs in via `engine.WithDecisionEvaluator` |
| `form/` | Schema-driven form definitions + server-side validation, persisted as blobs, bound to tasks by formKey via `engine.WithFormValidator` |
| `vault/` | AES-256-GCM secrets store over blobs; `engine.WithSecrets` → `Context.Secret(name)` |
| `otel/` | Zero-dep OTLP/HTTP JSON trace exporter fed by the history event stream; traceparent-parented |
| `replicate/` | Leader→standby journal streaming over HTTP (snapshot bootstrap, auto-resync); promotion = start engine on follower store |
| `rest/` | net/http API: instances/tasks/messages/signals/incidents/history/SSE/metrics, query envelopes + keyset pagination, auth chain (API key / static / HMAC / **OIDC+JWKS**), per-route RBAC, tenant confinement, CORS, trace context, bulk ops, migration, forms, secrets, **webhook channels**, analytics, agent-job worker API, OpenAPI 3.1 + `/docs` |
| `cmd/fliable` | Single binary: `serve` (flags: `--data --fsync --api-key --deploy --poll --otel --replicate-to --standby`; env: `FLIABLE_MASTER_KEY`, `FLIABLE_REPLICATION_SECRET`), `validate`, `version` |
| `packages/sdk-ts` | `@fliable/sdk`: framework-agnostic typed client (all endpoints incl. agent jobs, forms, migration, analytics) + headless React hooks |
| `examples/` | `embedded/` (Go API walkthrough), `ai-agent/` (governed agent E2E), `processes/` (sample BPMN) |
| `deploy/` | Helm chart, plain K8s manifests; `Dockerfile` + `docker-compose.yml` at repo root |
| `docs/` | architecture, bpmn-coverage, expressions, rest-api, roadmap-alignment |

## Key seams (extension points)

- `store.Store` — persistence (3 impls; conformance shared via extracted predicates in `store/query.go`)
- `engine.ServiceHandler` / external-task topics — service work
- `engine.AgentInvoker` + `/v1/agent-jobs` — AI providers (MCP-native tool descriptors)
- `engine.AgentGuard` — timeout / token budget / drift review
- `engine.DecisionEvaluator`, `engine.FormValidator`, `engine.SecretSource`
- `rest.Authenticator` — auth chain
- `engine.OnEvent` — history stream (feeds SSE, otel, custom sinks)

## Test surface

~11 test packages, all green under `-race`. Notables: crash-recovery
(`kill -9`-style journal truncation), leader/follower failover with
promotion, engine-on-SQL end-to-end, OIDC with real generated keys,
agent governance (budget/review/timeout, in-process + external), live
migration (task/timer/message waits, validation rejections), form-bound
task completion via REST, webhook dedup.
