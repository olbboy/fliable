// Fliable TypeScript client — zero dependencies, framework-agnostic.
// Works in the browser (fetch + EventSource) and Node 18+ (global fetch).
// Any UI framework — shadcn, Base UI, React, Vue, Svelte, Solid — can build
// on this typed data layer.

import type {
  AgentJob,
  AgentToolCall,
  AgentUsage,
  Definition,
  ExternalTask,
  FormDefinition,
  HistoryEvent,
  Incident,
  Instance,
  InstanceQuery,
  MigrationPlan,
  MigrationReport,
  Page,
  ProcessStats,
  Task,
  TaskQuery,
  VarFilter,
  WebhookChannel,
} from "./types";

export interface FliableOptions {
  /** Base URL of the Fliable server, e.g. "http://localhost:8080". */
  baseUrl: string;
  /** Optional X-Api-Key. */
  apiKey?: string;
  /** Optional bearer token (from MintToken). */
  token?: string;
  /** Optional tenant scope (X-Tenant-Id). */
  tenant?: string;
  /** Override fetch (tests, custom agents). Defaults to global fetch. */
  fetch?: typeof fetch;
}

export class FliableError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
    this.name = "FliableError";
  }
}

export class FliableClient {
  private base: string;
  private f: typeof fetch;

  constructor(private opts: FliableOptions) {
    this.base = opts.baseUrl.replace(/\/$/, "");
    this.f = opts.fetch ?? fetch;
  }

  private headers(json = true): Record<string, string> {
    const h: Record<string, string> = {};
    if (json) h["Content-Type"] = "application/json";
    if (this.opts.apiKey) h["X-Api-Key"] = this.opts.apiKey;
    if (this.opts.token) h["Authorization"] = `Bearer ${this.opts.token}`;
    if (this.opts.tenant) h["X-Tenant-Id"] = this.opts.tenant;
    return h;
  }

  private async req<T>(method: string, path: string, body?: unknown, raw?: BodyInit): Promise<T> {
    const res = await this.f(this.base + path, {
      method,
      headers: raw ? this.headers(false) : this.headers(),
      body: raw ?? (body === undefined ? undefined : JSON.stringify(body)),
    });
    const text = await res.text();
    const data = text ? JSON.parse(text) : undefined;
    if (!res.ok) {
      throw new FliableError(res.status, data?.error ?? res.statusText);
    }
    return data as T;
  }

  // ---- definitions ----
  deployBPMN(xml: string, name?: string): Promise<Definition> {
    const q = name ? `?name=${encodeURIComponent(name)}` : "";
    return this.req("POST", `/v1/definitions${q}`, undefined, xml);
  }
  deployDMN(xml: string): Promise<{ decisions: string[] }> {
    return this.req("POST", `/v1/decisions`, undefined, xml);
  }
  listDefinitions(): Promise<Definition[]> {
    return this.req("GET", `/v1/definitions`);
  }

  // ---- instances ----
  startInstance(req: {
    definitionKey?: string;
    definitionId?: string;
    businessKey?: string;
    variables?: Record<string, unknown>;
  }): Promise<Instance> {
    return this.req("POST", `/v1/instances`, req);
  }
  getInstance(id: string): Promise<Instance> {
    return this.req("GET", `/v1/instances/${id}`);
  }
  listInstances(q: InstanceQuery = {}): Promise<Page<Instance>> {
    return this.req("GET", `/v1/instances${instanceQuery(q)}`);
  }
  cancelInstance(id: string, reason?: string): Promise<void> {
    const r = reason ? `?reason=${encodeURIComponent(reason)}` : "";
    return this.req("DELETE", `/v1/instances/${id}${r}`);
  }
  suspendInstance(id: string): Promise<void> {
    return this.req("POST", `/v1/instances/${id}/suspend`);
  }
  resumeInstance(id: string): Promise<void> {
    return this.req("POST", `/v1/instances/${id}/resume`);
  }
  setVariables(id: string, vars: Record<string, unknown>): Promise<void> {
    return this.req("PUT", `/v1/instances/${id}/variables`, vars);
  }
  history(id: string, opts: { type?: string; afterSeq?: number } = {}): Promise<HistoryEvent[]> {
    const p = new URLSearchParams();
    if (opts.type) p.set("type", opts.type);
    if (opts.afterSeq) p.set("afterSeq", String(opts.afterSeq));
    const q = p.toString();
    return this.req("GET", `/v1/instances/${id}/history${q ? "?" + q : ""}`);
  }
  instanceIncidents(id: string): Promise<Incident[]> {
    return this.req("GET", `/v1/instances/${id}/incidents`);
  }

