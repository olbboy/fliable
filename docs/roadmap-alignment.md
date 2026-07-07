# Roadmap alignment — production, UI compatibility, AI integration

This document maps the capabilities shipped in the v1.1 line against the
three goals set for Fliable and against the domains (D1–D14) and NFRs of
the *Beat-Flowable Roadmap*. It is deliberately honest about what is
**done**, what is **partial**, and what is **out of scope** for this line so
the backlog stays legible.

The three goals, restated:

1. **Run in production with high performance.**
2. **Be compatible with every UI framework** (Base UI, shadcn/ui, …).
3. **Run alongside and integrate customisably with AI agents / AI
   assistants in production, with high performance.**

---

## Goal 1 — Production, high performance

| Capability | Status | Where |
|---|---|---|
| Rich query API — variable predicates (`eq/ne/gt/gte/lt/lte/contains/exists`), time windows, keyset (cursor) pagination, sort direction | **done** | `store/query.go`, `store/memory.go`, `store/journal.go`, `rest/query.go` |
| Multi-tenancy — `TenantID` on every record, per-`(tenant,key)` definition versioning, tenant-scoped queries and confinement | **done** | `store/types.go`, `engine.DeployTenant/StartInstanceTenant`, `rest` tenant middleware |
| AuthN — pluggable `Authenticator` chain: API key, static bearer, HMAC-SHA256 signed tokens (`MintToken`/`ParseToken`) | **done** | `rest/auth.go` |
| AuthZ — per-route RBAC (`viewer`/`operator`/`admin`) + tenant confinement | **done** | `rest/server.go` `route()`/`guard()` |
| CORS — configurable middleware for browser clients | **done** | `rest/cors.go` |
| Suspend / resume instances; bulk operations | **done** | `engine.SuspendInstance/ResumeInstance`, `rest/ops.go` |
| Housekeeping — history TTL / retention with a background loop | **done** | `engine/housekeeping.go` |
| Instance purge (GDPR-style delete) | **done** | `store.PurgeInstance`, `engine` cleanup path |
| W3C Trace Context — `traceparent` ingested/propagated, correlation across external + agent workers | **done** | `rest/trace.go` |
| OpenAPI 3.1 document + dependency-free `/docs` page | **done** | `rest/openapi.go` |
| Durable crash-safe store (WAL + snapshot), single static binary, ~2 ms boot with recovery | **pre-existing** | `store/journal.go`, `cmd/fliable` |
| Prometheus metrics, SSE event stream, event-sourced history | **pre-existing / extended** | `rest`, `engine/metrics.go` |

Delivered in the 1.2 line (previously out of scope):

| Capability | Status | Where |
|---|---|---|
| SQL persistence — full `Store` over `database/sql` (Postgres/CockroachDB, MySQL, SQLite dialects); driver injected by the app so `go.mod` stays zero-dep; optimistic claims allow shared-database engine replicas | **done** | `store/sqlstore.go`, `store/sqltest` |
| Hot-standby replication — journal streaming over HTTP, snapshot bootstrap, auto-resync, dedup; promotion = start an engine on the follower | **done** | `store/replicate.go`, `replicate/`, `--replicate-to` / `--standby` |
| OIDC/JWT authentication — RS256/384/512 + ES256/384/512 against fetched or static JWKS; role mapping, tenant claims | **done** | `rest/oidc.go` |
| Encrypted secrets vault — AES-256-GCM at rest, `Context.Secret` for handlers, admin REST CRUD | **done** | `vault/`, `rest/vaultapi.go` |
| OpenTelemetry — OTLP/HTTP JSON span export derived from the event stream, traceparent-parented | **done** | `otel/`, `--otel` |
| Live instance migration with dry-run (D2 P1) | **done** | `engine/migrate.go` |
| Schema-driven forms (D4 P0) | **done** | `form/`, `rest/formapi.go` |
| Webhook event channels + idempotency (D5) | **done** | `rest/webhook.go` |
| Process analytics — cycle times, incident/throughput stats (D13 P1) | **done** | `rest/analytics.go` |
| Packaging — Dockerfile (scratch), Helm chart, K8s manifests | **done** | `Dockerfile`, `deploy/` |

**Still out of scope** (documented, not dropped): active-active
multi-node clustering (the standby is warm, not active-active), gRPC
(protobuf codegen conflicts with the zero-dependency rule; OpenAPI +
REST + SSE is the contract surface), SAML/LDAP (OIDC covers modern IdPs;
both bridge to OIDC), a Kubernetes Operator, and CMMN 1.1 (a second
execution paradigm — deliberate, not incidental).

---

## Goal 2 — Compatible with every UI framework

The strategy is **headless-first**: ship a typed data layer with no
markup, so any component library or framework binds to it.

