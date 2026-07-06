# REST API reference

Base path `/v1`. All bodies are JSON unless noted. With
`--api-key`, every request except `GET /healthz` requires the
`X-Api-Key` header. Errors return `{"error": "..."}` with 400/401/404/
409/422 as appropriate.

## Definitions

| Method & path | Description |
|---|---|
| `POST /v1/definitions` | Deploy — body is **raw BPMN XML** (optional `?name=`). Same process id ⇒ new version. `422` with the full validation problem list on bad models. |
| `GET /v1/definitions` | List (latest version per key; `?latest=false` for all). |
| `GET /v1/definitions/{id}` | Metadata. |
| `GET /v1/definitions/{id}/xml` | Original XML. |

## Instances

| Method & path | Description |
|---|---|
| `POST /v1/instances` | `{definitionKey \| definitionId, businessKey?, variables?}` → the started instance (may already be completed). |
| `GET /v1/instances` | Filters: `definitionKey`, `businessKey`, `state`, `parentId`, `limit`. |
| `GET /v1/instances/{id}` | Full state incl. variables and tokens. |
| `DELETE /v1/instances/{id}?reason=` | Cancel: cancels tasks/jobs/subscriptions/children, records history. |
| `PUT /v1/instances/{id}/variables` | Merge variables `{...}`. |
| `GET /v1/instances/{id}/history` | Audit trail. Filters: `type` (prefix), `afterSeq`, `limit`. |
| `GET /v1/instances/{id}/incidents` | Incidents of one instance. |

## User tasks

| Method & path | Description |
|---|---|
| `GET /v1/tasks` | Open tasks by default. Filters: `assignee`, `candidateUser`, `candidateGroup`, `instanceId`, `definitionKey`, `state`, `limit`. |
| `GET /v1/tasks/{id}` | One task. |
| `POST /v1/tasks/{id}/claim` | `{user}` — fails if claimed by someone else. |
| `POST /v1/tasks/{id}/complete` | `{user?, variables?}` — merges variables, resumes the flow. `409` if already done. |

## Messages & signals

| Method & path | Description |
|---|---|
| `POST /v1/messages` | `{name, correlationKey?, variables?}`. Delivers to every matching waiting subscription (correlationKey matches the instance business key); if none matched, message start events spawn new instances. Returns `{activated: n}`. |
| `POST /v1/signals` | `{name, variables?}` — broadcast to all waiting subscriptions and signal start events. |

## External worker tasks

The built-in job-worker pattern: poll, lock, work, complete — from any
language.

| Method & path | Description |
|---|---|
| `POST /v1/external-tasks/fetch` | `{topic, workerId, maxTasks?, lockDuration?}` (Go duration, default `5m`) → locked tasks with variable snapshots. Expired locks are reclaimable. |
| `POST /v1/external-tasks/{id}/complete` | `{workerId, variables?}` |
| `POST /v1/external-tasks/{id}/fail` | `{workerId, message?, errorCode?}` — with `errorCode` throws a BPMN error; otherwise consumes a retry (exhausted ⇒ incident). |

## Incidents

| Method & path | Description |
|---|---|
| `GET /v1/incidents?resolved=false` | Everything that needs a human. |
| `POST /v1/incidents/{id}/resolve` | Mark resolved and resume the parked token with fresh retries. |

## Decisions (DMN)

| Method & path | Description |
|---|---|
| `POST /v1/decisions` | Body is raw DMN XML; registers all decision tables. |
| `GET /v1/decisions` | Registered decision IDs. |

## Observability

| Method & path | Description |
|---|---|
| `GET /healthz` | Liveness (no auth). |
| `GET /metrics` | Prometheus text format (`fliable_*` counters). |
| `GET /v1/stats` | Engine counters + runtime stats as JSON. |
| `GET /v1/events` | **Server-sent events** — the live engine event stream. Filters: `?instanceId=`, `?type=` prefix (e.g. `type=task.`). Event name = history type, data = the JSON event. |

```bash
curl -N "localhost:8080/v1/events?type=incident."   # live incident feed
```
