package engine

import (
	"fmt"
	"time"

	"github.com/olbboy/fliable/bpmn"
	"github.com/olbboy/fliable/expr"
	"github.com/olbboy/fliable/store"
)

// scopeWaitRef marks a token waiting for an embedded sub-process scope
// (as opposed to a call-activity child instance).
const scopeWaitRef = "__scope"

// miChildFlag is the token-local key marking a multi-instance child; its
// value is the element ID so nested loops don't collide.
const miChildFlag = "__miChild"

// activity dispatches activity elements.
func (rt *runtime) activity(tok *store.Token, el *bpmn.Element) error {
	if el.MultiInstance != nil && !isMIChild(tok, el) {
		return rt.startMultiInstance(tok, el)
	}
	rt.emit(store.HistElementActivated, el.ID, nil)
	if err := rt.applyInputs(el, tok); err != nil {
		rt.raiseIncident(tok, "", fmt.Sprintf("input mapping on %s: %v", el.ID, err))
		return nil
	}
	switch el.Type {
	case bpmn.TypeUserTask:
		return rt.userTask(tok, el)
	case bpmn.TypeServiceTask, bpmn.TypeSendTask:
		return rt.serviceTask(tok, el)
	case bpmn.TypeScriptTask:
		return rt.scriptTask(tok, el)
	case bpmn.TypeBusinessRuleTask:
		return rt.businessRuleTask(tok, el)
	case bpmn.TypeReceiveTask:
		return rt.receiveTask(tok, el)
	case bpmn.TypeManualTask, bpmn.TypeTask:
		rt.finishSyncActivity(tok, el)
		return nil
	case bpmn.TypeSubProcess:
		return rt.enterSubProcess(tok, el)
	case bpmn.TypeCallActivity:
		return rt.callActivity(tok, el)
	}
	rt.raiseIncident(tok, "", fmt.Sprintf("unsupported activity type %s", el.Type))
	return nil
}

func isMIChild(tok *store.Token, el *bpmn.Element) bool {
	return tok.LocalVars != nil && tok.LocalVars[miChildFlag] == el.ID
}

// finishSyncActivity completes an activity that ran synchronously.
func (rt *runtime) finishSyncActivity(tok *store.Token, el *bpmn.Element) {
	rt.emit(store.HistElementCompleted, el.ID, nil)
	if err := rt.applyOutputs(el, tok); err != nil {
		rt.raiseIncident(tok, "", fmt.Sprintf("output mapping on %s: %v", el.ID, err))
		return
	}
	rt.clearInputLocals(el, tok)
	rt.completeActivity(tok)
}

// ---- user task -----------------------------------------------------------------

func (rt *runtime) userTask(tok *store.Token, el *bpmn.Element) error {
	env := rt.env(tok)
	assignee, err := evalFieldString(el.Assignee, env)
	if err != nil {
		rt.raiseIncident(tok, "", fmt.Sprintf("assignee on %s: %v", el.ID, err))
		return nil
	}
	task := &store.Task{
		ID:            rt.e.newID("task"),
		InstanceID:    rt.inst.ID,
		TokenID:       tok.ID,
		ElementID:     el.ID,
		DefinitionKey: rt.inst.DefinitionKey,
		Name:          orDefault(el.Name, el.ID),
		State:         store.TaskCreated,
		Assignee:      assignee,
		FormKey:       el.FormKey,
		CreatedAt:     rt.e.now(),
	}
	for _, cu := range el.CandidateUsers {
		v, err := evalFieldString(cu, env)
		if err == nil && v != "" {
			task.CandidateUsers = append(task.CandidateUsers, v)
		}
	}
	for _, cg := range el.CandidateGroups {
		v, err := evalFieldString(cg, env)
		if err == nil && v != "" {
			task.CandidateGroups = append(task.CandidateGroups, v)
		}
	}
	if el.DueDate != "" {
		if due, err := rt.resolveDue(el.DueDate, env); err == nil {
			task.DueAt = due
		} else {
			rt.e.log.Warn("bad dueDate", "element", el.ID, "error", err)
		}
	}
	if el.Priority != "" {
		if v, err := evalField(el.Priority, env); err == nil {
			if f, ok := v.(float64); ok {
				task.Priority = int(f)
			} else {
				task.Priority = atoiSafe(expr.Stringify(v))
			}
		}
	}
	if err := rt.e.st.PutTask(task); err != nil {
		return err
	}
	rt.registerBoundaries(tok, el)
	tok.State = store.TokenWaitTask
	tok.WaitRef = task.ID
	rt.e.metrics.TasksCreated.Add(1)
	rt.emit(store.HistTaskCreated, el.ID, map[string]any{"taskId": task.ID, "assignee": assignee, "name": task.Name})
	return nil
}

