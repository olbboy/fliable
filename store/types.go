// Package store defines Fliable's persistence records and the Store
// interface the engine runs against, together with two built-in
// implementations: a lock-protected in-memory store and a durable
// journal store (append-only JSON log with snapshot compaction) that
// gives single-binary durability with zero external dependencies.
package store

import "time"

// Definition is a deployed process definition. Deploying the same Key again
// creates a new Version; running instances keep executing their original
// version (safe hot deployment).
type Definition struct {
	ID         string    `json:"id"`
	Key        string    `json:"key"`
	Version    int       `json:"version"`
	Name       string    `json:"name"`
	XML        []byte    `json:"xml"`
	DeployedAt time.Time `json:"deployedAt"`
}

// InstanceState is the lifecycle state of a process instance.
type InstanceState string

// Instance lifecycle states.
const (
	InstanceActive     InstanceState = "active"
	InstanceCompleted  InstanceState = "completed"
	InstanceTerminated InstanceState = "terminated"
)

// TokenState describes what a token is currently doing.
type TokenState string

// Token states. A token is either ready to advance or parked on a wait
// state; WaitRef points at the record it is waiting for.
const (
	TokenActive       TokenState = "active"
	TokenWaitTask     TokenState = "waitUserTask"
	TokenWaitTimer    TokenState = "waitTimer"
	TokenWaitMessage  TokenState = "waitMessage"
	TokenWaitSignal   TokenState = "waitSignal"
	TokenWaitExternal TokenState = "waitExternal"
	TokenWaitChild    TokenState = "waitChildInstance"
	TokenWaitMulti    TokenState = "waitMultiInstance"
	TokenJoining      TokenState = "joining"
	TokenWaitRetry    TokenState = "waitRetry"
	TokenWaitEvents   TokenState = "waitEvents" // event-based gateway
)

// Token is an execution pointer inside a process instance.
type Token struct {
	ID        string     `json:"id"`
	ElementID string     `json:"elementId"`
	State     TokenState `json:"state"`
	// ScopePath is the chain of sub-process element IDs from the process
	// root down to the scope containing ElementID.
	ScopePath []string `json:"scopePath,omitempty"`
	// ArrivedFlow is the sequence flow the token arrived through (used by
	// joining gateways).
	ArrivedFlow string `json:"arrivedFlow,omitempty"`
	// WaitRef references the task/job/subscription/child-instance the
	// token waits for.
	WaitRef string `json:"waitRef,omitempty"`
	// Parent is the ID of the token that spawned this one (multi-instance
	// children and boundary-event scopes).
	Parent string `json:"parent,omitempty"`
	// LocalVars are token-scoped variables (multi-instance element
	// variable, loop counters). They shadow instance variables.
	LocalVars map[string]any `json:"localVars,omitempty"`
}

// MultiInstanceState tracks one in-flight multi-instance activity.
type MultiInstanceState struct {
	TokenID   string `json:"tokenId"` // the parent token waiting on the loop
	Total     int    `json:"total"`
	Completed int    `json:"completed"`
	NextIndex int    `json:"nextIndex"` // sequential: next item to start
	Items     []any  `json:"items,omitempty"`
	Outputs   []any  `json:"outputs,omitempty"`
}

// Instance is a running or finished process instance.
type Instance struct {
	ID            string        `json:"id"`
	DefinitionID  string        `json:"definitionId"`
	DefinitionKey string        `json:"definitionKey"`
	BusinessKey   string        `json:"businessKey,omitempty"`
	State         InstanceState `json:"state"`

	// ParentID/ParentTokenID link a call-activity child to its parent.
	ParentID      string `json:"parentId,omitempty"`
	ParentTokenID string `json:"parentTokenId,omitempty"`

	Variables map[string]any    `json:"variables"`
	Tokens    map[string]*Token `json:"tokens"`
	// Multi holds multi-instance loop state keyed by parent token ID.
	Multi map[string]*MultiInstanceState `json:"multi,omitempty"`

	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitzero"`
	// EndElement records which end/terminate element finished the instance.
	EndElement string `json:"endElement,omitempty"`
	// Rev is a monotonically increasing revision used for optimistic
	// concurrency by stores that need it.
	Rev int64 `json:"rev"`
}

// TaskState is the lifecycle state of a user task.
type TaskState string

// User task lifecycle states.
const (
	TaskCreated   TaskState = "created"
	TaskCompleted TaskState = "completed"
	TaskCanceled  TaskState = "canceled"
)

// Task is a user task waiting for a human.
type Task struct {
	ID              string    `json:"id"`
	InstanceID      string    `json:"instanceId"`
	TokenID         string    `json:"tokenId"`
	ElementID       string    `json:"elementId"`
	DefinitionKey   string    `json:"definitionKey"`
	Name            string    `json:"name"`
	State           TaskState `json:"state"`
	Assignee        string    `json:"assignee,omitempty"`
	CandidateUsers  []string  `json:"candidateUsers,omitempty"`
	CandidateGroups []string  `json:"candidateGroups,omitempty"`
	FormKey         string    `json:"formKey,omitempty"`
	Priority        int       `json:"priority,omitempty"`
	DueAt           time.Time `json:"dueAt,omitzero"`
	CreatedAt       time.Time `json:"createdAt"`
	CompletedAt     time.Time `json:"completedAt,omitzero"`
	CompletedBy     string    `json:"completedBy,omitempty"`
}

