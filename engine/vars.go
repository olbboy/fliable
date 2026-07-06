package engine

import (
	"fmt"
	"time"

	"github.com/olbboy/fliable/bpmn"
	"github.com/olbboy/fliable/expr"
	"github.com/olbboy/fliable/store"
)

// env builds the expression environment for a token: instance variables
// overlaid with token-local variables along the parent chain (outermost
// first), plus engine-provided metadata.
func (rt *runtime) env(tok *store.Token) map[string]any {
	out := make(map[string]any, len(rt.inst.Variables)+8)
	for k, v := range rt.inst.Variables {
		out[k] = v
	}
	if tok != nil {
		// Scope owner tokens carry locals visible inside their scope
		// (e.g. the multi-instance element variable on a sub-process
		// iteration); apply outermost scope first.
		for _, ownerID := range tok.ScopeOwners {
			if ownerID == "" {
				continue
			}
			if owner := rt.inst.Tokens[ownerID]; owner != nil {
				applyParentChainLocals(rt, out, owner)
			}
		}
		applyParentChainLocals(rt, out, tok)
	}
	out["instanceId"] = rt.inst.ID
	out["businessKey"] = rt.inst.BusinessKey
	out["definitionKey"] = rt.inst.DefinitionKey
	return out
}

// applyParentChainLocals overlays a token's local variables, walking its
// parent chain so outer locals apply first.
func applyParentChainLocals(rt *runtime, out map[string]any, tok *store.Token) {
	var chain []*store.Token
	for t := tok; t != nil; t = rt.inst.Tokens[t.Parent] {
		chain = append(chain, t)
		if t.Parent == "" {
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		for k, v := range chain[i].LocalVars {
			out[k] = v
		}
	}
}

// timerSchedule computes the first due time (and repetition data for
// cycles) from a timer event definition. Timer values may be expressions.
func (e *Engine) timerSchedule(ev *bpmn.EventDefinition, vars map[string]any) (due time.Time, repeats int, interval time.Duration, err error) {
	if vars == nil {
		vars = map[string]any{}
	}
	now := e.now()
	switch {
	case ev.TimerDuration != "":
		s, ferr := evalFieldString(ev.TimerDuration, vars)
		if ferr != nil {
			return time.Time{}, 0, 0, ferr
		}
		d, derr := expr.ParseISODuration(s)
		if derr != nil {
			return time.Time{}, 0, 0, derr
		}
		return now.Add(d), 0, 0, nil

	case ev.TimerDate != "":
		v, ferr := evalField(ev.TimerDate, vars)
		if ferr != nil {
			return time.Time{}, 0, 0, ferr
		}
		switch x := v.(type) {
		case time.Time:
			return x, 0, 0, nil
		case string:
			t, perr := expr.ParseTime(x)
			if perr != nil {
				return time.Time{}, 0, 0, perr
			}
			return t, 0, 0, nil
		}
		return time.Time{}, 0, 0, fmt.Errorf("engine: timer date %v is not a time", ev.TimerDate)

	case ev.TimerCycle != "":
		s, ferr := evalFieldString(ev.TimerCycle, vars)
		if ferr != nil {
			return time.Time{}, 0, 0, ferr
		}
		n, iv, cerr := expr.ParseTimerCycle(s)
		if cerr != nil {
			return time.Time{}, 0, 0, cerr
		}
		return now.Add(iv), n, iv, nil
	}
	return time.Time{}, 0, 0, fmt.Errorf("engine: empty timer definition")
}
