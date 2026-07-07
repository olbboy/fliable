package engine

import (
	"sync/atomic"
	"time"
)

// Metrics holds engine counters, updated lock-free on hot paths.
type Metrics struct {
	DefinitionsDeployed atomic.Int64
	InstancesStarted    atomic.Int64
	InstancesCompleted  atomic.Int64
	InstancesTerminated atomic.Int64
	TasksCreated        atomic.Int64
	TasksCompleted      atomic.Int64
	ServiceTasksRun     atomic.Int64
	ExternalCreated     atomic.Int64
	ExternalCompleted   atomic.Int64
	TimersFired         atomic.Int64
	JobsExecuted        atomic.Int64
	MessagesCorrelated  atomic.Int64
	DecisionsEvaluated  atomic.Int64
	IncidentsCreated    atomic.Int64
	AgentInvocations    atomic.Int64
	AgentInputTokens    atomic.Int64
	AgentOutputTokens   atomic.Int64
}

// MetricsSnapshot is a point-in-time copy of the counters.
type MetricsSnapshot struct {
	DefinitionsDeployed int64 `json:"definitionsDeployed"`
	InstancesStarted    int64 `json:"instancesStarted"`
	InstancesCompleted  int64 `json:"instancesCompleted"`
	InstancesTerminated int64 `json:"instancesTerminated"`
	TasksCreated        int64 `json:"tasksCreated"`
	TasksCompleted      int64 `json:"tasksCompleted"`
	ServiceTasksRun     int64 `json:"serviceTasksRun"`
	ExternalCreated     int64 `json:"externalTasksCreated"`
	ExternalCompleted   int64 `json:"externalTasksCompleted"`
	TimersFired         int64 `json:"timersFired"`
	JobsExecuted        int64 `json:"jobsExecuted"`
	MessagesCorrelated  int64 `json:"messagesCorrelated"`
	DecisionsEvaluated  int64 `json:"decisionsEvaluated"`
	IncidentsCreated    int64 `json:"incidentsCreated"`
	AgentInvocations    int64 `json:"agentInvocations"`
	AgentInputTokens    int64 `json:"agentInputTokens"`
	AgentOutputTokens   int64 `json:"agentOutputTokens"`
}

// Snapshot copies all counters.
func (m *Metrics) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		DefinitionsDeployed: m.DefinitionsDeployed.Load(),
		InstancesStarted:    m.InstancesStarted.Load(),
		InstancesCompleted:  m.InstancesCompleted.Load(),
		InstancesTerminated: m.InstancesTerminated.Load(),
		TasksCreated:        m.TasksCreated.Load(),
		TasksCompleted:      m.TasksCompleted.Load(),
		ServiceTasksRun:     m.ServiceTasksRun.Load(),
		ExternalCreated:     m.ExternalCreated.Load(),
		ExternalCompleted:   m.ExternalCompleted.Load(),
		TimersFired:         m.TimersFired.Load(),
		JobsExecuted:        m.JobsExecuted.Load(),
		MessagesCorrelated:  m.MessagesCorrelated.Load(),
		DecisionsEvaluated:  m.DecisionsEvaluated.Load(),
		IncidentsCreated:    m.IncidentsCreated.Load(),
		AgentInvocations:    m.AgentInvocations.Load(),
		AgentInputTokens:    m.AgentInputTokens.Load(),
		AgentOutputTokens:   m.AgentOutputTokens.Load(),
	}
}

// newTicker exists so tests could stub time; the standard ticker is fine
// for production.
func newTicker(d time.Duration) *time.Ticker { return time.NewTicker(d) }