| Capability | Status | Where |
|---|---|---|
| Framework-agnostic `FliableClient` covering every REST endpoint (instances, tasks, messages/signals, incidents, bulk ops, external + AI agent jobs, history, metrics, SSE) | **done** | `packages/sdk-ts/src/client.ts` |
| Headless React hooks returning **data + handlers only** — no markup | **done** | `packages/sdk-ts/src/react.tsx` |
| Worked examples binding the same hooks to **shadcn/ui** and **Base UI** | **done** | `packages/sdk-ts/README.md` |
| Live updates — SSE-backed `{ live: true }` and interval polling | **done** | `packages/sdk-ts/src/react.tsx` |
| Zero runtime dependencies; React is an optional peer dep behind `/react` | **done** | `packages/sdk-ts/package.json` |
| OpenAPI 3.1 spec so any generator can produce a client for another language | **done** | `rest/openapi.go` |

Because the client has no framework coupling, the same typed calls drop
into Vue composables, Svelte stores, or Solid resources with a thin
adapter. This delivers the roadmap's **D7 P1** ("work UI headless SDK")
and **NFR "SDKs: … TypeScript …"**.

**Out of scope this line:** the shipped visual apps themselves (Design
canvas / reflow, Work inbox app, Ops cockpit UI) — Fliable provides the
headless data layer they would be built on, not the pixels.

---

## Goal 3 — AI agent / assistant integration (roadmap D9, the leapfrog)

The engine stays **provider-agnostic and zero-dependency**: no LLM SDK
lives in the core. An agent is just another BPMN task, governed exactly
like every other task.

| Capability | Status | Where |
|---|---|---|
| Agent task type — an `agentTask` participates in the same token model, retries, and incident handling as any task | **done** | `engine/agent.go`, `engine/activity.go` |
| Provider-agnostic `AgentInvoker` interface — MCP-native tool descriptors, no vendored SDK | **done** | `engine/agent.go` |
| Two execution modes — in-process invoker, **or** external AI worker over REST (agent jobs, fetch-and-lock) | **done** | `engine/agent.go`, `rest/agent.go`, `store` agent-job methods |
| Governance via deterministic event log — `agent.invoked` / `agent.completed` history events capture prompt, tool calls, and token usage | **done** | `engine/agent.go` (`recordAgentResult`), `store/types.go` |
| Human-in-the-loop approval gate — agent proposal parked as a task, released on an `approved` variable | **done** | `engine/agent.go` (`createApprovalTask`), `engine.CompleteTask` branch |
| Structured output + BPMN-error routing from agent results | **done** | `engine/agent.go` (`applyAgentOutput`) |
| Per-agent metrics — invocations, input/output tokens | **done** | `engine/metrics.go` |
| New wait state `TokenWaitAgent` with cancellation + resume, per the wait-state convention | **done** | `store/types.go`, `engine/exec.go` |

This realises the roadmap's D9 P0 (agent task type, retry, tool chaining,
structured output) and the three D9 **P1 leapfrogs**: MCP-native, open
tooling; governance by deterministic replay; and agent-as-task under one
token model.

**Out of scope this line:** an orchestration dashboard (D9 P2) and
automated drift/hallucination detectors — the event log makes both
buildable, but neither ships in this line.

Guidance for wiring a real model (e.g. Claude) lives outside the engine:
implement `AgentInvoker` in your own worker binary, or run the external
AI-worker loop against `/v1/agent-jobs`. Keeping the LLM call out of core
is deliberate — it preserves the zero-dependency guarantee and lets any
provider or MCP toolchain plug in.

---

## Roadmap domain coverage summary

| Domain | Status | Notes |
|---|---|---|
| D2 — migration & versioning | **advanced** | per-tenant immutable versioning + drain; **live migration with activity map, var transforms and dry-run validation** |
| D4 — forms | **core** | schema-driven engine with validation/conditions/defaults; headless rendering by design |
| D5 — eventing & triggers | core | webhook channels with secrets, correlation, variable mapping, idempotency; Kafka/AMQP adapters live outside core via external workers |
| D7 — human work | advanced | full task API; **headless work SDK**; form-bound completions validate server-side |
| D9 — AI agent orchestration & governance | **core + leapfrogs** | see Goal 3 |
| D10 — operations & observability | **advanced** | suspend/resume, bulk ops, incident resolve, history TTL, **OpenTelemetry span export**, analytics endpoint; time-travel UI later |
| D11 — identity, security, tenancy | **advanced** | auth chain + **OIDC/JWKS**, RBAC, tenant isolation, **encrypted secrets vault** |
| D13 — query & index | core | rich filters + keyset pagination on memory, journal **and SQL**; analytics from the event log |
| NFRs | **substantial** | OpenAPI, SSE, TS SDK, single binary, history TTL, **SQL store, hot-standby DR, OTel, Docker/Helm/K8s**; active-active clustering, gRPC, Operator later |

The engine's zero-dependency, event-sourced, single-binary foundation is
unchanged — every capability above was added without pulling a single
third-party module into `go.mod`.