  // ---- tasks ----
  listTasks(q: TaskQuery = {}): Promise<Page<Task>> {
    return this.req("GET", `/v1/tasks${taskQuery(q)}`);
  }
  getTask(id: string): Promise<Task> {
    return this.req("GET", `/v1/tasks/${id}`);
  }
  claimTask(id: string, user: string): Promise<void> {
    return this.req("POST", `/v1/tasks/${id}/claim`, { user });
  }
  completeTask(id: string, vars?: Record<string, unknown>, user?: string): Promise<void> {
    return this.req("POST", `/v1/tasks/${id}/complete`, { user, variables: vars });
  }

  // ---- messages & signals ----
  correlateMessage(name: string, correlationKey?: string, variables?: Record<string, unknown>): Promise<{ activated: number }> {
    return this.req("POST", `/v1/messages`, { name, correlationKey, variables });
  }
  broadcastSignal(name: string, variables?: Record<string, unknown>): Promise<{ activated: number }> {
    return this.req("POST", `/v1/signals`, { name, variables });
  }

  // ---- incidents ----
  listIncidents(resolved?: boolean): Promise<Incident[]> {
    const q = resolved === undefined ? "" : `?resolved=${resolved}`;
    return this.req("GET", `/v1/incidents${q}`);
  }
  resolveIncident(id: string): Promise<void> {
    return this.req("POST", `/v1/incidents/${id}/resolve`);
  }

  // ---- bulk ----
  batch(op: "cancel" | "suspend" | "resume" | "resolve-incidents", ids: string[], reason?: string) {
    return this.req<{ succeeded: number; failed: number; results: { id: string; ok: boolean; error?: string }[] }>(
      "POST",
      `/v1/batch/${op}`,
      { ids, reason },
    );
  }

  // ---- external workers ----
  fetchExternalTasks(topic: string, workerId: string, maxTasks = 10, lockDuration = "5m"): Promise<ExternalTask[]> {
    return this.req("POST", `/v1/external-tasks/fetch`, { topic, workerId, maxTasks, lockDuration });
  }
  completeExternalTask(id: string, workerId: string, variables?: Record<string, unknown>): Promise<void> {
    return this.req("POST", `/v1/external-tasks/${id}/complete`, { workerId, variables });
  }
  failExternalTask(id: string, workerId: string, message?: string, errorCode?: string): Promise<void> {
    return this.req("POST", `/v1/external-tasks/${id}/fail`, { workerId, message, errorCode });
  }

  // ---- AI agent workers ----
  fetchAgentJobs(topic: string, workerId: string, maxJobs = 5, lockDuration = "5m"): Promise<AgentJob[]> {
    return this.req("POST", `/v1/agent-jobs/fetch`, { topic, workerId, maxJobs, lockDuration });
  }
  completeAgentJob(
    id: string,
    workerId: string,
    result: { output?: Record<string, unknown>; text?: string; toolCalls?: AgentToolCall[]; usage?: AgentUsage },
  ): Promise<void> {
    return this.req("POST", `/v1/agent-jobs/${id}/complete`, { workerId, ...result });
  }
  failAgentJob(id: string, workerId: string, message?: string, errorCode?: string): Promise<void> {
    return this.req("POST", `/v1/agent-jobs/${id}/fail`, { workerId, message, errorCode });
  }

  // ---- migration ----
  /** Validate (dryRun) or apply a live migration onto another definition version. */
  migrateInstance(id: string, plan: MigrationPlan): Promise<MigrationReport> {
    return this.req("POST", `/v1/instances/${id}/migrate`, plan);
  }

  // ---- forms (headless: schema in, your components out) ----
  deployForm(def: FormDefinition): Promise<FormDefinition> {
    return this.req("POST", `/v1/forms`, def);
  }
  listForms(): Promise<FormDefinition[]> {
    return this.req("GET", `/v1/forms`);
  }
  getForm(key: string): Promise<FormDefinition> {
    return this.req("GET", `/v1/forms/${encodeURIComponent(key)}`);
  }
  deleteForm(key: string): Promise<void> {
    return this.req("DELETE", `/v1/forms/${encodeURIComponent(key)}`);
  }
  /** The form bound to a task plus current variables for prefill. */
  taskForm(taskId: string): Promise<{ form: FormDefinition; variables: Record<string, unknown> }> {
    return this.req("GET", `/v1/tasks/${taskId}/form`);
  }

