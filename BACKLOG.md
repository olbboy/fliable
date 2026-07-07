# BACKLOG — roadmap gap analysis with acceptance criteria

Source of truth: `fliable-beat-flowable-roadmap.md` (D1–D14 + NFRs),
narrowed to the two supreme goals: (1) production/high-performance,
(2) first-class AI agent integration. Every item lists a **measurable
acceptance criterion**; status is only DONE when build+tests+race are
green and the criterion holds.

Legend: ✅ done (evidence in tests/docs) · 🔶 partial · ⬜ open · 🚫 deliberately excluded (see DECISIONS.md)

## Goal 1 — production, high performance

| # | Roadmap item | Acceptance criterion | Status |
|---|---|---|---|
| 1.1 | D1 P0 BPMN 2.0 execution | Coverage doc + table-driven tests for tasks/gateways/events/scopes/MI | ✅ `docs/bpmn-coverage.md`, `engine_test.go` |
| 1.2 | NFR durable store | Crash-safe journal, `kill -9` tested; snapshot compaction | ✅ `store/journal.go`, store tests |
| 1.3 | NFR SQL store | Full `Store` on `database/sql`; engine E2E passes on it; multi-replica-safe claims | ✅ `store/sqlstore.go`, `TestEngineOnSQLStore`, claim tests |
| 1.4 | NFR HA / DR | Leader→standby replication; failover test completes an open task on the promoted engine; RPO = in-flight batch | ✅ `replicate/`, `TestLeaderFollowerFailover` (warm standby; active-active 🚫) |
| 1.5 | D13 P0 query API | Var predicates (8 ops), time windows, keyset pagination on all 3 stores | ✅ `store/query.go` + per-store tests |
| 1.6 | D11 P0 authN/authZ | API key + HMAC + **OIDC/JWKS** (RS/ES ×3 algs each) verified against real keys; per-route RBAC; tenant confinement | ✅ `rest/oidc.go`, `oidc_test.go` |
| 1.7 | D11 P0 multi-tenancy | TenantID on every record; per-(tenant,key) versioning; cross-tenant reads blocked | ✅ `engine/tenant_test.go` |
| 1.8 | D11 P0 secrets vault | AES-256-GCM at rest; plaintext never in store/history; wrong-key + tamper tests | ✅ `vault/`, `vault_test.go` |
| 1.9 | D10 P0 housekeeping | History TTL loop + GDPR purge removes every dependent record | ✅ `engine/housekeeping.go`, `purge_test.go` |
| 1.10 | D10 P0 ops cockpit primitives | suspend/resume, bulk ops, incident resolve over REST | ✅ `rest/ops.go` |
| 1.11 | NFR observability | Prometheus + SSE + OTLP/HTTP trace export with traceparent parenting, verified against an httptest collector | ✅ `otel/otel_test.go` |
| 1.12 | D2 P1 live migration | Dry-run validation; task/timer/subscription carry-over incl. message rename re-correlation; nothing written on validation failure | ✅ `engine/migrate_test.go` |
| 1.13 | D4 P0 forms | Server-side validation blocks completion (422) before state change; conditional visibility; defaults | ✅ `form/`, `rest/formapi_test.go` |
| 1.14 | D5 P1 triggers | Webhook channels: channel-secret auth, correlation/var mapping, idempotency dedup verified | ✅ `rest/webhook_test.go` |
| 1.15 | D13 P1 process intelligence | Cycle time p50/p95, incident + throughput stats endpoint | ✅ `rest/analytics.go` (report builder ⬜) |
| 1.16 | NFR packaging | Dockerfile (static scratch), compose (leader+standby), Helm, K8s manifests | ✅ (K8s Operator 🚫 for now) |
| 1.17 | NFR API | REST + OpenAPI 3.1 + SSE + webhooks | ✅ (gRPC 🚫 — zero-dep conflict) |
| 1.18 | NFR SDKs | Go (native) + TypeScript headless | ✅ (Python/Java SDK ⬜ — generate from OpenAPI) |
| 1.19 | NFR performance measured | Benchmarks run and recorded (see PROGRESS.md): full lifecycle ~124µs/op, agent path ~125µs/op, expr eval ~95ns/op | ✅ measured, not guessed |
| 1.20 | D1 P0 CMMN 1.1 | — | 🚫 second paradigm, see DECISIONS.md |
| 1.21 | D3 modeling/authoring (Design) | Collaborative canvas, AI authoring | ⬜ out of engine scope this line; headless SDK is the substrate |
| 1.22 | NFR IDM breadth | SAML/LDAP | 🚫 OIDC bridges cover; see DECISIONS.md |
| 1.23 | NFR conformance suites | Public BPMN/DMN conformance run | ⬜ |
| 1.24 | D12 connector marketplace | Public connector SDK/registry | ⬜ external-task pattern is the substrate |

## Goal 2 — first-class AI agent integration

| # | Roadmap item (D9) | Acceptance criterion | Status |
|---|---|---|---|
| 2.1 | P0 agent task type | Agent is a first-class activity: same token model, retries, incidents, boundary events; test-covered | ✅ `engine/agent.go`, `agent_test.go` |
| 2.2 | P0 structured output + tool chaining | Structured output merges into variables; tool calls recorded verbatim | ✅ |
| 2.3 | P1 MCP-native, provider-agnostic | Tool descriptors carry MCP server refs; no LLM SDK in core; in-process invoker OR external worker over `/v1/agent-jobs` (fetch-and-lock) | ✅ |
| 2.4 | P1 governance via event log | `agent.invoked`/`agent.completed`/`agent.rejected` carry prompt, tool calls, usage, approval — replayable audit | ✅ |
| 2.5 | P0/P1 human-in-the-loop | `agentApproval` gates output behind a user task; rejection discards | ✅ |
| 2.6 | P1 retry/timeout/cost guard | `AgentGuard`: invoke timeout backstop, per-instance token budget circuit-breaker, verified for both execution paths | ✅ `engine/guard_test.go` |
| 2.7 | P1 drift hook | `AgentGuard.Review` rejects results into retry→incident with `agent.rejected` audit events | ✅ |
| 2.8 | Config flexibility | Per-task model/effort/maxTokens/retries/approval in BPMN XML; per-agent invoker registry; guard via option | ✅ |
| 2.9 | Runnable E2E demo | `go run ./examples/ai-agent` — agent → MCP-style tool → approval → routed flow → audit printout, offline on a clean machine | ✅ |
| 2.10 | P2 orchestration dashboard | Latency/cost/queue dashboard UI | ⬜ data exists (metrics + analytics + history); UI out of scope |
| 2.11 | Automated drift detectors | Statistical drift detection shipping in-box | ⬜ hook exists (2.7); detectors are deployment-specific |

## Open items queue (prioritized)

1. ⬜ Python SDK generated from `/openapi.json` + smoke test (NFR SDKs).
2. ⬜ Conformance/chaos test expansion: property-based token-flow tests, replay-equivalence check (NFR P1).
3. ⬜ Index projection interface: stream history into Postgres/ClickHouse for heavy analytics (D13 P0 "pluggable index").
4. ⬜ Agent orchestration dashboard (D9 P2) — headless SDK + analytics endpoint are the substrate.
5. ⬜ Form debugger / live form dev tools (D3/D4 P2).