// resolveDue interprets a due date value: ISO-8601 duration (relative to
// now), a date string, or a time value from an expression.
func (rt *runtime) resolveDue(src string, env map[string]any) (time.Time, error) {
	v, err := evalField(src, env)
	if err != nil {
		return time.Time{}, err
	}
	switch x := v.(type) {
	case time.Time:
		return x, nil
	case string:
		if d, err := expr.ParseISODuration(x); err == nil {
			return rt.e.now().Add(d), nil
		}
		return expr.ParseTime(x)
	}
	return time.Time{}, fmt.Errorf("cannot interpret %v as due date", v)
}

// ---- service task -----------------------------------------------------------------

func (rt *runtime) serviceTask(tok *store.Token, el *bpmn.Element) error {
	// External worker topic wins.
	if el.Topic != "" {
		ext := &store.ExternalTask{
			ID:         rt.e.newID("ext"),
			InstanceID: rt.inst.ID,
			TokenID:    tok.ID,
			ElementID:  el.ID,
			Topic:      el.Topic,
			State:      store.ExternalPending,
			Variables:  rt.env(tok),
			Retries:    orDefaultInt(el.Retries, 3),
			CreatedAt:  rt.e.now(),
		}
		if err := rt.e.st.PutExternalTask(ext); err != nil {
			return err
		}
		rt.registerBoundaries(tok, el)
		tok.State = store.TokenWaitExternal
		tok.WaitRef = ext.ID
		rt.e.metrics.ExternalCreated.Add(1)
		return nil
	}

	// Inline expression evaluation.
	if el.Expression != "" {
		v, err := expr.Eval(el.Expression, rt.env(tok))
		if err != nil {
			rt.handleActivityFailure(tok, el, err)
			return nil
		}
		if el.ResultVar != "" {
			rt.inst.Variables[el.ResultVar] = v
		}
		if el.Type == bpmn.TypeSendTask {
			rt.throwMessage(el)
		}
		rt.finishSyncActivity(tok, el)
		return nil
	}

	// Registered Go handler.
	if el.TaskType != "" {
		h := rt.e.handler(el.TaskType)
		if h == nil {
			rt.raiseIncident(tok, "", fmt.Sprintf("no handler registered for service task type %q", el.TaskType))
			return nil
		}
		out, err := h(Context{
			InstanceID:  rt.inst.ID,
			BusinessKey: rt.inst.BusinessKey,
			ElementID:   el.ID,
			ElementName: el.Name,
			Variables:   rt.env(tok),
		})
		if err != nil {
			rt.handleActivityFailure(tok, el, err)
			return nil
		}
		for k, v := range out {
			rt.inst.Variables[k] = expr.Normalize(v)
		}
		if el.ResultVar != "" && out != nil {
			if v, ok := out[el.ResultVar]; ok {
				rt.inst.Variables[el.ResultVar] = expr.Normalize(v)
			}
		}
		rt.e.metrics.ServiceTasksRun.Add(1)
		if el.Type == bpmn.TypeSendTask {
			rt.throwMessage(el)
		}
		rt.finishSyncActivity(tok, el)
		return nil
	}

	// A send task with only a message reference just throws it.
	if el.Type == bpmn.TypeSendTask && el.Event != nil {
		rt.throwMessage(el)
		rt.finishSyncActivity(tok, el)
		return nil
	}

	rt.raiseIncident(tok, "", fmt.Sprintf("service task %s has no type, topic or expression", el.ID))
	return nil
}

// handleActivityFailure implements the retry → incident cycle and BPMN
// error routing for synchronous activity failures.
func (rt *runtime) handleActivityFailure(tok *store.Token, el *bpmn.Element, err error) {
	if berr, ok := err.(*BPMNError); ok {
		rt.throwError(tok, berr.Code, berr.Message)
		return
	}
	attempts := 0
	if tok.LocalVars != nil {
		if f, ok := tok.LocalVars["__attempts"].(float64); ok {
			attempts = int(f)
		}
	}
	attempts++
	maxRetries := orDefaultInt(el.Retries, 3)
	if attempts < maxRetries {
		if tok.LocalVars == nil {
			tok.LocalVars = map[string]any{}
		}
		tok.LocalVars["__attempts"] = float64(attempts)
		backoff := rt.e.retryBackoff << (attempts - 1)
		job := &store.Job{
			ID:         rt.e.newID("job"),
			Kind:       store.JobRetry,
			InstanceID: rt.inst.ID,
			TokenID:    tok.ID,
			ElementID:  el.ID,
			DueAt:      rt.e.now().Add(backoff),
			Retries:    maxRetries - attempts,
			CreatedAt:  rt.e.now(),
		}
		if putErr := rt.e.st.PutJob(job); putErr != nil {
			rt.raiseIncident(tok, "", fmt.Sprintf("%v (retry scheduling also failed: %v)", err, putErr))
			return
		}
		tok.State = store.TokenWaitRetry
		tok.WaitRef = job.ID
		rt.e.log.Warn("activity failed, retry scheduled", "element", el.ID, "attempt", attempts, "backoff", backoff, "error", err)
		return
	}
	rt.raiseIncident(tok, "", fmt.Sprintf("activity %s failed after %d attempts: %v", el.ID, attempts, err))
}

