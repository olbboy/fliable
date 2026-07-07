# @fliable/sdk

Headless TypeScript SDK for the [Fliable](../../README.md) workflow & BPM
engine. **Framework-agnostic** — a typed client plus optional React hooks
that return *data and handlers only, no markup*. You bring the components:
[shadcn/ui](https://ui.shadcn.com), [Base UI](https://base-ui.com), Radix,
Headless UI, Vue, Svelte, Solid — anything.

Zero runtime dependencies. Works in the browser (`fetch` + `EventSource`)
and Node 18+.

```bash
npm install @fliable/sdk
```

## Client (any framework)

```ts
import { FliableClient } from "@fliable/sdk";

const fliable = new FliableClient({
  baseUrl: "http://localhost:8080",
  token: "<bearer token>",      // or apiKey; optional
  tenant: "acme",               // optional multi-tenant scope
});

// Start a process
const inst = await fliable.startInstance({
  definitionKey: "orderFulfillment",
  businessKey: "ord-1001",
  variables: { amount: 900, region: "EU" },
});

// Query the task inbox with a variable filter + pagination
const page = await fliable.listTasks({
  candidateGroup: "fraud-team",
  vars: [{ name: "amount", op: "gte", value: 500 }],
  limit: 20,
});

await fliable.completeTask(page.items[0].id, { approved: true }, "alice");

// Live event stream (browser)
const stop = fliable.events((ev) => console.log(ev.type, ev.elementId), {
  type: "task.",
});
```

The same client covers instances, tasks, messages/signals, incidents,
bulk operations, external workers, **AI agent jobs**, history and metrics —
plus **live instance migration**, **schema-driven forms** (`taskForm(id)`
returns the field schema and prefill variables; render with your own
inputs), **webhook channel management**, **secrets administration** and
**process analytics** (`processAnalytics()` → cycle times, incident and
throughput stats per definition).

## React hooks (headless)

The hooks hold no opinion about rendering. Wire the returned data into
your own components.

```tsx
import { FliableClient } from "@fliable/sdk";
import { FliableProvider, useTasks, useCompleteTask } from "@fliable/sdk/react";

const client = new FliableClient({ baseUrl: "/api" });

export function App() {
  return (
    <FliableProvider client={client}>
      <TaskInbox />
    </FliableProvider>
  );
}
```

### With shadcn/ui

```tsx
import { useTasks, useCompleteTask } from "@fliable/sdk/react";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardTitle, CardContent } from "@/components/ui/card";

function TaskInbox() {
  const { data, loading, refresh } = useTasks({ candidateGroup: "sales" }, { live: true });
  const complete = useCompleteTask();

  if (loading) return <p>Loading…</p>;
  return (
    <div className="grid gap-3">
      {data?.items.map((task) => (
        <Card key={task.id}>
          <CardHeader>
            <CardTitle>{task.name}</CardTitle>
          </CardHeader>
          <CardContent className="flex justify-between">
            <span className="text-muted-foreground">{task.assignee ?? "unassigned"}</span>
            <Button
              disabled={complete.pending}
              onClick={async () => {
                await complete.run(task.id, { approved: true }, "me");
                refresh();
              }}
            >
              Approve
            </Button>
          </CardContent>
        </Card>
      ))}
    </div>
  );
}
```

### With Base UI

```tsx
import { useInstances } from "@fliable/sdk/react";
import { Table } from "@base-ui-components/react/table";

function Instances() {
  const { data } = useInstances({ state: "active" }, { live: true });
  return (
    <Table.Root>
      <Table.Body>
        {data?.items.map((i) => (
          <Table.Row key={i.id}>
            <Table.Cell>{i.businessKey}</Table.Cell>
            <Table.Cell>{i.state}</Table.Cell>
          </Table.Row>
        ))}
      </Table.Body>
    </Table.Root>
  );
}
```

Because the data layer is fully headless and typed, the *same* hooks drop
into a Vue composable, a Svelte store, or a Solid resource with a thin
adapter — the client has no framework coupling.

## Hooks reference

| Hook | Returns |
|---|---|
| `useInstances(query, {live, pollMs})` | `{ data: Page<Instance>, loading, error, refresh }` |
| `useInstance(id, {live})` | `{ data: Instance, ... }` |
| `useTasks(query, {live})` | `{ data: Page<Task>, ... }` |
| `useTask(id)` | `{ data: Task, ... }` |
| `useStartInstance()` | `{ run, pending, error }` |
| `useClaimTask()` / `useCompleteTask()` | `{ run, pending, error }` |
| `useEventStream(onEvent, filter)` | subscribes to live SSE |

`{ live: true }` refetches on matching server-sent events; `{ pollMs }`
polls on an interval. Both are optional.

## OpenAPI

The server serves an OpenAPI 3.1 document at `/openapi.json` and a
dependency-free reference at `/docs`. Point any generator at it to build a
client for another language.