// JobKind classifies background jobs.
type JobKind string

// Job kinds.
const (
	JobTimer JobKind = "timer"
	JobAsync JobKind = "async"
	JobRetry JobKind = "retry"
)

// Job is a due-at-some-time unit of engine work (timer fire, async
// continuation, service retry).
type Job struct {
	ID         string  `json:"id"`
	Kind       JobKind `json:"kind"`
	InstanceID string  `json:"instanceId"`
	// DefinitionKey is set instead of InstanceID for timer-start jobs that
	// create a new instance when they fire.
	DefinitionKey string    `json:"definitionKey,omitempty"`
	TokenID       string    `json:"tokenId"`
	ElementID     string    `json:"elementId"`
	DueAt         time.Time `json:"dueAt"`
	// Repeats is the remaining repetition count for cycle timers
	// (-1 = unbounded).
	Repeats  int           `json:"repeats,omitempty"`
	Interval time.Duration `json:"interval,omitempty"`
	// Retries counts remaining attempts before the job dead-letters into
	// an incident.
	Retries   int       `json:"retries"`
	CreatedAt time.Time `json:"createdAt"`
}

// ExternalTaskState is the lifecycle of an external worker task.
type ExternalTaskState string

// External task states.
const (
	ExternalPending ExternalTaskState = "pending"
	ExternalDone    ExternalTaskState = "done"
	ExternalFailed  ExternalTaskState = "failed"
)

// ExternalTask is a unit of work fetched and completed by polling workers
// over the API (the Zeebe-style job worker pattern, built in).
type ExternalTask struct {
	ID         string            `json:"id"`
	InstanceID string            `json:"instanceId"`
	TokenID    string            `json:"tokenId"`
	ElementID  string            `json:"elementId"`
	Topic      string            `json:"topic"`
	State      ExternalTaskState `json:"state"`
	Variables  map[string]any    `json:"variables,omitempty"`
	LockedBy   string            `json:"lockedBy,omitempty"`
	LockUntil  time.Time         `json:"lockUntil,omitzero"`
	Retries    int               `json:"retries"`
	CreatedAt  time.Time         `json:"createdAt"`
}

// SubscriptionKind classifies event subscriptions.
type SubscriptionKind string

// Subscription kinds.
const (
	SubMessage SubscriptionKind = "message"
	SubSignal  SubscriptionKind = "signal"
)

// Subscription registers interest in a message or signal. Start
// subscriptions (IsStart) create new instances; the rest resume tokens.
type Subscription struct {
	ID             string           `json:"id"`
	Kind           SubscriptionKind `json:"kind"`
	Name           string           `json:"name"`
	CorrelationKey string           `json:"correlationKey,omitempty"`
	InstanceID     string           `json:"instanceId,omitempty"`
	TokenID        string           `json:"tokenId,omitempty"`
	ElementID      string           `json:"elementId,omitempty"`
	IsStart        bool             `json:"isStart,omitempty"`
	DefinitionKey  string           `json:"definitionKey,omitempty"`
	CreatedAt      time.Time        `json:"createdAt"`
}

// Incident is a persistent error requiring human or programmatic
// intervention (dead-lettered job, failed external task, unhandled BPMN
// error). Instances never silently disappear on failure.
type Incident struct {
	ID         string    `json:"id"`
	InstanceID string    `json:"instanceId"`
	TokenID    string    `json:"tokenId"`
	ElementID  string    `json:"elementId"`
	Message    string    `json:"message"`
	Code       string    `json:"code,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	Resolved   bool      `json:"resolved,omitempty"`
	ResolvedAt time.Time `json:"resolvedAt,omitzero"`
}

// HistoryEvent is one immutable entry in an instance's event-sourced audit
// trail.
type HistoryEvent struct {
	Seq        int64          `json:"seq"`
	InstanceID string         `json:"instanceId"`
	Time       time.Time      `json:"time"`
	Type       string         `json:"type"`
	ElementID  string         `json:"elementId,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
}

// History event types emitted by the engine.
const (
	HistInstanceStarted    = "instance.started"
	HistInstanceCompleted  = "instance.completed"
	HistInstanceTerminated = "instance.terminated"
	HistElementActivated   = "element.activated"
	HistElementCompleted   = "element.completed"
	HistTaskCreated        = "task.created"
	HistTaskCompleted      = "task.completed"
	HistTaskCanceled       = "task.canceled"
	HistVariablesSet       = "variables.set"
	HistTimerScheduled     = "timer.scheduled"
	HistTimerFired         = "timer.fired"
	HistMessageReceived    = "message.received"
	HistSignalReceived     = "signal.received"
	HistErrorThrown        = "error.thrown"
	HistIncidentCreated    = "incident.created"
	HistIncidentResolved   = "incident.resolved"
)