// ---- script & rule tasks -----------------------------------------------------------------

func (rt *runtime) scriptTask(tok *store.Token, el *bpmn.Element) error {
	if el.Expression == "" {
		rt.finishSyncActivity(tok, el)
		return nil
	}
	v, err := expr.Eval(el.Expression, rt.env(tok))
	if err != nil {
		rt.handleActivityFailure(tok, el, err)
		return nil
	}
	if el.ResultVar != "" {
		rt.inst.Variables[el.ResultVar] = v
	}
	rt.finishSyncActivity(tok, el)
	return nil
}

func (rt *runtime) businessRuleTask(tok *store.Token, el *bpmn.Element) error {
	if rt.e.decisions == nil {
		rt.raiseIncident(tok, "", fmt.Sprintf("business rule task %s: no decision evaluator configured", el.ID))
		return nil
	}
	v, err := rt.e.decisions.EvaluateDecision(el.DecisionRef, rt.env(tok))
	if err != nil {
		rt.handleActivityFailure(tok, el, err)
		return nil
	}
	target := orDefault(el.ResultVar, "decisionResult")
	rt.inst.Variables[target] = expr.Normalize(v)
	rt.e.metrics.DecisionsEvaluated.Add(1)
	rt.finishSyncActivity(tok, el)
	return nil
}

// ---- receive task -----------------------------------------------------------------------

func (rt *runtime) receiveTask(tok *store.Token, el *bpmn.Element) error {
	rt.registerBoundaries(tok, el)
	tok.State = store.TokenWaitMessage
	return rt.createEventWait(tok, el, el.Event, true)
}

// ---- sub-process & call activity -----------------------------------------------------------

func (rt *runtime) enterSubProcess(tok *store.Token, el *bpmn.Element) error {
	start := el.Sub.NoneStartEvent()
	if start == nil {
		rt.raiseIncident(tok, "", fmt.Sprintf("sub-process %s has no none start event", el.ID))
		return nil
	}
	rt.registerBoundaries(tok, el)
	tok.State = store.TokenWaitChild
	tok.WaitRef = scopeWaitRef
	childPath := append(append([]string(nil), tok.ScopePath...), el.ID)
	childOwners := append(append([]string(nil), tok.ScopeOwners...), tok.ID)
	rt.registerEventSubprocesses(el.Sub, childPath, tok.ID)
	rt.spawnToken(start.ID, childPath, childOwners, "", nil)
	return nil
}

func (rt *runtime) callActivity(tok *store.Token, el *bpmn.Element) error {
	def, err := rt.e.st.LatestDefinition(el.CalledElement)
	if err != nil {
		rt.raiseIncident(tok, "", fmt.Sprintf("call activity %s: unknown process %q", el.ID, el.CalledElement))
		return nil
	}
	env := rt.env(tok)
	childVars := map[string]any{}
	if len(el.Inputs) == 0 {
		// No explicit mappings: inherit a copy of all variables.
		childVars = cloneLocals(rt.inst.Variables)
	} else {
		for _, m := range el.Inputs {
			v, err := expr.Eval(m.Source, env)
			if err != nil {
				rt.raiseIncident(tok, "", fmt.Sprintf("call activity input %s: %v", m.Target, err))
				return nil
			}
			childVars[m.Target] = v
		}
	}
	childID := rt.e.newID("inst")
	rt.registerBoundaries(tok, el)
	tok.State = store.TokenWaitChild
	tok.WaitRef = childID

	e := rt.e
	defID := def.ID
	parentID := rt.inst.ID
	tokID := tok.ID
	businessKey := rt.inst.BusinessKey
	rt.continuations = append(rt.continuations, func() {
		if _, err := e.startChildInstance(defID, childID, businessKey, childVars, parentID, tokID); err != nil {
			e.log.Error("call activity child start failed", "definition", defID, "error", err)
			_ = e.resume(parentID, func(prt *runtime) (bool, error) {
				t := prt.inst.Tokens[tokID]
				if t != nil && t.WaitRef == childID {
					prt.raiseIncident(t, "", fmt.Sprintf("child instance start failed: %v", err))
				}
				return true, nil
			})
		}
	})
	return nil
}

