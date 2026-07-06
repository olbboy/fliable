# BPMN 2.0 coverage & semantics

Fliable's extension namespace is `https://fliable.dev/schema/1.0`
(prefix `fliable` below). For migration, the same attributes are also
read from the Flowable, Camunda and Activiti namespaces, and JUEL-style
`${...}` / `#{...}` wrappers are stripped from expressions.

## Tasks

| Element | Notes |
|---|---|
| `userTask` | `fliable:assignee`, `fliable:candidateUsers`, `fliable:candidateGroups` (comma lists), `fliable:formKey`, `fliable:dueDate` (ISO duration = relative, or date, or expression), `fliable:priority`. Fields are literals unless wrapped `${...}` or prefixed `=`. |
| `serviceTask` | Three execution modes, checked in order: `fliable:topic` → external worker task; `fliable:expression` → inline expression, result to `fliable:resultVariable`; `fliable:type` (also read from `delegateExpression`/`class`) → registered Go handler. `fliable:retries` sets the retry budget (default 3). |
| `scriptTask` | `<script>` body is one expression; result assigned to `fliable:resultVariable`. |
| `businessRuleTask` | `fliable:decisionRef` names a decision in the configured evaluator (see dmn package); result to `fliable:resultVariable` (default `decisionResult`). |
| `sendTask` | Like serviceTask; additionally throws its `messageRef` for in-process correlation. |
| `receiveTask` | Waits for its `messageRef`; correlation key = instance business key. |
| `manualTask`, `task` | Pass-through. |

All activities support `fliable:async="true"` (continuation via job
executor), boundary events, multi-instance and
`extensionElements/inputOutput` variable mappings (`inputParameter` sets
token-local variables; `outputParameter` maps expressions back to
instance variables).

## Gateways

| Element | Semantics |
|---|---|
| `exclusiveGateway` | Conditions evaluated in document order; first true wins; `default` flow as fallback; no match → incident. |
| `parallelGateway` | Fork on all outgoing; join waits for one token per incoming flow. |
| `inclusiveGateway` | Fork on all true conditions (default as fallback). Join fires when no other token in the instance can still reach it (graph reachability, including boundary-event paths and tokens in nested scopes projected onto their sub-process element). |
| `eventBasedGateway` | Targets must be intermediate catch events or receive tasks. First trigger wins and cancels the other waits. |

## Events

| Trigger | Start | Intermediate catch | Boundary | End |
|---|---|---|---|---|
| none | ✅ | — | — | ✅ |
| timer | ✅ (`timeDate`, `timeDuration`, `timeCycle` incl. `R<n>/…`) | ✅ | ✅ interrupting & non-interrupting (cycles repeat) | — |
| message | ✅ (new instance per correlation) | ✅ | ✅ | ✅ (throws internally) |
| signal | ✅ | ✅ | ✅ | ✅ (broadcast) |
| error | ✅ (event sub-process only) | — | ✅ | ✅ (throws) |
| terminate | — | — | — | ✅ (ends the enclosing scope; at root, the instance) |

Intermediate throw events support none/message/signal. Timer values may
be expressions. Non-interrupting variants: `cancelActivity="false"` on
boundary events, `isInterrupting="false"` on event sub-process starts.

## Scopes

- **`subProcess`** — embedded scope; completes when its last token dies.
  Boundary events on the sub-process cancel the whole scope.
- **event `subProcess`** (`triggeredByEvent="true"`) — armed while its
  parent scope is active; message/signal/timer/error typed starts;
  interrupting starts kill sibling tokens in the scope.
- **`callActivity`** — starts a child instance of `calledElement`
  (latest version). `inputParameter`/`outputParameter` map variables in
  and out; with no input mappings the child inherits a copy of all
  variables. Cancelling either side propagates to the other. Errors
  unhandled in the child terminate it and notify the parent.

## Multi-instance

`multiInstanceLoopCharacteristics` on any activity:

- `fliable:collection` (expression → list) or `<loopCardinality>`
- `fliable:elementVariable` — per-iteration variable (token-local)
- `isSequential` — one at a time vs all at once
- `<completionCondition>` — checked after each completion with
  `nrOfInstances`, `nrOfCompletedInstances`, `nrOfActiveInstances`;
  true cancels the remaining iterations
- `fliable:outputElement` (expression per iteration) collected into
  `fliable:outputCollection`

## Timers

ISO-8601 throughout: durations (`PT10M`, `P1DT2H`), dates
(RFC3339 / `2026-01-02`), cycles (`R3/PT10S`, `R/PT1H` unbounded).
Years/months use 365/30-day civil approximations.

## Failure semantics

Handler/expression failures consume retries with exponential backoff
(base 5s ×2 per attempt, `WithRetryBackoff` to tune), then raise an
**incident** and park the token. `ResolveIncident` re-activates the
element with a fresh retry budget. `engine.NewBPMNError(code, msg)`
bypasses retries and routes as a BPMN error: activity boundary → event
sub-processes and scope boundaries outward → unhandled = incident +
termination. `errorCode`/`errorMessage` become instance variables in the
handler path.
