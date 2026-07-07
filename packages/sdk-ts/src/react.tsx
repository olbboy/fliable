// Headless React hooks for Fliable. They return data and handlers only —
// no markup, no styling — so you wire them into your own components
// (shadcn/ui, Base UI, Radix, Headless UI, or anything else). React is a
// peer dependency.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
} from "react";
import { FliableClient } from "./client";
import type { HistoryEvent, Instance, InstanceQuery, Page, Task, TaskQuery } from "./types";

const Ctx = createContext<FliableClient | null>(null);

/** Provide a FliableClient to the hook tree. */
export function FliableProvider(props: { client: FliableClient; children: React.ReactNode }) {
  return <Ctx.Provider value={props.client}>{props.children}</Ctx.Provider>;
}

/** Access the configured client. */
export function useFliable(): FliableClient {
  const c = useContext(Ctx);
  if (!c) throw new Error("useFliable must be used within <FliableProvider>");
  return c;
}

export interface Query<T> {
  data: T | undefined;
  loading: boolean;
  error: Error | undefined;
  refresh: () => void;
}

interface LiveOpts {
  /** Refetch when a matching SSE event arrives (browser only). */
  live?: boolean;
  /** Poll interval in ms (0 disables). */
  pollMs?: number;
}

function useAsync<T>(fn: () => Promise<T>, deps: unknown[], live?: { instanceId?: string; type?: string } & LiveOpts) {
  const client = useFliable();
  const [state, setState] = useState<Query<T>>({ data: undefined, loading: true, error: undefined, refresh: () => {} });
  const fnRef = useRef(fn);
  fnRef.current = fn;

  const load = useCallback(() => {
    setState((s) => ({ ...s, loading: true }));
    fnRef.current()
      .then((data) => setState({ data, loading: false, error: undefined, refresh: load }))
      .catch((error) => setState((s) => ({ ...s, loading: false, error, refresh: load })));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  // Live refresh via SSE.
  useEffect(() => {
    if (!live?.live) return;
    const unsub = client.events(() => load(), { instanceId: live.instanceId, type: live.type });
    return unsub;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [live?.live, live?.instanceId, live?.type]);

  // Polling.
  useEffect(() => {
    if (!live?.pollMs) return;
    const t = setInterval(load, live.pollMs);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [live?.pollMs]);

  return { ...state, refresh: load };
}

/** List/query process instances, with optional live refresh. */
export function useInstances(query: InstanceQuery = {}, opts: LiveOpts = {}): Query<Page<Instance>> {
  const client = useFliable();
  return useAsync(() => client.listInstances(query), [JSON.stringify(query)], { ...opts, type: "instance." });
}

/** Load one instance, live-refreshing on its own events. */
export function useInstance(id: string | undefined, opts: LiveOpts = {}): Query<Instance> {
  const client = useFliable();
  return useAsync(
    () => (id ? client.getInstance(id) : Promise.reject(new Error("no instance id"))),
    [id],
    { ...opts, instanceId: id },
  );
}

/** Query the user-task inbox, with optional live refresh on task events. */
export function useTasks(query: TaskQuery = {}, opts: LiveOpts = {}): Query<Page<Task>> {
  const client = useFliable();
  return useAsync(() => client.listTasks(query), [JSON.stringify(query)], { ...opts, type: "task." });
}

/** Load one task. */
export function useTask(id: string | undefined): Query<Task> {
  const client = useFliable();
  return useAsync(() => (id ? client.getTask(id) : Promise.reject(new Error("no task id"))), [id]);
}

export interface Mutation<Args extends unknown[]> {
  run: (...args: Args) => Promise<void>;
  pending: boolean;
  error: Error | undefined;
}

function useMutation<Args extends unknown[]>(fn: (...args: Args) => Promise<unknown>): Mutation<Args> {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<Error | undefined>();
  const run = useCallback(
    async (...args: Args) => {
      setPending(true);
      setError(undefined);
      try {
        await fn(...args);
      } catch (e) {
        setError(e as Error);
        throw e;
      } finally {
        setPending(false);
      }
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  );
  return { run, pending, error };
}

/** Start a process instance. */
export function useStartInstance() {
  const client = useFliable();
  return useMutation((req: Parameters<FliableClient["startInstance"]>[0]) => client.startInstance(req));
}

/** Complete a user task (claim + complete are separate hooks). */
export function useCompleteTask() {
  const client = useFliable();
  return useMutation((id: string, vars?: Record<string, unknown>, user?: string) => client.completeTask(id, vars, user));
}

/** Claim a user task. */
export function useClaimTask() {
  const client = useFliable();
  return useMutation((id: string, user: string) => client.claimTask(id, user));
}

/** Subscribe to the live engine event stream (browser only). */
export function useEventStream(
  onEvent: (ev: HistoryEvent) => void,
  filter: { instanceId?: string; type?: string } = {},
) {
  const client = useFliable();
  const cb = useRef(onEvent);
  cb.current = onEvent;
  useEffect(() => {
    return client.events((ev) => cb.current(ev), filter);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filter.instanceId, filter.type]);
}
