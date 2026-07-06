// Package engine implements the Fliable process engine: a token-based
// BPMN 2.0 runtime designed to be embedded in Go programs or served over
// the REST API.
//
// Architecture in one paragraph: process definitions parse once into an
// immutable graph (package bpmn); each process instance is a small record
// holding variables plus a set of tokens; the engine advances active
// tokens through element behaviors until every token parks on a wait state
// (user task, timer, message, external worker, child instance); wait
// states are persisted through the pluggable store; timers and async
// continuations are jobs claimed by a scheduler goroutine; every state
// change is appended to an event-sourced history stream. Work is
// serialized per instance with striped locks, so thousands of instances
// advance in parallel with no database row locking — the contention point
// Java BPM engines hit under load.
package engine

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olbboy/fliable/bpmn"
	"github.com/olbboy/fliable/expr"
	"github.com/olbboy/fliable/store"
)

// ServiceHandler executes a service task in-process. It returns output
// variables to merge into the instance, or an error. Return a *BPMNError
// to throw a catchable BPMN error; any other error triggers the retry /
// incident cycle.
type ServiceHandler func(ctx Context) (map[string]any, error)

// Context is the read view a service handler gets.
type Context struct {
	InstanceID  string
	BusinessKey string
	ElementID   string
	ElementName string
	Variables   map[string]any
}

// BPMNError is a business error thrown by handlers and caught by error
// boundary events or event sub-processes.
type BPMNError struct {
	Code    string
	Message string
}

func (e *BPMNError) Error() string {
	if e.Message == "" {
		return "bpmn error " + e.Code
	}
	return fmt.Sprintf("bpmn error %s: %s", e.Code, e.Message)
}

// NewBPMNError creates a catchable business error.
func NewBPMNError(code, message string) *BPMNError {
	return &BPMNError{Code: code, Message: message}
}

// DecisionEvaluator evaluates a decision (DMN table) for business rule
// tasks. The dmn package provides an implementation; custom rule engines
// can plug in the same way.
type DecisionEvaluator interface {
	EvaluateDecision(key string, vars map[string]any) (any, error)
}

// Option configures the engine.
type Option func(*Engine)

// WithClock injects a clock (tests, deterministic replay).
func WithClock(fn func() time.Time) Option {
	return func(e *Engine) { e.now = fn }
}

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) { e.log = l }
}

// WithDecisionEvaluator plugs in a rule/decision engine for business rule
// tasks.
func WithDecisionEvaluator(d DecisionEvaluator) Option {
	return func(e *Engine) { e.decisions = d }
}

// WithRetryBackoff sets the base backoff for service task retries
// (attempt n waits base * 2^n). Default 5s.
func WithRetryBackoff(d time.Duration) Option {
	return func(e *Engine) { e.retryBackoff = d }
}

// WithJobPollInterval sets how often the scheduler claims due jobs.
// Default 100ms.
func WithJobPollInterval(d time.Duration) Option {
	return func(e *Engine) { e.pollInterval = d }
}

// Engine is the Fliable process engine. Create with New, register service
// handlers, deploy definitions, then Start it to activate timers and async
// execution.
type Engine struct {
	st  store.Store
	log *slog.Logger
	now func() time.Time

	handlers  map[string]ServiceHandler
	hmu       sync.RWMutex
	decisions DecisionEvaluator

	listeners []func(*store.HistoryEvent)
	lmu       sync.RWMutex

	defCache sync.Map // definition ID -> *parsedDef

	locks        [64]sync.Mutex // striped per-instance locks
	retryBackoff time.Duration
	pollInterval time.Duration

	stop    chan struct{}
	stopped chan struct{}
	running atomic.Bool

	metrics  Metrics
	seq      atomic.Uint64
	idSuffix string
}

type parsedDef struct {
	def  *store.Definition
	doc  *bpmn.Definitions
	proc *bpmn.Process
}