  // ---- secrets (admin) ----
  listSecretNames(): Promise<{ names: string[] }> {
    return this.req("GET", `/v1/secrets`);
  }
  putSecret(name: string, value: string): Promise<void> {
    return this.req("PUT", `/v1/secrets/${encodeURIComponent(name)}`, { value });
  }
  deleteSecret(name: string): Promise<void> {
    return this.req("DELETE", `/v1/secrets/${encodeURIComponent(name)}`);
  }

  // ---- webhook channels (admin) ----
  putWebhook(name: string, ch: Omit<WebhookChannel, "name">): Promise<WebhookChannel> {
    return this.req("PUT", `/v1/webhooks/${encodeURIComponent(name)}`, ch);
  }
  listWebhooks(): Promise<WebhookChannel[]> {
    return this.req("GET", `/v1/webhooks`);
  }
  deleteWebhook(name: string): Promise<void> {
    return this.req("DELETE", `/v1/webhooks/${encodeURIComponent(name)}`);
  }

  // ---- observability ----
  stats(): Promise<Record<string, unknown>> {
    return this.req("GET", `/v1/stats`);
  }
  /** Per-process cycle times, state counts, incidents and 24h throughput. */
  processAnalytics(): Promise<ProcessStats[]> {
    return this.req("GET", `/v1/analytics/processes`);
  }

  /**
   * Subscribe to the live server-sent event stream. Returns an unsubscribe
   * function. Browser-only (uses EventSource); for Node use a polling hook
   * or an SSE polyfill.
   */
  events(
    handler: (ev: HistoryEvent) => void,
    filter: { instanceId?: string; type?: string } = {},
  ): () => void {
    const p = new URLSearchParams();
    if (filter.instanceId) p.set("instanceId", filter.instanceId);
    if (filter.type) p.set("type", filter.type);
    const url = `${this.base}/v1/events${p.toString() ? "?" + p.toString() : ""}`;
    const es = new EventSource(url, { withCredentials: !!this.opts.token });
    es.onmessage = (e) => {
      try {
        handler(JSON.parse(e.data));
      } catch {
        /* ignore malformed frame */
      }
    };
    return () => es.close();
  }
}

function varParams(p: URLSearchParams, vars?: VarFilter[]) {
  for (const v of vars ?? []) {
    p.append("var", v.op === "exists" ? `${v.name}:exists` : `${v.name}:${v.op}:${v.value}`);
  }
}

function instanceQuery(q: InstanceQuery): string {
  const p = new URLSearchParams();
  if (q.definitionKey) p.set("definitionKey", q.definitionKey);
  if (q.businessKey) p.set("businessKey", q.businessKey);
  if (q.state) p.set("state", q.state);
  if (q.startedAfter) p.set("startedAfter", q.startedAfter);
  if (q.startedBefore) p.set("startedBefore", q.startedBefore);
  if (q.cursor) p.set("cursor", q.cursor);
  if (q.desc) p.set("desc", "true");
  if (q.limit) p.set("limit", String(q.limit));
  varParams(p, q.vars);
  const s = p.toString();
  return s ? "?" + s : "";
}

function taskQuery(q: TaskQuery): string {
  const p = new URLSearchParams();
  if (q.instanceId) p.set("instanceId", q.instanceId);
  if (q.assignee) p.set("assignee", q.assignee);
  if (q.unassigned) p.set("unassigned", "true");
  if (q.candidateUser) p.set("candidateUser", q.candidateUser);
  if (q.candidateGroup) p.set("candidateGroup", q.candidateGroup);
  if (q.definitionKey) p.set("definitionKey", q.definitionKey);
  if (q.state) p.set("state", q.state);
  if (q.dueBefore) p.set("dueBefore", q.dueBefore);
  if (q.cursor) p.set("cursor", q.cursor);
  if (q.desc) p.set("desc", "true");
  if (q.limit) p.set("limit", String(q.limit));
  varParams(p, q.vars);
  const s = p.toString();
  return s ? "?" + s : "";
}
