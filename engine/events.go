package engine

import (
	"fmt"

	"github.com/olbboy/fliable/bpmn"
	"github.com/olbboy/fliable/store"
)

// eventTriggered routes a fired subscription or timer to the right
// behavior: intermediate catch, receive task, event-based gateway target,
// boundary event or event sub-process start. Returns false when the wait
// state no longer exists (already resolved by another path).
func (rt *runtime) eventTriggered(tokenID, elementID, refID string) (bool, error) {
	el := rt.pd.proc.FindElement(elementID)
	if el == nil {
		return false, fmt.Errorf("engine: triggered element %q not found", elementID)
	}

	switch el.Type {
	case bpmn.TypeBoundaryEvent:
		return rt.boundaryTriggered(tokenID, el, refID)
	case bpmn.TypeStartEvent:
		return rt.eventSubprocessTriggered(el, refID)
	}

	tok := rt.inst.Tokens[tokenID]
	if tok == nil {
		return false, nil
	}

	// Event-based gateway: the token still sits on the gateway; the
	// triggered element is one of its targets.
	if tok.State == store.TokenWaitEvents {
		rt.dropTokenEventWaits(tok.ID)
		tok.ElementID = elementID
		tok.State = store.TokenActive
		tok.WaitRef = ""
		rt.emit(store.HistElementActivated, elementID, nil)
		return true, rt.leave(tok, el)
	}

	// Plain catch event or receive task.
	if tok.ElementID != elementID {
		return false, nil
	}
	switch tok.State {
	case store.TokenWaitMessage, store.TokenWaitSignal, store.TokenWaitTimer:
		rt.dropTokenEventWaits(tok.ID)
		tok.State = store.TokenActive
		tok.WaitRef = ""
		if el.Type == bpmn.TypeReceiveTask {
			rt.emit(store.HistElementCompleted, el.ID, nil)
			if err := rt.applyOutputs(el, tok); err != nil {
				rt.raiseIncident(tok, "", fmt.Sprintf("output mapping on %s: %v", el.ID, err))
				return true, nil
			}
			rt.clearInputLocals(el, tok)
			rt.completeActivity(tok)
			return true, nil
		}
		return true, rt.leave(tok, el)
	}
	return false, nil
}

// boundaryTriggered fires a boundary event attached to the activity the
// token occupies.
func (rt *runtime) boundaryTriggered(tokenID string, bel *bpmn.Element, refID string) (bool, error) {
	tok := rt.inst.Tokens[tokenID]
	if tok == nil || tok.ElementID != bel.AttachedTo {
		return false, nil
	}
	rt.emit(store.HistElementActivated, bel.ID, map[string]any{"boundary": true, "interrupting": bel.CancelActivity})

	if bel.CancelActivity {
		rt.cancelActivity(tok)
		rt.moveToBoundary(tok, bel)
		return true, nil
	}

	// Non-interrupting: spawn a parallel token at the boundary event.
	nt := rt.spawnToken(bel.ID, tok.ScopePath, "", nil)
	nt.ArrivedFlow = ""
	return true, nil
}

// cancelActivity cancels an in-flight activity occupied by tok: its wait
// record, boundary registrations, nested scope tokens and child
// instances. The token itself survives (the caller repositions it).
func (rt *runtime) cancelActivity(tok *store.Token) {
	el := rt.element(tok)
	rt.cancelWaits(tok)
	if el != nil && el.Type == bpmn.TypeSubProcess {
		rt.killScopeTokens(append(append([]string(nil), tok.ScopePath...), el.ID), "")
	}
	if el != nil {
		rt.emit(store.HistElementCompleted, el.ID, map[string]any{"canceled": true})
	}
}

// moveToBoundary repositions a token onto a boundary event and lets it
// continue from there.
func (rt *runtime) moveToBoundary(tok *store.Token, bel *bpmn.Element) {
	tok.ElementID = bel.ID
	tok.State = store.TokenActive
	tok.WaitRef = ""
	tok.LocalVars = nil
}

// eventSubprocessTriggered starts an event sub-process from its typed
// start event.
func (rt *runtime) eventSubprocessTriggered(startEl *bpmn.Element, refID string) (bool, error) {
	// Locate the event sub-process containing this start event and its
	// scope path.
	path, esID := rt.findEventSubprocess(startEl.ID)
	if esID == "" {
		return false, fmt.Errorf("engine: start event %q is not inside an event sub-process", startEl.ID)
	}
	interrupting := startEl.CancelActivity

	rt.emit(store.HistElementActivated, esID, map[string]any{"eventSubprocess": true, "interrupting": interrupting})
	if interrupting {
		// Kill every token in the enclosing scope except those already in
		// the event sub-process, then remove the one-shot trigger.
		rt.killScopeTokens(path, esID)
		if refID != "" {
			_ = rt.e.st.DeleteSubscription(refID)
		}
	}
	rt.spawnToken(startEl.ID, append(append([]string(nil), path...), esID), "", nil)
	return true, nil
}

// findEventSubprocess returns the scope path and element ID of the event
// sub-process whose start event has the given ID.
func (rt *runtime) findEventSubprocess(startID string) ([]string, string) {
	var walk func(c *bpmn.Container, path []string) ([]string, string)
	walk = func(c *bpmn.Container, path []string) ([]string, string) {
		for _, id := range c.Order {
			el := c.Elements[id]
			if el == nil || el.Sub == nil {
				continue
			}
			if el.TriggeredByEvent {
				if _, ok := el.Sub.Elements[startID]; ok {
					return path, el.ID
				}
			}
			if p, es := walk(el.Sub, append(path, el.ID)); es != "" {
				return p, es
			}
		}
		return nil, ""
	}
	return walk(&rt.pd.proc.Container, nil)
}

// timerFired handles a claimed timer job inside a resume.
func (rt *runtime) timerFired(job *store.Job) (bool, error) {
	rt.emit(store.HistTimerFired, job.ElementID, nil)
	rt.e.metrics.TimersFired.Add(1)

	// Repeating timers (non-interrupting boundary / event sub-process
	// cycles) reschedule before routing.
	el := rt.pd.proc.FindElement(job.ElementID)
	if el != nil && job.Interval > 0 && (job.Repeats > 1 || job.Repeats == -1) {
		nonInterrupting := (el.Type == bpmn.TypeBoundaryEvent && !el.CancelActivity) ||
			(el.Type == bpmn.TypeStartEvent && !el.CancelActivity)
		if nonInterrupting {
			next := *job
			next.ID = rt.e.newID("job")
			next.DueAt = job.DueAt.Add(job.Interval)
			if next.Repeats > 0 {
				next.Repeats--
			}
			next.CreatedAt = rt.e.now()
			if err := rt.e.st.PutJob(&next); err != nil {
				rt.e.log.Error("timer reschedule failed", "error", err)
			}
		}
	}
	return rt.eventTriggered(job.TokenID, job.ElementID, job.ID)
}
