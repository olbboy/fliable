// Types mirroring the Fliable REST API JSON shapes. Hand-maintained to
// stay dependency-free; regenerate from /openapi.json if you prefer
// codegen.

export type InstanceState = "active" | "completed" | "terminated";
export type TaskState = "created" | "completed" | "canceled";

export interface Definition {
  id: string;
  tenantId?: string;
  key: string;
  version: number;
  name: string;
  deployedAt: string;
}

export interface Token {
  id: string;
  elementId: string;
  state: string;
  waitRef?: string;
}

export interface Instance {
  id: string;
  tenantId?: string;
  definitionId: string;
  definitionKey: string;
  businessKey?: string;
  state: InstanceState;
  suspended?: boolean;
  variables: Record<string, unknown>;
  tokens: Record<string, Token>;
  startedAt: string;
  endedAt?: string;
  endElement?: string;
}

export interface Task {
  id: string;
  tenantId?: string;
  instanceId: string;
  elementId: string;
  definitionKey: string;
  name: string;
  state: TaskState;
  assignee?: string;
  candidateUsers?: string[];
  candidateGroups?: string[];
  formKey?: string;
  priority?: number;
  dueAt?: string;
  createdAt: string;
  completedAt?: string;
  completedBy?: string;
}

export interface Incident {
  id: string;
  instanceId: string;
  tokenId: string;
  elementId: string;
  message: string;
  code?: string;
  createdAt: string;
  resolved?: boolean;
}

export interface HistoryEvent {
  seq: number;
  instanceId: string;
  time: string;
  type: string;
  elementId?: string;
  detail?: Record<string, unknown>;
}

export interface AgentToolSpec {
  name: string;
  description?: string;
  schema?: string;
  mcpServer?: string;
}

export interface AgentJob {
  id: string;
  tenantId?: string;
  instanceId: string;
  tokenId: string;
  elementId: string;
  agent?: string;
  topic: string;
  prompt: string;
  system?: string;
  tools?: AgentToolSpec[];
  model?: string;
  effort?: string;
  maxTokens?: number;
  variables?: Record<string, unknown>;
  state: string;
  retries: number;
  createdAt: string;
}

export interface ExternalTask {
  id: string;
  instanceId: string;
  elementId: string;
  topic: string;
  state: string;
  variables?: Record<string, unknown>;
  retries: number;
}

export interface AgentToolCall {
  tool: string;
  input?: Record<string, unknown>;
  result?: string;
  error?: string;
}

export interface AgentUsage {
  inputTokens?: number;
  outputTokens?: number;
  costUsd?: number;
  model?: string;
}

export interface Page<T> {
  items: T[];
  count: number;
  nextCursor?: string;
}

/** VarFilter builds a ?var=name:op:value predicate. */
export interface VarFilter {
  name: string;
  op: "eq" | "ne" | "lt" | "lte" | "gt" | "gte" | "contains" | "exists";
  value?: string | number | boolean;
}

export interface InstanceQuery {
  definitionKey?: string;
  businessKey?: string;
  state?: InstanceState;
  vars?: VarFilter[];
  startedAfter?: string;
  startedBefore?: string;
  cursor?: string;
  desc?: boolean;
  limit?: number;
}

export interface TaskQuery {
  instanceId?: string;
  assignee?: string;
  unassigned?: boolean;
  candidateUser?: string;
  candidateGroup?: string;
  definitionKey?: string;
  state?: TaskState;
  vars?: VarFilter[];
  dueBefore?: string;
  cursor?: string;
  desc?: boolean;
  limit?: number;
}

// ---- live migration (D2) ----

export interface MigrationPlan {
  targetDefinitionId: string;
  /** Element ID renames, old -> new (identity by default). */
  activityMap?: Record<string, string>;
  /** Variable derivations: name -> expression over pre-migration vars. */
  varTransforms?: Record<string, string>;
  /** Validate and report without applying. */
  dryRun?: boolean;
}

export interface TokenMove {
  tokenId: string;
  from: string;
  to: string;
  state: string;
}

export interface MigrationReport {
  instanceId: string;
  targetDefinitionId: string;
  tokenMoves: TokenMove[];
  issues?: string[];
  applied: boolean;
}

// ---- forms (headless) ----

export type FormFieldType = "string" | "text" | "number" | "boolean" | "enum" | "date";

export interface FormField {
  id: string;
  label?: string;
  type: FormFieldType;
  required?: boolean;
  options?: string[];
  min?: number;
  max?: number;
  pattern?: string;
  default?: unknown;
  /** Expression over the submitted variables; false hides the field. */
  visibleIf?: string;
  readOnly?: boolean;
}

export interface FormDefinition {
  key: string;
  name?: string;
  fields: FormField[];
}

// ---- webhook channels ----

export interface WebhookChannel {
  name: string;
  kind?: "message" | "signal";
  event: string;
  correlationExpr?: string;
  dedupeExpr?: string;
  secret?: string;
  vars?: Record<string, string>;
  tenantId?: string;
}

// ---- analytics ----

export interface ProcessStats {
  definitionKey: string;
  active: number;
  completed: number;
  terminated: number;
  suspended: number;
  openIncidents: number;
  openTasks: number;
  avgDurationMs?: number;
  p50DurationMs?: number;
  p95DurationMs?: number;
  completedLast24h: number;
}
