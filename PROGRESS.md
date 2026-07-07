# PROGRESS — plan, status, evidence

Phases follow the roadmap ordering (engine credibility → human
work/authoring substrate → AI leapfrog → breadth). Status is only DONE
with green build + `-race` tests + the acceptance criterion from
BACKLOG.md. Updated: 2026-07-07.

## Phase 0 — engine credibility (production core) ✅ DONE
- [x] BPMN 2.0 runtime, DMN, sandboxed expressions, retry→incident, crash
      reconciliation *(pre-existing, verified)*
- [x] Rich query API: var predicates, time windows, keyset pagination
- [x] Multi-tenancy end-to-end; auth chain + per-route RBAC + CORS
- [x] OIDC/JWKS authentication (RS256/384/512, ES256/384/512)
- [x] Encrypted secrets vault + `Context.Secret`
- [x] Housekeeping TTL + GDPR purge; suspend/resume; bulk ops
- [x] SQL store (`database/sql`, 3 dialects) + engine E2E on it
- [x] Hot-standby replication + tested failover/promotion
- [x] OpenTelemetry OTLP/HTTP trace export + W3C trace context
- [x] Packaging: Dockerfile (scratch), docker-compose (leader+standby),
      Helm chart, K8s manifests

## Phase 1 — human work & headless substrate ✅ DONE
- [x] Task API (claim/complete/candidates/due/priority) *(pre-existing)*
- [x] Schema-driven forms + server-side validation on completion
- [x] Headless TypeScript SDK (`@fliable/sdk`) + React hooks; OpenAPI 3.1
- [x] Live instance migration with dry-run (D2 leapfrog, pulled forward)

## Phase 2 — AI leapfrog (goal 2) ✅ DONE
- [x] Agent task as first-class activity (token model, retries, incidents)
- [x] MCP-native tool descriptors; provider-agnostic invoker seam
- [x] External AI workers over `/v1/agent-jobs` (fetch-and-lock)
- [x] Governance by event log: agent.invoked/completed/rejected with
      prompt, tool calls, usage, approval identity
- [x] Human-in-the-loop approval gate
- [x] AgentGuard: invoke timeout, per-instance token budget, drift review
      hook — enforced on both execution paths
- [x] Runnable E2E demo: `go run ./examples/ai-agent`
- [x] Agent metrics (invocations, tokens) + analytics endpoint

## Phase 3 — breadth 🔶 IN PROGRESS
- [x] Webhook event channels + idempotency (automation/n8n-style)
- [x] Process analytics (cycle times p50/p95, incidents, throughput)
- [ ] Python SDK generated from OpenAPI (queue #1)
- [ ] Property-based / replay-equivalence chaos tests (queue #2)
- [ ] History index projection interface (queue #3)
- [ ] Agent orchestration dashboard (queue #4)
- Excluded with rationale (DECISIONS.md): CMMN (D-06), gRPC (D-05),
  SAML/LDAP (D-14), active-active clustering (D-07)

## Definition-of-done checklist (mission)
- [x] Single static binary builds clean (`CGO_ENABLED=0`, 7.8 MB stripped)
- [x] `go test ./... -race` fully green (11 packages)
- [x] `go vet` + gofmt gate clean (`make vet`)
- [x] Benchmarks run and recorded (below)
- [x] Deployable: Dockerfile + docker-compose + Helm; runs from a clean
      machine (`docker compose up`)
- [x] AI-agent workflow demo runs end-to-end offline
- [x] Docs + runnable examples; INVENTORY/BACKLOG/PROGRESS/DECISIONS current
- [x] Git history: small per-task commits, every commit green
- [ ] v1.0.0 tag push — blocked by environment (DECISIONS D-16)

## Measured performance (4-core cloud VM, Go 1.24, in-memory store)

| Benchmark | Result | Meaning |
|---|---|---|
| `BenchmarkProcessExecution` | ~123 µs/op, 10.7 KB, 98 allocs | full lifecycle: start → script+expr → gateway → service → end, ~10 history writes → **~8,100 instances/s/core** |
| `BenchmarkProcessExecutionParallel` | ~138 µs/op (all cores) | striped locks under contention on one shared memory store |
| `BenchmarkAgentTask` | ~125 µs/op, 11.3 KB, 105 allocs | fully governed agent invocation (templating, governance events, budget charge, review) minus the model call → **engine overhead ≈0.1 ms per agent step** — LLM latency dominates by 3–4 orders of magnitude |
| `BenchmarkEvalCondition` (expr) | ~95 ns/op, 1 alloc | compiled+cached expression evaluation on the hot path |

Interpretation vs NFR targets: engine overhead is negligible against any
real workload's I/O (LLM calls, human tasks, network). The measured
bottleneck under parallel load is the single memory-store mutex — the
SQL store shards contention differently (per-statement), and per-instance
striping already prevents cross-instance serialization. No further
optimization until a measured workload demands it (**đo, không đoán**).

## How to resume a session
Read INVENTORY.md (what exists) → BACKLOG.md open items (what's next,
with acceptance criteria) → DECISIONS.md (what not to re-litigate).
Verify baseline with `make vet && make race` before changing anything.
