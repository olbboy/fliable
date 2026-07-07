# DECISIONS — architecture decision log

Why things are the way they are. Every deliberate exclusion and every
non-obvious design choice, so future sessions don't re-litigate them.

## D-01 Zero dependencies is a product feature, not a constraint to work around
`go.mod` has no require block, ever (CLAUDE.md invariant). Everything —
OIDC crypto, AES-GCM vault, OTLP export, SQL store, replication — is
stdlib. Consequence: features whose ecosystem demands codegen or vendored
SDKs are excluded (see D-05) or pushed across a seam (see D-02, D-04).
This is the structural moat vs JVM engines: single ~8 MB static binary,
~2 ms boot, no CVE surface from a dependency tree.

## D-02 LLM calls never live in the engine core
Goal 2 ("first-class AI") is delivered as *orchestration + governance*,
not as a bundled model client. The `AgentInvoker` interface (in-process)
and the `/v1/agent-jobs` fetch-and-lock API (external workers) are the
seams; tool descriptors are MCP-shaped so any MCP toolchain binds.
Rationale: provider-agnostic (roadmap D9 P1 explicitly targets beating
Flowable's Spring-AI vendoring), keeps zero-dep, and the external-worker
path scales AI workloads independently of the engine.

## D-03 SQL store: one generic record table + Go-side filtering
`store.SQL` uses a single `fliable_records` table (kind/id PK, JSON
payload, few indexed columns) with a deliberately tiny SQL surface
(~8 statement shapes). Fine filtering reuses the exact predicate
functions the memory store uses, so all backends behave identically and
the SQL store inherits the whole filter test surface. Claims are
optimistic single-row UPDATE/DELETE guards → safe with multiple engine
replicas on one database, no SELECT FOR UPDATE (not portable). Trade-off
accepted: list queries scan a kind (bounded by indexes); pushdown of hot
filters is a later optimization, correctness and portability first.
The driver is injected by the embedding app (`*sql.DB`), keeping D-01.

## D-04 Testing SQL without a database: a faithful fake driver
`store/sqltest` implements `database/sql/driver` for exactly the
statement set the store issues, with real semantics (upsert, optimistic
claim, delete-as-claim). This is NOT a mock that makes tests pass — the
marshaling/filtering/claim logic under test is the store's own; only the
storage substrate is swapped. The same tests run against real Postgres by
swapping `OpenDB`. Chosen over requiring Postgres in CI (impossible here)
and over skipping tests (forbidden).

## D-05 gRPC excluded
gRPC requires protobuf codegen + google.golang.org modules — a direct
D-01 violation with no stdlib path (hand-rolled proto is unmaintainable).
The contract surface is REST + OpenAPI 3.1 + SSE + webhooks; any language
generates a client from `/openapi.json`. Revisit only if D-01 is ever
relaxed.

## D-06 CMMN 1.1 excluded (for now)
A full second execution paradigm (plan items, sentries, discretionary
items) is months of work and would dilute the two supreme goals. BPMN
event subprocesses + message/signal correlation + the automation
(webhook) layer cover a useful slice of case-management patterns.
Documented as the largest known scope cut; the unified event-sourced
runtime (roadmap D1 P1) remains the design target when it lands.

## D-07 HA = warm standby, not active-active
Active-active multi-node needs distributed consensus or partitioned
ownership — a different engine architecture. What ships: (a) SQL store
with optimistic claims → N engines on one database for job/worker
distribution; (b) journal replication to a warm standby with ~2 ms
promotion (RPO = in-flight batch, RTO = boot). This covers the DR NFR
honestly; "no single point of failure" via consensus is future work and
is *not* claimed anywhere.

## D-08 Secrets: engine-side resolution, name-bound AEAD
Secrets resolve only at `Context.Secret(name)` in service handlers —
never in expressions (the expression language stays pure/deterministic;
a `secret()` builtin would leak values into history via interpolation).
AES-256-GCM with the secret *name* as additional data, so a ciphertext
moved to another name fails to open. Master key comes from the
environment and is never persisted. `GET /v1/secrets/{name}/value` exists
(admin-only) because external workers legitimately need credentials;
listing returns names only.

## D-09 Agent guard semantics
- Token budget is charged to a reserved instance variable
  (`__agentTokens`) rather than a side table: it persists, replicates,
  purges, and migrates with the instance for free.
- Budget exhaustion raises an *incident* (never silent, resumable after
  a human raises the budget or resolves) instead of failing the instance.
- `Review` runs after the result is recorded in history — a rejected
  output is still auditable evidence — and rejection goes through the
  standard retry→incident cycle, so drift handling is governed like any
  failure.
- The in-process invoke timeout abandons the goroutine (documented):
  protecting the engine beats leaking one goroutine on a hung provider;
  invokers should carry their own deadlines. External jobs need no extra
  timeout — lock expiry already re-delivers.

## D-10 Live migration: validate-everything-then-apply
Migration validates every token (element existence, type equality,
wait-state compatibility, scope-path survival) and all variable
transforms before writing anything; any issue → report only. No partial
migrations, ever. Timer waits keep their original due time (changing
schedules is a modeling decision, not a migration side effect);
message/signal subscription *names* refresh from the target model so
renamed events correlate. Multi-instance tokens require the target
element to still be multi-instance.

## D-11 Replication: push + resync-on-anything
The leader pushes (OnAppend tap → bounded channel → batched HTTP).
Pull-from-offset was rejected because journal compaction truncates the
file, making stable offsets a lie. Any error/overflow triggers a full
snapshot resync — always converges, no offset bookkeeping. History dedup
by (instance, seq) makes the snapshot/stream overlap idempotent. The
follower's engine stays cold (started only on promotion) so timers can
never double-fire — this is why `--standby` runs only the listener.

## D-12 Forms/webhooks/secrets persist as store blobs
One generic `Blob` record (kind+key+bytes) instead of three new record
types: one Store-interface change served three features, and every store
(memory/journal/SQL) got them for free, including snapshots, replication
and purge.

## D-13 OTel: derive spans from the history stream
The exporter subscribes to `engine.OnEvent` instead of instrumenting the
engine internals: zero hot-path cost when unused, and spans are exactly
the audit trail (element activated→completed), which is the semantic
users want. Trace identity: caller's `traceparent` if present, else
deterministic from the instance ID (retries land in the same trace).
Metrics stay Prometheus (`/metrics`) — no OTLP metrics duplication.

## D-14 SAML/LDAP excluded
Every major IdP (Entra, Okta, Auth0, Keycloak, Dex) fronts SAML/LDAP
estates with OIDC. Implementing XML-DSig (SAML) correctly in stdlib is a
security risk with negative expected value. OIDC + API keys + HMAC +
static tokens cover the auth matrix.

## D-15 Demo invoker is deterministic, and that's the point
`examples/ai-agent` uses a scripted invoker so the demo runs offline on a
clean machine (Definition-of-Done requirement) and exercises *the
engine's* contract: templating, MCP tool descriptors, structured output,
approval, guard, audit. A real provider is a drop-in `AgentInvoker` in
the embedder's module — documented in the example header.

## D-16 v1.0.0 tag push blocked by environment
The session's git proxy rejects tag ref pushes (403) and the GitHub MCP
toolset has no release-creation tool. CHANGELOG is authoritative in-repo;
a maintainer must push tags/releases from an unrestricted environment.