// New creates an engine on top of the given store.
func New(st store.Store, opts ...Option) *Engine {
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	e := &Engine{
		st:           st,
		log:          slog.Default(),
		now:          func() time.Time { return time.Now().UTC() },
		handlers:     map[string]ServiceHandler{},
		retryBackoff: 5 * time.Second,
		pollInterval: 100 * time.Millisecond,
		idSuffix:     hex.EncodeToString(rnd[:]),
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Store exposes the underlying store (read-mostly: queries, dashboards).
func (e *Engine) Store() store.Store { return e.st }

// Metrics returns a snapshot of engine counters.
func (e *Engine) Metrics() MetricsSnapshot { return e.metrics.Snapshot() }

// RegisterHandler binds a service task type (fliable:type / Flowable
// delegate name) to a Go handler.
func (e *Engine) RegisterHandler(taskType string, h ServiceHandler) {
	e.hmu.Lock()
	defer e.hmu.Unlock()
	e.handlers[taskType] = h
}

func (e *Engine) handler(taskType string) ServiceHandler {
	e.hmu.RLock()
	defer e.hmu.RUnlock()
	return e.handlers[taskType]
}

// OnEvent subscribes to the live history event stream (audit, websockets,
// metrics). Listeners run synchronously; keep them fast.
func (e *Engine) OnEvent(fn func(*store.HistoryEvent)) {
	e.lmu.Lock()
	defer e.lmu.Unlock()
	e.listeners = append(e.listeners, fn)
}

// Start launches the background scheduler (timers, async continuations,
// retries). Safe to call once.
func (e *Engine) Start() {
	if !e.running.CompareAndSwap(false, true) {
		return
	}
	e.stop = make(chan struct{})
	e.stopped = make(chan struct{})
	go e.schedulerLoop()
}

// Stop halts the scheduler and waits for it to drain.
func (e *Engine) Stop() {
	if !e.running.CompareAndSwap(true, false) {
		return
	}
	close(e.stop)
	<-e.stopped
}

// ---- ids -------------------------------------------------------------------

// newID returns a lexicographically sortable unique id: hex timestamp,
// engine-lifetime counter and per-engine random discriminator (read from
// crypto/rand once at construction, so the hot path stays syscall-free).
func (e *Engine) newID(prefix string) string {
	return fmt.Sprintf("%s_%011x%08x%s", prefix, e.now().UnixMicro(), e.seq.Add(1), e.idSuffix)
}

func (e *Engine) lockFor(instanceID string) *sync.Mutex {
	h := uint32(2166136261)
	for i := 0; i < len(instanceID); i++ {
		h = (h ^ uint32(instanceID[i])) * 16777619
	}
	return &e.locks[h%uint32(len(e.locks))]
}

// ---- deployment --------------------------------------------------------------

// Deploy parses, validates and stores a BPMN definition. Redeploying the
// same process key bumps the version; in-flight instances continue on
// their original version.
func (e *Engine) Deploy(xml []byte, name string) (*store.Definition, error) {
	doc, err := bpmn.Parse(xml)
	if err != nil {
		return nil, err
	}
	proc := doc.FirstExecutable()
	if proc == nil {
		return nil, errors.New("engine: no executable process in document")
	}
	version := 1
	if prev, err := e.st.LatestDefinition(proc.ID); err == nil {
		version = prev.Version + 1
	}
	if name == "" {
		name = proc.Name
	}
	def := &store.Definition{
		ID:         e.newID("def"),
		Key:        proc.ID,
		Version:    version,
		Name:       name,
		XML:        append([]byte(nil), xml...),
		DeployedAt: e.now(),
	}
	if err := e.st.PutDefinition(def); err != nil {
		return nil, err
	}
	e.defCache.Store(def.ID, &parsedDef{def: def, doc: doc, proc: proc})
	if err := e.registerStartTriggers(def, proc); err != nil {
		return nil, err
	}
	e.metrics.DefinitionsDeployed.Add(1)
	e.log.Info("definition deployed", "key", def.Key, "version", def.Version, "id", def.ID)
	return def, nil
}

// registerStartTriggers installs message-start subscriptions and
// timer-start jobs for the newest version of a definition, replacing the
// previous version's triggers.
func (e *Engine) registerStartTriggers(def *store.Definition, proc *bpmn.Process) error {
	// Drop older start triggers for this key.
	isStart := true
	subs, err := e.st.ListSubscriptions(store.SubscriptionFilter{IsStart: &isStart})
	if err != nil {
		return err
	}
	for _, s := range subs {
		if s.DefinitionKey == def.Key {
			if err := e.st.DeleteSubscription(s.ID); err != nil {
				return err
			}
		}
	}
	jobs, err := e.st.ListJobs("")
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.DefinitionKey == def.Key && j.InstanceID == "" {
			if err := e.st.DeleteJob(j.ID); err != nil {
				return err
			}
		}
	}

	for _, se := range proc.StartEvents() {
		if se.Event == nil {
			continue
		}
		switch se.Event.Kind {
		case bpmn.KindMessage:
			sub := &store.Subscription{
				ID:            e.newID("sub"),
				Kind:          store.SubMessage,
				Name:          se.Event.Message,
				IsStart:       true,
				DefinitionKey: def.Key,
				ElementID:     se.ID,
				CreatedAt:     e.now(),
			}
			if err := e.st.PutSubscription(sub); err != nil {
				return err
			}
		case bpmn.KindSignal:
			sub := &store.Subscription{
				ID:            e.newID("sub"),
				Kind:          store.SubSignal,
				Name:          se.Event.Signal,
				IsStart:       true,
				DefinitionKey: def.Key,
				ElementID:     se.ID,
				CreatedAt:     e.now(),
			}
			if err := e.st.PutSubscription(sub); err != nil {
				return err
			}
		case bpmn.KindTimer:
			due, repeats, interval, err := e.timerSchedule(se.Event, nil)
			if err != nil {
				return fmt.Errorf("engine: timer start event %s: %w", se.ID, err)
			}
			job := &store.Job{
				ID:            e.newID("job"),
				Kind:          store.JobTimer,
				DefinitionKey: def.Key,
				ElementID:     se.ID,
				DueAt:         due,
				Repeats:       repeats,
				Interval:      interval,
				CreatedAt:     e.now(),
			}
			if err := e.st.PutJob(job); err != nil {
				return err
			}
		}
	}
	return nil
}

// definition resolves and caches a parsed definition by ID.
func (e *Engine) definition(id string) (*parsedDef, error) {
	if v, ok := e.defCache.Load(id); ok {
		return v.(*parsedDef), nil
	}
	def, err := e.st.GetDefinition(id)
	if err != nil {
		return nil, err
	}
	doc, err := bpmn.Parse(def.XML)
	if err != nil {
		return nil, fmt.Errorf("engine: stored definition %s is invalid: %w", id, err)
	}
	pd := &parsedDef{def: def, doc: doc, proc: doc.FirstExecutable()}
	e.defCache.Store(id, pd)
	return pd, nil
}

// ---- instance lifecycle --------------------------------------------------------

// StartInstance starts an instance of the latest version of key.
func (e *Engine) StartInstance(key, businessKey string, vars map[string]any) (*store.Instance, error) {
	def, err := e.st.LatestDefinition(key)
	if err != nil {
		return nil, fmt.Errorf("engine: unknown process %q: %w", key, err)
	}
	return e.StartInstanceByDefinition(def.ID, businessKey, vars)
}

// StartInstanceByDefinition starts an instance of an exact definition
// version.
func (e *Engine) StartInstanceByDefinition(defID, businessKey string, vars map[string]any) (*store.Instance, error) {
	return e.startInstanceAt(defID, businessKey, vars, "", "", "")
}

// startChildInstance starts a call-activity child with a pre-allocated
// instance ID (the parent already parked its token on that ID).
func (e *Engine) startChildInstance(defID, childID, businessKey string, vars map[string]any, parentID, parentTokenID string) (*store.Instance, error) {
	return e.startInstance(defID, childID, businessKey, vars, "", parentID, parentTokenID)
}

// startInstanceAt starts an instance from an optional non-default start
// element (message/timer/signal starts).
func (e *Engine) startInstanceAt(defID, businessKey string, vars map[string]any, startElement, parentID, parentTokenID string) (*store.Instance, error) {
	return e.startInstance(defID, "", businessKey, vars, startElement, parentID, parentTokenID)
}

// startInstance creates the instance record and runs it until quiescent.
func (e *Engine) startInstance(defID, presetID, businessKey string, vars map[string]any, startElement, parentID, parentTokenID string) (*store.Instance, error) {
	pd, err := e.definition(defID)
	if err != nil {
		return nil, err
	}
	start := pd.proc.NoneStartEvent()
	if startElement != "" {
		start = pd.proc.Elements[startElement]
	}
	if start == nil {
		return nil, fmt.Errorf("engine: process %q has no usable start event", pd.def.Key)
	}
	if vars == nil {
		vars = map[string]any{}
	}
	normVars := make(map[string]any, len(vars))
	for k, v := range vars {
		normVars[k] = expr.Normalize(v)
	}
	id := presetID
	if id == "" {
		id = e.newID("inst")
	}
	inst := &store.Instance{
		ID:            id,
		DefinitionID:  pd.def.ID,
		DefinitionKey: pd.def.Key,
		BusinessKey:   businessKey,
		State:         store.InstanceActive,
		Variables:     normVars,
		Tokens:        map[string]*store.Token{},
		ParentID:      parentID,
		ParentTokenID: parentTokenID,
		StartedAt:     e.now(),
	}
	tok := &store.Token{ID: e.newID("tok"), ElementID: start.ID, State: store.TokenActive}
	inst.Tokens[tok.ID] = tok

	e.metrics.InstancesStarted.Add(1)
	rt := &runtime{e: e, pd: pd, inst: inst}
	rt.emit(store.HistInstanceStarted, start.ID, map[string]any{"businessKey": businessKey, "definitionKey": pd.def.Key, "version": float64(pd.def.Version)})
	rt.registerEventSubprocesses(&pd.proc.Container, nil, "")

	mu := e.lockFor(inst.ID)
	mu.Lock()
	err = rt.drain()
	if err == nil {
		err = e.st.PutInstance(inst)
	}
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	e.afterRun(rt)
	return e.st.GetInstance(inst.ID)
}

// resume loads an instance, applies mutate under the instance lock, then
// drains active tokens and persists. mutate returns false to abort silently
// (e.g. the wait state was already resolved).
func (e *Engine) resume(instanceID string, mutate func(rt *runtime) (bool, error)) error {
	mu := e.lockFor(instanceID)
	mu.Lock()

	inst, err := e.st.GetInstance(instanceID)
	if err != nil {
		mu.Unlock()
		return err
	}
	if inst.State != store.InstanceActive {
		mu.Unlock()
		return fmt.Errorf("engine: instance %s is %s", instanceID, inst.State)
	}
	pd, err := e.definition(inst.DefinitionID)
	if err != nil {
		mu.Unlock()
		return err
	}
	rt := &runtime{e: e, pd: pd, inst: inst}
	ok, err := mutate(rt)
	if err == nil && ok {
		err = rt.drain()
	}
	if err == nil && ok {
		err = e.st.PutInstance(inst)
	}
	mu.Unlock()
	if err != nil {
		return err
	}
	if ok {
		e.afterRun(rt)
	}
	return nil
}

// afterRun executes cross-instance continuations collected during a run,
// after the instance lock is released (avoids lock-order deadlocks).
func (e *Engine) afterRun(rt *runtime) {
	for _, c := range rt.continuations {
		c()
	}
}

// CancelInstance terminates a running instance and cancels all its wait
// states.
func (e *Engine) CancelInstance(id, reason string) error {
	err := e.resume(id, func(rt *runtime) (bool, error) {
		rt.terminate(reason)
		return true, nil
	})
	return err
}

// GetInstance returns an instance by id.
func (e *Engine) GetInstance(id string) (*store.Instance, error) { return e.st.GetInstance(id) }

// ListInstances queries instances.
func (e *Engine) ListInstances(f store.InstanceFilter) ([]*store.Instance, error) {
	return e.st.ListInstances(f)
}

// SetVariables merges variables into a running instance.
func (e *Engine) SetVariables(id string, vars map[string]any) error {
	return e.resume(id, func(rt *runtime) (bool, error) {
		for k, v := range vars {
			rt.inst.Variables[k] = expr.Normalize(v)
		}
		rt.emit(store.HistVariablesSet, "", map[string]any{"names": varNames(vars)})
		return true, nil
	})
}

// ---- user tasks ------------------------------------------------------------------

// ClaimTask assigns a task to a user (fails if already claimed by someone
// else).
func (e *Engine) ClaimTask(taskID, user string) error {
	t, err := e.st.GetTask(taskID)
	if err != nil {
		return err
	}
	if t.State != store.TaskCreated {
		return fmt.Errorf("engine: task %s is %s", taskID, t.State)
	}
	if t.Assignee != "" && t.Assignee != user {
		return fmt.Errorf("engine: task %s already assigned to %s", taskID, t.Assignee)
	}
	t.Assignee = user
	return e.st.PutTask(t)
}

// CompleteTask finishes a user task, merging vars into the instance and
// resuming the flow.
func (e *Engine) CompleteTask(taskID string, vars map[string]any, user string) error {
	t, err := e.st.GetTask(taskID)
	if err != nil {
		return err
	}
	if t.State != store.TaskCreated {
		return fmt.Errorf("engine: task %s is %s", taskID, t.State)
	}
	return e.resume(t.InstanceID, func(rt *runtime) (bool, error) {
		tok := rt.inst.Tokens[t.TokenID]
		if tok == nil || tok.State != store.TokenWaitTask || tok.WaitRef != t.ID {
			return false, fmt.Errorf("engine: task %s is no longer active", taskID)
		}
		t.State = store.TaskCompleted
		t.CompletedAt = e.now()
		t.CompletedBy = user
		if err := e.st.PutTask(t); err != nil {
			return false, err
		}
		for k, v := range vars {
			rt.inst.Variables[k] = expr.Normalize(v)
		}
		e.metrics.TasksCompleted.Add(1)
		rt.emit(store.HistTaskCompleted, t.ElementID, map[string]any{"taskId": t.ID, "by": user})
		el := rt.element(tok)
		if el != nil {
			rt.applyOutputs(el, tok)
		}
		rt.completeActivity(tok)
		return true, nil
	})
}

// ListTasks queries user tasks.
func (e *Engine) ListTasks(f store.TaskFilter) ([]*store.Task, error) { return e.st.ListTasks(f) }

// ---- messages & signals --------------------------------------------------------------

// CorrelateMessage delivers a named message. correlationKey narrows
// delivery to instances whose business key matches (empty = all waiting
// subscriptions). Message start events spawn new instances. Returns the
// number of activations.
func (e *Engine) CorrelateMessage(name, correlationKey string, vars map[string]any) (int, error) {
	subs, err := e.st.ListSubscriptions(store.SubscriptionFilter{Kind: store.SubMessage, Name: name})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, sub := range subs {
		if sub.IsStart {
			continue
		}
		if correlationKey != "" && sub.CorrelationKey != correlationKey {
			continue
		}
		if err := e.triggerSubscription(sub, vars, store.HistMessageReceived); err != nil {
			e.log.Warn("message trigger failed", "subscription", sub.ID, "error", err)
			continue
		}
		count++
	}
	// Start events: only when the message did not target a waiting
	// instance, or always? BPMN semantics: a message is delivered once;
	// Fliable delivers to every matching waiting subscription and starts
	// new instances only when no correlation key was matched.
	if count == 0 {
		for _, sub := range subs {
			if !sub.IsStart {
				continue
			}
			def, err := e.st.LatestDefinition(sub.DefinitionKey)
			if err != nil {
				continue
			}
			if _, err := e.startInstanceAt(def.ID, correlationKey, vars, sub.ElementID, "", ""); err != nil {
				return count, err
			}
			count++
		}
	}
	if count > 0 {
		e.metrics.MessagesCorrelated.Add(1)
	}
	return count, nil
}

// BroadcastSignal delivers a signal to every waiting subscription and
// signal start event.
func (e *Engine) BroadcastSignal(name string, vars map[string]any) (int, error) {
	subs, err := e.st.ListSubscriptions(store.SubscriptionFilter{Kind: store.SubSignal, Name: name})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, sub := range subs {
		if sub.IsStart {
			def, err := e.st.LatestDefinition(sub.DefinitionKey)
			if err != nil {
				continue
			}
			if _, err := e.startInstanceAt(def.ID, "", vars, sub.ElementID, "", ""); err != nil {
				return count, err
			}
			count++
			continue
		}
		if err := e.triggerSubscription(sub, vars, store.HistSignalReceived); err != nil {
			e.log.Warn("signal trigger failed", "subscription", sub.ID, "error", err)
			continue
		}
		count++
	}
	return count, nil
}

// triggerSubscription resumes the token waiting on a subscription.
func (e *Engine) triggerSubscription(sub *store.Subscription, vars map[string]any, histType string) error {
	return e.resume(sub.InstanceID, func(rt *runtime) (bool, error) {
		// The subscription may have been cancelled by a concurrent path.
		still, err := e.st.ListSubscriptions(store.SubscriptionFilter{InstanceID: sub.InstanceID})
		if err != nil {
			return false, err
		}
		found := false
		for _, s := range still {
			if s.ID == sub.ID {
				found = true
			}
		}
		if !found {
			return false, nil
		}
		for k, v := range vars {
			rt.inst.Variables[k] = expr.Normalize(v)
		}
		rt.emit(histType, sub.ElementID, map[string]any{"name": sub.Name})
		return rt.eventTriggered(sub.TokenID, sub.ElementID, sub.ID)
	})
}

// ---- external worker tasks ---------------------------------------------------------------

// FetchExternalTasks locks up to limit pending tasks on topic for a
// worker.
func (e *Engine) FetchExternalTasks(topic, workerID string, lockFor time.Duration, limit int) ([]*store.ExternalTask, error) {
	now := e.now()
	return e.st.FetchAndLockExternalTasks(topic, workerID, now.Add(lockFor), now, limit)
}

// CompleteExternalTask reports successful completion by a worker.
func (e *Engine) CompleteExternalTask(id, workerID string, vars map[string]any) error {
	t, err := e.st.GetExternalTask(id)
	if err != nil {
		return err
	}
	if t.State != store.ExternalPending {
		return fmt.Errorf("engine: external task %s is %s", id, t.State)
	}
	if t.LockedBy != workerID {
		return fmt.Errorf("engine: external task %s locked by %q, not %q", id, t.LockedBy, workerID)
	}
	return e.resume(t.InstanceID, func(rt *runtime) (bool, error) {
		tok := rt.inst.Tokens[t.TokenID]
		if tok == nil || tok.State != store.TokenWaitExternal || tok.WaitRef != t.ID {
			return false, fmt.Errorf("engine: external task %s is no longer active", id)
		}
		t.State = store.ExternalDone
		if err := e.st.PutExternalTask(t); err != nil {
			return false, err
		}
		for k, v := range vars {
			rt.inst.Variables[k] = expr.Normalize(v)
		}
		e.metrics.ExternalCompleted.Add(1)
		el := rt.element(tok)
		rt.emit(store.HistElementCompleted, tok.ElementID, map[string]any{"worker": workerID})
		if el != nil {
			rt.applyOutputs(el, tok)
		}
		rt.completeActivity(tok)
		return true, nil
	})
}

// FailExternalTask reports a failure; retries left triggers redelivery
// after backoff, exhausted retries raise an incident. An errorCode throws
// a BPMN error instead.
func (e *Engine) FailExternalTask(id, workerID, message, errorCode string) error {
	t, err := e.st.GetExternalTask(id)
	if err != nil {
		return err
	}
	if t.State != store.ExternalPending {
		return fmt.Errorf("engine: external task %s is %s", id, t.State)
	}
	if t.LockedBy != workerID {
		return fmt.Errorf("engine: external task %s locked by %q, not %q", id, t.LockedBy, workerID)
	}
	return e.resume(t.InstanceID, func(rt *runtime) (bool, error) {
		tok := rt.inst.Tokens[t.TokenID]
		if tok == nil || tok.State != store.TokenWaitExternal || tok.WaitRef != t.ID {
			return false, fmt.Errorf("engine: external task %s is no longer active", id)
		}
		if errorCode != "" {
			t.State = store.ExternalFailed
			if err := e.st.PutExternalTask(t); err != nil {
				return false, err
			}
			rt.throwError(tok, errorCode, message)
			return true, nil
		}
		t.Retries--
		if t.Retries > 0 {
			t.LockedBy = ""
			t.LockUntil = time.Time{}
			return true, e.st.PutExternalTask(t)
		}
		t.State = store.ExternalFailed
		if err := e.st.PutExternalTask(t); err != nil {
			return false, err
		}
		rt.raiseIncident(tok, "", fmt.Sprintf("external task on topic %q failed: %s", t.Topic, message))
		return true, nil
	})
}

// ---- incidents ------------------------------------------------------------------------------

// ResolveIncident marks an incident resolved and re-activates its token
// (fresh retries for the failed element).
func (e *Engine) ResolveIncident(id string) error {
	inc, err := e.st.GetIncident(id)
	if err != nil {
		return err
	}
	if inc.Resolved {
		return fmt.Errorf("engine: incident %s already resolved", id)
	}
	return e.resume(inc.InstanceID, func(rt *runtime) (bool, error) {
		inc.Resolved = true
		inc.ResolvedAt = e.now()
		if err := e.st.PutIncident(inc); err != nil {
			return false, err
		}
		rt.emit(store.HistIncidentResolved, inc.ElementID, map[string]any{"incidentId": inc.ID})
		tok := rt.inst.Tokens[inc.TokenID]
		if tok != nil && tok.State == store.TokenWaitRetry {
			tok.State = store.TokenActive
			tok.WaitRef = ""
		}
		return true, nil
	})
}

// ---- history ---------------------------------------------------------------------------------

// History returns the audit trail of an instance.
func (e *Engine) History(instanceID string, f store.HistoryFilter) ([]*store.HistoryEvent, error) {
	return e.st.ListHistory(instanceID, f)
}

func varNames(vars map[string]any) []any {
	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

// evalField resolves a model string that is either a literal or an
// expression: "${...}"/"#{...}" wrappers and "=" prefixes mark
// expressions; anything else is literal.
func evalField(s string, vars map[string]any) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	var src string
	switch {
	case strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}"):
		src = s[2 : len(s)-1]
	case strings.HasPrefix(s, "#{") && strings.HasSuffix(s, "}"):
		src = s[2 : len(s)-1]
	case strings.HasPrefix(s, "="):
		src = s[1:]
	default:
		return s, nil
	}
	return expr.Eval(src, vars)
}

func evalFieldString(s string, vars map[string]any) (string, error) {
	v, err := evalField(s, vars)
	if err != nil {
		return "", err
	}
	return expr.Stringify(v), nil
}
