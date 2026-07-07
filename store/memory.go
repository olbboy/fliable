package store

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is an in-process Store. All reads return deep copies so callers
// can never alias engine-internal state. It is the default store for
// embedded use and the backing state of the durable journal store.
type Memory struct {
	mu sync.RWMutex

	definitions map[string]*Definition
	instances   map[string]*Instance
	tasks       map[string]*Task
	jobs        map[string]*Job
	externals   map[string]*ExternalTask
	agentJobs   map[string]*AgentJob
	subs        map[string]*Subscription
	incidents   map[string]*Incident
	history     map[string][]*HistoryEvent
	histSeq     map[string]int64
}

// NewMemory creates an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		definitions: map[string]*Definition{},
		instances:   map[string]*Instance{},
		tasks:       map[string]*Task{},
		jobs:        map[string]*Job{},
		externals:   map[string]*ExternalTask{},
		agentJobs:   map[string]*AgentJob{},
		subs:        map[string]*Subscription{},
		incidents:   map[string]*Incident{},
		history:     map[string][]*HistoryEvent{},
		histSeq:     map[string]int64{},
	}
}

// Close implements Store.
func (m *Memory) Close() error { return nil }

// ---- definitions -----------------------------------------------------------

// PutDefinition implements Store.
func (m *Memory) PutDefinition(d *Definition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.definitions[d.ID] = cloneDefinition(d)
	return nil
}