// ---- boundary & event sub-process registration ------------------------------------------------

// registerBoundaries creates jobs/subscriptions for all boundary events
// attached to the activity the token occupies.
func (rt *runtime) registerBoundaries(tok *store.Token, el *bpmn.Element) {
	c := rt.scope(tok)
	for _, b := range c.BoundaryEvents(el.ID) {
		switch b.Event.Kind {
		case bpmn.KindTimer, bpmn.KindMessage, bpmn.KindSignal:
			if err := rt.createEventWait(tok, b, b.Event, false); err != nil {
				rt.e.log.Error("boundary registration failed", "boundary", b.ID, "error", err)
			}
		}
		// Error boundaries need no registration: errors route at throw
		// time.
	}
}

// cancelBoundaries removes all event waits registered for a token.
func (rt *runtime) cancelBoundaries(tok *store.Token) {
	rt.dropTokenEventWaits(tok.ID)
}

// registerEventSubprocesses installs waits for event sub-process start
// events in a scope. ownerTokenID ties their lifetime to the scope owner
// ("" = process root).
func (rt *runtime) registerEventSubprocesses(c *bpmn.Container, scopePath []string, ownerTokenID string) {
	for _, id := range c.Order {
		es := c.Elements[id]
		if es == nil || es.Type != bpmn.TypeSubProcess || !es.TriggeredByEvent || es.Sub == nil {
			continue
		}
		for _, se := range es.Sub.StartEvents() {
			if se.Event == nil {
				continue
			}
			switch se.Event.Kind {
			case bpmn.KindMessage:
				sub := &store.Subscription{
					ID:             rt.e.newID("sub"),
					Kind:           store.SubMessage,
					Name:           se.Event.Message,
					CorrelationKey: rt.inst.BusinessKey,
					InstanceID:     rt.inst.ID,
					TokenID:        ownerTokenID,
					ElementID:      se.ID,
					CreatedAt:      rt.e.now(),
				}
				if err := rt.e.st.PutSubscription(sub); err != nil {
					rt.e.log.Error("event sub-process subscription failed", "error", err)
				}
			case bpmn.KindSignal:
				sub := &store.Subscription{
					ID:         rt.e.newID("sub"),
					Kind:       store.SubSignal,
					Name:       se.Event.Signal,
					InstanceID: rt.inst.ID,
					TokenID:    ownerTokenID,
					ElementID:  se.ID,
					CreatedAt:  rt.e.now(),
				}
				if err := rt.e.st.PutSubscription(sub); err != nil {
					rt.e.log.Error("event sub-process subscription failed", "error", err)
				}
			case bpmn.KindTimer:
				due, repeats, interval, err := rt.e.timerSchedule(se.Event, rt.inst.Variables)
				if err != nil {
					rt.e.log.Error("event sub-process timer invalid", "start", se.ID, "error", err)
					continue
				}
				job := &store.Job{
					ID:         rt.e.newID("job"),
					Kind:       store.JobTimer,
					InstanceID: rt.inst.ID,
					TokenID:    ownerTokenID,
					ElementID:  se.ID,
					DueAt:      due,
					Repeats:    repeats,
					Interval:   interval,
					CreatedAt:  rt.e.now(),
				}
				if err := rt.e.st.PutJob(job); err != nil {
					rt.e.log.Error("event sub-process timer failed", "error", err)
				}
			}
		}
	}
}

// ---- io mappings -------------------------------------------------------------------------------

func (rt *runtime) applyInputs(el *bpmn.Element, tok *store.Token) error {
	if len(el.Inputs) == 0 || el.Type == bpmn.TypeCallActivity {
		return nil
	}
	env := rt.env(tok)
	for _, m := range el.Inputs {
		v, err := expr.Eval(m.Source, env)
		if err != nil {
			return err
		}
		if tok.LocalVars == nil {
			tok.LocalVars = map[string]any{}
		}
		tok.LocalVars[m.Target] = v
	}
	return nil
}

func (rt *runtime) applyOutputs(el *bpmn.Element, tok *store.Token) error {
	if len(el.Outputs) == 0 || el.Type == bpmn.TypeCallActivity {
		return nil
	}
	env := rt.env(tok)
	for _, m := range el.Outputs {
		v, err := expr.Eval(m.Source, env)
		if err != nil {
			return err
		}
		rt.inst.Variables[m.Target] = v
	}
	return nil
}

func (rt *runtime) clearInputLocals(el *bpmn.Element, tok *store.Token) {
	if tok.LocalVars == nil {
		return
	}
	for _, m := range el.Inputs {
		delete(tok.LocalVars, m.Target)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orDefaultInt(n, def int) int {
	if n <= 0 {
		return def
	}
	return n
}
