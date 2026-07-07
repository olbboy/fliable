package store

import (
	"errors"
	"time"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("store: not found")

// InstanceFilter narrows ListInstances. Zero-value fields are ignored.
// Results are ordered by ID (time-sortable); Cursor holds the last ID from
// the previous page for stable keyset pagination.
type InstanceFilter struct {
	TenantID      string
	DefinitionKey string
	DefinitionID  string
	BusinessKey   string
	State         InstanceState
	ParentID      string
	Vars          []VarMatch
	StartedAfter  time.Time
	StartedBefore time.Time
	EndedAfter    time.Time
	EndedBefore   time.Time
	Cursor        string
	Desc          bool
	Limit         int
}

// TaskFilter narrows ListTasks. Vars match against the task's instance
// variables. Zero-value fields are ignored.
type TaskFilter struct {
	TenantID       string
	InstanceID     string
	Assignee       string
	Unassigned     bool
	CandidateUser  string
	CandidateGroup string
	State          TaskState
	DefinitionKey  string
	ElementID      string
	Vars           []VarMatch
	CreatedAfter   time.Time
	CreatedBefore  time.Time
	DueBefore      time.Time
	Cursor         string
	Desc           bool
	Limit          int
}

// SubscriptionFilter narrows ListSubscriptions.
type SubscriptionFilter struct {
	Kind           SubscriptionKind
	Name           string
	CorrelationKey string
	InstanceID     string
	IsStart        *bool
}

// IncidentFilter narrows ListIncidents.
type IncidentFilter struct {
	InstanceID string
	Resolved   *bool
	Limit      int
}

// HistoryFilter narrows ListHistory.
type HistoryFilter struct {
	Type     string
	AfterSeq int64
	Limit    int
}

// Store is the persistence contract the engine runs against. Individual
// methods must be atomic and safe for concurrent use; cross-record
// consistency is the engine's responsibility (it serializes work per
// instance).
type Store interface {
	// Definitions.
	PutDefinition(d *Definition) error
	GetDefinition(id string) (*Definition, error)
	LatestDefinition(key string) (*Definition, error)
	// LatestDefinitionForTenant returns the newest version of key within a
	// tenant; tenantID "" is the default (untenanted) space.
	LatestDefinitionForTenant(tenantID, key string) (*Definition, error)
	ListDefinitions(latestOnly bool) ([]*Definition, error)

	// Instances.
	PutInstance(inst *Instance) error
	GetInstance(id string) (*Instance, error)
	ListInstances(f InstanceFilter) ([]*Instance, error)

	// User tasks.
	PutTask(t *Task) error
	GetTask(id string) (*Task, error)
	ListTasks(f TaskFilter) ([]*Task, error)

	// Jobs.
	PutJob(j *Job) error
	GetJob(id string) (*Job, error)
	DeleteJob(id string) error
	// DueJobs atomically claims and removes up to limit jobs due at or
	// before now, returning them for execution.
	DueJobs(now time.Time, limit int) ([]*Job, error)
	ListJobs(instanceID string) ([]*Job, error)

	// External worker tasks.
	PutExternalTask(t *ExternalTask) error
	GetExternalTask(id string) (*ExternalTask, error)
	// FetchAndLockExternalTasks atomically locks up to limit pending tasks
	// on topic for workerID until the given time.
	FetchAndLockExternalTasks(topic, workerID string, until, now time.Time, limit int) ([]*ExternalTask, error)
	ListExternalTasks(instanceID string) ([]*ExternalTask, error)

	// Event subscriptions.
	PutSubscription(s *Subscription) error
	DeleteSubscription(id string) error
	ListSubscriptions(f SubscriptionFilter) ([]*Subscription, error)

	// Incidents.
	PutIncident(i *Incident) error
	GetIncident(id string) (*Incident, error)
	ListIncidents(f IncidentFilter) ([]*Incident, error)

	// History. AppendHistory assigns ev.Seq.
	AppendHistory(ev *HistoryEvent) error
	ListHistory(instanceID string, f HistoryFilter) ([]*HistoryEvent, error)

	// Close releases resources (flushes journals, closes files).
	Close() error
}