// GetDefinition implements Store.
func (m *Memory) GetDefinition(id string) (*Definition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.definitions[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneDefinition(d), nil
}

// LatestDefinition implements Store.
func (m *Memory) LatestDefinition(key string) (*Definition, error) {
	return m.LatestDefinitionForTenant("", key)
}

// LatestDefinitionForTenant implements Store.
func (m *Memory) LatestDefinitionForTenant(tenantID, key string) (*Definition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var best *Definition
	for _, d := range m.definitions {
		if d.Key == key && d.TenantID == tenantID && (best == nil || d.Version > best.Version) {
			best = d
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return cloneDefinition(best), nil
}

// ListDefinitions implements Store.
func (m *Memory) ListDefinitions(latestOnly bool) ([]*Definition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byKey := map[string]*Definition{}
	var out []*Definition
	for _, d := range m.definitions {
		if latestOnly {
			if cur, ok := byKey[d.Key]; !ok || d.Version > cur.Version {
				byKey[d.Key] = d
			}
			continue
		}
		out = append(out, cloneDefinition(d))
	}
	if latestOnly {
		for _, d := range byKey {
			out = append(out, cloneDefinition(d))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

// ---- instances -------------------------------------------------------------

// PutInstance implements Store.
func (m *Memory) PutInstance(inst *Instance) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := cloneInstance(inst)
	c.Rev++
	m.instances[c.ID] = c
	inst.Rev = c.Rev
	return nil
}

// GetInstance implements Store.
func (m *Memory) GetInstance(id string) (*Instance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst, ok := m.instances[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneInstance(inst), nil
}

// ListInstances implements Store.
func (m *Memory) ListInstances(f InstanceFilter) ([]*Instance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Instance
	for _, inst := range m.instances {
		if f.TenantID != "" && inst.TenantID != f.TenantID {
			continue
		}
		if f.DefinitionKey != "" && inst.DefinitionKey != f.DefinitionKey {
			continue
		}
		if f.DefinitionID != "" && inst.DefinitionID != f.DefinitionID {
			continue
		}
		if f.BusinessKey != "" && inst.BusinessKey != f.BusinessKey {
			continue
		}
		if f.State != "" && inst.State != f.State {
			continue
		}
		if f.ParentID != "" && inst.ParentID != f.ParentID {
			continue
		}
		if !inTimeWindow(inst.StartedAt, f.StartedAfter, f.StartedBefore) {
			continue
		}
		if (!f.EndedAfter.IsZero() || !f.EndedBefore.IsZero()) && !inTimeWindow(inst.EndedAt, f.EndedAfter, f.EndedBefore) {
			continue
		}
		if len(f.Vars) > 0 && !MatchVars(inst.Variables, f.Vars) {
			continue
		}
		if !afterCursor(inst.ID, f.Cursor, f.Desc) {
			continue
		}
		out = append(out, cloneInstance(inst))
	}
	sortByID(out, func(i *Instance) string { return i.ID }, f.Desc)
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// PurgeInstance implements Store.
func (m *Memory) PurgeInstance(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.instances, id)
	delete(m.history, id)
	delete(m.histSeq, id)
	for tid, t := range m.tasks {
		if t.InstanceID == id {
			delete(m.tasks, tid)
		}
	}
	for jid, j := range m.jobs {
		if j.InstanceID == id {
			delete(m.jobs, jid)
		}
	}
	for eid, e := range m.externals {
		if e.InstanceID == id {
			delete(m.externals, eid)
		}
	}
	for aid, a := range m.agentJobs {
		if a.InstanceID == id {
			delete(m.agentJobs, aid)
		}
	}
	for sid, s := range m.subs {
		if s.InstanceID == id {
			delete(m.subs, sid)
		}
	}
	for iid, inc := range m.incidents {
		if inc.InstanceID == id {
			delete(m.incidents, iid)
		}
	}
	return nil
}

// ---- tasks -----------------------------------------------------------------

// PutTask implements Store.
func (m *Memory) PutTask(t *Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.ID] = cloneTask(t)
	return nil
}

// GetTask implements Store.
func (m *Memory) GetTask(id string) (*Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneTask(t), nil
}

// ListTasks implements Store.
func (m *Memory) ListTasks(f TaskFilter) ([]*Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Task
	for _, t := range m.tasks {
		if f.TenantID != "" && t.TenantID != f.TenantID {
			continue
		}
		if f.InstanceID != "" && t.InstanceID != f.InstanceID {
			continue
		}
		if f.Assignee != "" && t.Assignee != f.Assignee {
			continue
		}
		if f.Unassigned && t.Assignee != "" {
			continue
		}
		if f.CandidateUser != "" && !containsStr(t.CandidateUsers, f.CandidateUser) {
			continue
		}
		if f.CandidateGroup != "" && !containsStr(t.CandidateGroups, f.CandidateGroup) {
			continue
		}
		if f.State != "" && t.State != f.State {
			continue
		}
		if f.DefinitionKey != "" && t.DefinitionKey != f.DefinitionKey {
			continue
		}
		if f.ElementID != "" && t.ElementID != f.ElementID {
			continue
		}
		if !inTimeWindow(t.CreatedAt, f.CreatedAfter, f.CreatedBefore) {
			continue
		}
		if !f.DueBefore.IsZero() && (t.DueAt.IsZero() || t.DueAt.After(f.DueBefore)) {
			continue
		}
		if len(f.Vars) > 0 {
			inst := m.instances[t.InstanceID]
			if inst == nil || !MatchVars(inst.Variables, f.Vars) {
				continue
			}
		}
		if !afterCursor(t.ID, f.Cursor, f.Desc) {
			continue
		}
		out = append(out, cloneTask(t))
	}
	sortByID(out, func(t *Task) string { return t.ID }, f.Desc)
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// ---- jobs ------------------------------------------------------------------

// PutJob implements Store.
func (m *Memory) PutJob(j *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *j
	m.jobs[j.ID] = &cp
	return nil
}

// GetJob implements Store.
func (m *Memory) GetJob(id string) (*Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *j
	return &cp, nil
}

// DeleteJob implements Store.
func (m *Memory) DeleteJob(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.jobs, id)
	return nil
}

// DueJobs implements Store.
func (m *Memory) DueJobs(now time.Time, limit int) ([]*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var due []*Job
	for _, j := range m.jobs {
		if !j.DueAt.After(now) {
			cp := *j
			due = append(due, &cp)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].DueAt.Equal(due[j].DueAt) {
			return due[i].DueAt.Before(due[j].DueAt)
		}
		return due[i].ID < due[j].ID
	})
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}
	for _, j := range due {
		delete(m.jobs, j.ID)
	}
	return due, nil
}

// ListJobs implements Store.
func (m *Memory) ListJobs(instanceID string) ([]*Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Job
	for _, j := range m.jobs {
		if instanceID == "" || j.InstanceID == instanceID {
			cp := *j
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- external tasks ---------------------------------------------------------

// PutExternalTask implements Store.
func (m *Memory) PutExternalTask(t *ExternalTask) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.externals[t.ID] = cloneExternal(t)
	return nil
}

// GetExternalTask implements Store.
func (m *Memory) GetExternalTask(id string) (*ExternalTask, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.externals[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneExternal(t), nil
}

// FetchAndLockExternalTasks implements Store.
func (m *Memory) FetchAndLockExternalTasks(topic, workerID string, until, now time.Time, limit int) ([]*ExternalTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var avail []*ExternalTask
	for _, t := range m.externals {
		if t.Topic != topic || t.State != ExternalPending {
			continue
		}
		if t.LockedBy != "" && t.LockUntil.After(now) {
			continue
		}
		avail = append(avail, t)
	}
	sort.Slice(avail, func(i, j int) bool { return avail[i].ID < avail[j].ID })
	if limit > 0 && len(avail) > limit {
		avail = avail[:limit]
	}
	out := make([]*ExternalTask, 0, len(avail))
	for _, t := range avail {
		t.LockedBy = workerID
		t.LockUntil = until
		out = append(out, cloneExternal(t))
	}
	return out, nil
}

// ListExternalTasks implements Store.
func (m *Memory) ListExternalTasks(instanceID string) ([]*ExternalTask, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*ExternalTask
	for _, t := range m.externals {
		if instanceID == "" || t.InstanceID == instanceID {
			out = append(out, cloneExternal(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- agent jobs --------------------------------------------------------------

// PutAgentJob implements Store.
func (m *Memory) PutAgentJob(j *AgentJob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agentJobs[j.ID] = cloneAgentJob(j)
	return nil
}

// GetAgentJob implements Store.
func (m *Memory) GetAgentJob(id string) (*AgentJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.agentJobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneAgentJob(j), nil
}

// FetchAndLockAgentJobs implements Store.
func (m *Memory) FetchAndLockAgentJobs(topic, workerID string, until, now time.Time, limit int) ([]*AgentJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var avail []*AgentJob
	for _, j := range m.agentJobs {
		if j.Topic != topic || j.State != AgentPending {
			continue
		}
		if j.LockedBy != "" && j.LockUntil.After(now) {
			continue
		}
		avail = append(avail, j)
	}
	sort.Slice(avail, func(i, j int) bool { return avail[i].ID < avail[j].ID })
	if limit > 0 && len(avail) > limit {
		avail = avail[:limit]
	}
	out := make([]*AgentJob, 0, len(avail))
	for _, j := range avail {
		j.LockedBy = workerID
		j.LockUntil = until
		out = append(out, cloneAgentJob(j))
	}
	return out, nil
}

// ListAgentJobs implements Store.
func (m *Memory) ListAgentJobs(instanceID string) ([]*AgentJob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*AgentJob
	for _, j := range m.agentJobs {
		if instanceID == "" || j.InstanceID == instanceID {
			out = append(out, cloneAgentJob(j))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- subscriptions -----------------------------------------------------------

// PutSubscription implements Store.
func (m *Memory) PutSubscription(s *Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *s
	m.subs[s.ID] = &cp
	return nil
}

// DeleteSubscription implements Store.
func (m *Memory) DeleteSubscription(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.subs, id)
	return nil
}

// ListSubscriptions implements Store.
func (m *Memory) ListSubscriptions(f SubscriptionFilter) ([]*Subscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Subscription
	for _, s := range m.subs {
		if f.Kind != "" && s.Kind != f.Kind {
			continue
		}
		if f.Name != "" && s.Name != f.Name {
			continue
		}
		if f.CorrelationKey != "" && s.CorrelationKey != f.CorrelationKey {
			continue
		}
		if f.InstanceID != "" && s.InstanceID != f.InstanceID {
			continue
		}
		if f.IsStart != nil && s.IsStart != *f.IsStart {
			continue
		}
		cp := *s
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- incidents ----------------------------------------------------------------

// PutIncident implements Store.
func (m *Memory) PutIncident(i *Incident) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *i
	m.incidents[i.ID] = &cp
	return nil
}

// GetIncident implements Store.
func (m *Memory) GetIncident(id string) (*Incident, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	i, ok := m.incidents[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *i
	return &cp, nil
}

// ListIncidents implements Store.
func (m *Memory) ListIncidents(f IncidentFilter) ([]*Incident, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Incident
	for _, i := range m.incidents {
		if f.InstanceID != "" && i.InstanceID != f.InstanceID {
			continue
		}
		if f.Resolved != nil && i.Resolved != *f.Resolved {
			continue
		}
		cp := *i
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// ---- history --------------------------------------------------------------------

// AppendHistory implements Store.
func (m *Memory) AppendHistory(ev *HistoryEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appendHistoryLocked(ev)
	return nil
}

func (m *Memory) appendHistoryLocked(ev *HistoryEvent) {
	if ev.Seq == 0 {
		m.histSeq[ev.InstanceID]++
		ev.Seq = m.histSeq[ev.InstanceID]
	} else if ev.Seq > m.histSeq[ev.InstanceID] {
		m.histSeq[ev.InstanceID] = ev.Seq
	}
	cp := *ev
	if ev.Detail != nil {
		cp.Detail = cloneMap(ev.Detail)
	}
	m.history[ev.InstanceID] = append(m.history[ev.InstanceID], &cp)
}

// ListHistory implements Store.
func (m *Memory) ListHistory(instanceID string, f HistoryFilter) ([]*HistoryEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*HistoryEvent
	for _, ev := range m.history[instanceID] {
		if f.Type != "" && !strings.HasPrefix(ev.Type, f.Type) {
			continue
		}
		if ev.Seq <= f.AfterSeq {
			continue
		}
		cp := *ev
		if ev.Detail != nil {
			cp.Detail = cloneMap(ev.Detail)
		}
		out = append(out, &cp)
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// ---- cloning helpers --------------------------------------------------------------

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func cloneDefinition(d *Definition) *Definition {
	cp := *d
	cp.XML = append([]byte(nil), d.XML...)
	return &cp
}

func cloneInstance(in *Instance) *Instance {
	cp := *in
	cp.Variables = cloneMap(in.Variables)
	cp.Tokens = make(map[string]*Token, len(in.Tokens))
	for id, tok := range in.Tokens {
		t := *tok
		t.ScopePath = append([]string(nil), tok.ScopePath...)
		t.ScopeOwners = append([]string(nil), tok.ScopeOwners...)
		if tok.LocalVars != nil {
			t.LocalVars = cloneMap(tok.LocalVars)
		}
		cp.Tokens[id] = &t
	}
	if in.Multi != nil {
		cp.Multi = make(map[string]*MultiInstanceState, len(in.Multi))
		for k, v := range in.Multi {
			mv := *v
			mv.Items = cloneSlice(v.Items)
			mv.Outputs = cloneSlice(v.Outputs)
			cp.Multi[k] = &mv
		}
	}
	return &cp
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneSlice(s []any) []any {
	if s == nil {
		return nil
	}
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = cloneValue(v)
	}
	return out
}

func cloneTask(t *Task) *Task {
	cp := *t
	cp.CandidateUsers = append([]string(nil), t.CandidateUsers...)
	cp.CandidateGroups = append([]string(nil), t.CandidateGroups...)
	return &cp
}

func cloneExternal(t *ExternalTask) *ExternalTask {
	cp := *t
	if t.Variables != nil {
		cp.Variables = cloneMap(t.Variables)
	}
	return &cp
}

func cloneAgentJob(j *AgentJob) *AgentJob {
	cp := *j
	cp.Tools = append([]AgentToolSpec(nil), j.Tools...)
	if j.Variables != nil {
		cp.Variables = cloneMap(j.Variables)
	}
	return &cp
}

// cloneValue deep-copies the JSON-ish value universe. Nil maps/slices stay
// typed non-nil zero values to keep type assertions safe.
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = cloneValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = cloneValue(vv)
		}
		return out
	case nil:
		return nil
	default:
		return v
	}
}
