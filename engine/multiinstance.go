package engine

import (
	"fmt"

	"github.com/olbboy/fliable/bpmn"
	"github.com/olbboy/fliable/expr"
	"github.com/olbboy/fliable/store"
)

// startMultiInstance turns the arriving token into the loop coordinator
// and spawns child tokens per item (all at once for parallel, one at a
// time for sequential).
func (rt *runtime) startMultiInstance(tok *store.Token, el *bpmn.Element) error {
	mi := el.MultiInstance
	env := rt.env(tok)

	var items []any
	switch {
	case mi.Collection != "":
		v, err := expr.Eval(mi.Collection, env)
		if err != nil {
			rt.raiseIncident(tok, "", fmt.Sprintf("multi-instance collection on %s: %v", el.ID, err))
			return nil
		}
		list, ok := v.([]any)
		if !ok {
			rt.raiseIncident(tok, "", fmt.Sprintf("multi-instance collection on %s is %T, want list", el.ID, v))
			return nil
		}
		items = list
	case mi.Cardinality != "":
		v, err := expr.Eval(mi.Cardinality, env)
		if err != nil {
			rt.raiseIncident(tok, "", fmt.Sprintf("multi-instance cardinality on %s: %v", el.ID, err))
			return nil
		}
		f, ok := v.(float64)
		if !ok || f < 0 {
			rt.raiseIncident(tok, "", fmt.Sprintf("multi-instance cardinality on %s = %v, want a non-negative number", el.ID, v))
			return nil
		}
		items = make([]any, int(f))
		for i := range items {
			items[i] = float64(i)
		}
	default:
		rt.raiseIncident(tok, "", fmt.Sprintf("multi-instance on %s has neither collection nor cardinality", el.ID))
		return nil
	}

	rt.emit(store.HistElementActivated, el.ID, map[string]any{
		"multiInstance": true, "instances": float64(len(items)), "sequential": mi.Sequential,
	})

	if len(items) == 0 {
		// Empty loop completes immediately.
		if mi.OutputCollection != "" {
			rt.inst.Variables[mi.OutputCollection] = []any{}
		}
		rt.emit(store.HistElementCompleted, el.ID, map[string]any{"instances": 0.0})
		rt.completeActivity(tok)
		return nil
	}

	state := &store.MultiInstanceState{TokenID: tok.ID, Total: len(items), Items: items}
	if rt.inst.Multi == nil {
		rt.inst.Multi = map[string]*store.MultiInstanceState{}
	}
	rt.inst.Multi[tok.ID] = state
	tok.State = store.TokenWaitMulti
	tok.WaitRef = ""
	rt.registerBoundaries(tok, el)

	if mi.Sequential {
		rt.spawnMIChild(tok, el, state, 0)
		state.NextIndex = 1
		return nil
	}
	for i := range items {
		rt.spawnMIChild(tok, el, state, i)
	}
	state.NextIndex = len(items)
	return nil
}

func (rt *runtime) spawnMIChild(parent *store.Token, el *bpmn.Element, state *store.MultiInstanceState, index int) {
	locals := map[string]any{
		miChildFlag:   el.ID,
		"loopCounter": float64(index),
	}
	if el.MultiInstance.ElementVariable != "" {
		locals[el.MultiInstance.ElementVariable] = state.Items[index]
	}
	child := rt.spawnToken(el.ID, parent.ScopePath, parent.ID, locals)
	_ = child
}

// miChildCompleted collects one finished loop iteration and decides
// whether to spawn the next, finish the loop, or keep waiting.
func (rt *runtime) miChildCompleted(child *store.Token) {
	parentID := child.Parent
	state := rt.inst.Multi[parentID]
	parent := rt.inst.Tokens[parentID]
	el := rt.element(child)
	delete(rt.inst.Tokens, child.ID)
	if state == nil || parent == nil || el == nil || el.MultiInstance == nil {
		return
	}
	mi := el.MultiInstance

	if mi.OutputElement != "" {
		v, err := expr.Eval(mi.OutputElement, rt.env(child))
		if err != nil {
			rt.raiseIncident(parent, "", fmt.Sprintf("multi-instance output on %s: %v", el.ID, err))
			return
		}
		state.Outputs = append(state.Outputs, v)
	}
	state.Completed++

	// Completion condition can cut the loop short.
	if mi.CompletionCondition != "" {
		env := rt.env(parent)
		env["nrOfInstances"] = float64(state.Total)
		env["nrOfCompletedInstances"] = float64(state.Completed)
		env["nrOfActiveInstances"] = float64(rt.countMIChildren(parentID))
		done, err := expr.EvalBool(mi.CompletionCondition, env)
		if err != nil {
			rt.raiseIncident(parent, "", fmt.Sprintf("completion condition on %s: %v", el.ID, err))
			return
		}
		if done {
			rt.finishMultiInstance(parent, el, state, true)
			return
		}
	}

	if state.Completed >= state.Total {
		rt.finishMultiInstance(parent, el, state, false)
		return
	}
	if mi.Sequential && state.NextIndex < state.Total {
		rt.spawnMIChild(parent, el, state, state.NextIndex)
		state.NextIndex++
	}
}

func (rt *runtime) countMIChildren(parentID string) int {
	n := 0
	for _, t := range rt.inst.Tokens {
		if t.Parent == parentID {
			n++
		}
	}
	return n
}

// finishMultiInstance ends the loop: cancels stragglers (early
// completion), publishes outputs and moves the coordinator token on.
func (rt *runtime) finishMultiInstance(parent *store.Token, el *bpmn.Element, state *store.MultiInstanceState, early bool) {
	for id, t := range rt.inst.Tokens {
		if t.Parent == parent.ID {
			rt.cancelActivity(t)
			delete(rt.inst.Tokens, id)
		}
	}
	if el.MultiInstance.OutputCollection != "" {
		out := state.Outputs
		if out == nil {
			out = []any{}
		}
		rt.inst.Variables[el.MultiInstance.OutputCollection] = out
	}
	delete(rt.inst.Multi, parent.ID)
	parent.State = store.TokenActive
	parent.WaitRef = ""
	rt.emit(store.HistElementCompleted, el.ID, map[string]any{
		"multiInstance": true, "completed": float64(state.Completed), "early": early,
	})
	if err := rt.applyOutputs(el, parent); err != nil {
		rt.raiseIncident(parent, "", fmt.Sprintf("output mapping on %s: %v", el.ID, err))
		return
	}
	rt.cancelBoundaries(parent)
	rt.completeActivity(parent)
}
