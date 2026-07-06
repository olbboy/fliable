# Fliable architecture

## Overview

```
             ┌────────────────────────────────────────────────┐
             │                 cmd/fliable                    │
             │  single binary: serve / validate / version     │
             └───────────────┬────────────────────────────────┘
                             │
   HTTP clients ──────► ┌────▼─────┐        ┌───────────┐
   (REST, SSE,          │   rest   │        │    dmn    │
    workers)            └────┬─────┘        └─────▲─────┘
                             │       decisions    │
   Go programs ───────► ┌────▼────────────────────┴─────┐
   (embedded)           │            engine             │
                        │  token runtime · scheduler ·  │
                        │  incidents · history · locks  │
                        └──┬──────────────┬─────────────┘
                           │              │
                     ┌─────▼─────┐  ┌─────▼─────┐
                     │   bpmn    │  │   store   │
                     │ model+XML │  │ memory /  │
                     │ +validate │  │ journal / │
                     └───────────┘  │ your impl │
                           ▲        └───────────┘
                     ┌─────┴─────┐
                     │   expr    │  sandboxed expressions
                     └───────────┘
```

Every package is usable on its own; `engine` is the composition point.

## Execution model

**Definitions are immutable graphs.** BPMN XML parses once into a flat,
engine-oriented model (`bpmn.Element` with a type discriminator) and is
cached per definition version. Redeploying a process key bumps the
version; running instances keep executing the version they started on.

**Instances are small records.** A `store.Instance` holds variables, a
map of tokens, and multi-instance loop state. A token is an execution
pointer: element ID, scope path (chain of enclosing sub-process IDs),
state, and a reference to whatever it is waiting for.

**Run-to-quiescence.** All engine work happens inside `runtime.drain()`:
pick the lowest-ID active token, execute its element behavior, repeat
until every token is parked on a wait state (user task, timer, message,
external worker, child instance, join) or the instance ends. Behaviors
mutate the instance in memory and write side records (tasks, jobs,
subscriptions, incidents, history) through the store; the instance record
is persisted once when the run goes quiet. This makes wait states the
transaction boundaries — the same model Flowable uses, without the
operation-stack machinery.

**Concurrency = striped locks, not row locks.** Each instance is
serialized by one of 64 striped mutexes; different instances advance
fully in parallel. Cross-instance effects (starting a call-activity
child, notifying a parent, throwing messages/signals to other instances)
are queued as *continuations* and executed after the current instance's
lock is released — this makes lock-ordering deadlocks structurally
impossible, including parent/child cycles.

**One scheduler goroutine.** Timers, async continuations, retries and
timer-start events are all `store.Job` records claimed atomically by
`DueJobs`. The scheduler polls (default 100 ms); tests inject a virtual
clock and call `RunDueJobs` directly for deterministic time. A claimed
job whose execution fails is re-queued with backoff and eventually
surfaces as an incident — never silently dropped.

**Handlers run under the instance lock.** Service handlers and OnEvent
listeners execute while the instance's stripe lock is held: they must not
call engine methods synchronously (return output variables, use external
worker topics, or hand off to a goroutine instead), or they can deadlock.

**Startup reconciliation.** `Start()` first repairs the small
at-least-once windows a crash can leave behind: call-activity children
that were never started (→ resolvable incident) or whose completion never
reached the parent (→ parent resumed), wait states whose claimed job was
lost (→ re-armed), and orphaned task records (→ cancelled). Broken states
become visible incidents, never silent hangs.

## Scopes, boundaries, errors

Sub-processes create nested scopes identified by the token scope path.
The owner token parks at the sub-process element while child tokens run
inside; when the last token in a scope is consumed, the scope completes
and the owner resumes. Boundary events register jobs/subscriptions tied
to the host activity's token and are cancelled when the activity
completes. BPMN errors route innermost-first: error boundary on the
failing activity → event sub-process in each enclosing scope → error
boundary on each enclosing sub-process → unhandled (incident + instance
termination, propagated to a call-activity parent).

## Persistence

`store.Store` is a narrow CRUD + query interface. Two implementations
ship in-tree:

- **Memory** — maps guarded by an RWMutex; every read returns a deep
  copy so callers can never alias engine state.
- **Journal** — wraps Memory with an append-only JSON write-ahead log
  and periodic atomic snapshot compaction. Recovery = load snapshot +
  replay journal; a torn final line (crash mid-write) is detected and
  dropped. `Fsync` mode survives OS crashes, default mode survives
  process crashes.

Store methods must be individually atomic; cross-record consistency is
the engine's job (it serializes per instance). This keeps custom Store
implementations (Postgres, etc.) straightforward: no transactions
spanning calls are required, though `DueJobs` and
`FetchAndLockExternalTasks` must claim atomically.

## Determinism & replay

History is an event-sourced stream (`store.HistoryEvent`) with per-
instance sequence numbers, emitted for every state change and fanned out
to in-process listeners and SSE clients. The engine clock is injectable
(`WithClock`), expressions are deterministic, and IDs embed timestamps
from that clock — the ingredients for reproducible tests and time-travel
debugging.

## What Fliable deliberately does not do

- **Multi-node clustering.** Fliable is embedded-first, like SQLite.
  The Store interface is the seam for shared-storage deployments.
- **In-engine scripting runtimes.** Script tasks use the sandboxed
  expression language; real logic belongs in Go handlers or external
  workers, where it is testable and typed.
- **CMMN.** Case-management semantics are out of scope; most real-world
  CMMN usage maps onto event sub-processes + user tasks, which Fliable
  covers.
