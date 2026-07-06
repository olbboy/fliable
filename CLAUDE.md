# Fliable — developer notes

Compact BPMN 2.0 / DMN workflow engine in pure Go (zero dependencies,
stdlib only — keep it that way).

## Commands

- `make test` / `make race` — full suite; engine tests use a virtual
  clock (`newHarness`, `h.advance(d)`) so timer tests are instant and
  deterministic. Never add `time.Sleep`-based engine tests.
- `make vet` — vet + gofmt check (CI enforces both)
- `make bench` — engine & expression benchmarks
- `go run ./examples/embedded` — quick end-to-end sanity check

## Architecture (docs/architecture.md has the full version)

- `bpmn` — XML → flat `Element` model (type discriminator + field
  union). Parser also reads flowable/camunda/activiti extension attrs.
- `expr` — sandboxed expression language; compiled programs cached in
  `expr.Cached`. All numbers are float64.
- `store` — persistence records + `Store` interface; `Memory` (deep-copy
  isolation) and `Journal` (WAL + snapshot, crash-safe).
- `engine` — `runtime.drain()` advances tokens to quiescence under a
  striped per-instance lock. Cross-instance effects MUST go through
  `rt.continuations` (run after unlock) — never call into another
  instance while holding a lock, that's the deadlock seam.
- `dmn` — decision tables; plugs in via `engine.WithDecisionEvaluator`.
- `rest` — pure net/http handlers, Go 1.22 method patterns.

## Conventions

- Wait states are the persistence boundaries; every new wait state needs
  a `TokenState`, cancellation in `cancelWaits`, and a resume path.
- Every state change emits a `store.HistoryEvent` (`rt.emit`).
- Failures raise incidents — never swallow an error silently.
- Table-driven tests with inline BPMN XML fixtures (see engine_test.go).
