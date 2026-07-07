# Changelog

All notable changes to Fliable are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/), and the engine follows
[Semantic Versioning](https://semver.org/).

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
