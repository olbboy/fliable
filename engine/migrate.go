package engine

import (
	"fmt"

	"github.com/olbboy/fliable/bpmn"
	"github.com/olbboy/fliable/expr"
	"github.com/olbboy/fliable/store"
)

// MigrationPlan describes how to move one live instance onto another
// definition version. Elements keep their IDs by default; ActivityMap
// overrides individual IDs (old -> new) for renamed elements.
//
// Migration is validated completely before anything is written: every
// token must land on an element that exists in the target with the same
// type and a compatible wait state, through a scope path that still
// exists. DryRun runs exactly that validation and reports the outcome
// without applying — the safe preflight the classic engines lack.
type MigrationPlan struct {
	// TargetDefinitionID is the exact definition version to move to.
	TargetDefinitionID string `json:"targetDefinitionId"`
	// ActivityMap renames element IDs (old -> new). Unlisted elements
	// map to themselves.
	ActivityMap map[string]string `json:"activityMap,omitempty"`
	// VarTransforms sets/derives variables during migration: name ->
	// expression evaluated against the pre-migration variables.
	VarTransforms map[string]string `json:"varTransforms,omitempty"`
	// DryRun validates and reports without changing anything.
	DryRun bool `json:"dryRun,omitempty"`
}

// TokenMove is one token's relocation in a migration report.
type TokenMove struct {
	TokenID string `json:"tokenId"`
	From    string `json:"from"`
	To      string `json:"to"`
	State   string `json:"state"`
}

// MigrationReport is the result of validating (and possibly applying) a
// migration plan.
type MigrationReport struct {
	InstanceID         string      `json:"instanceId"`
	TargetDefinitionID string      `json:"targetDefinitionId"`
	TokenMoves         []TokenMove `json:"tokenMoves"`
	Issues             []string    `json:"issues,omitempty"`
	Applied            bool        `json:"applied"`
}

// MigrateInstance moves a live (active or suspended) instance onto the
// target definition. On any validation issue nothing is applied and the
// issues come back in the report; with DryRun the report always comes
// back unapplied.
func (e *Engine) MigrateInstance(instanceID string, plan MigrationPlan) (*MigrationReport, error) {
	if plan.TargetDefinitionID == "" {
		return nil, fmt.Errorf("engine: migration needs targetDefinitionId")
	}
	target, err := e.definition(plan.TargetDefinitionID)
	if err != nil {
		return nil, fmt.Errorf("engine: target definition: %w", err)
	}

	mu := e.lockFor(instanceID)
	mu.Lock()
	defer mu.Unlock()

	inst, err := e.st.GetInstance(instanceID)
	if err != nil {
		return nil, err
	}
	if inst.State != store.InstanceActive {
		return nil, fmt.Errorf("engine: instance %s is %s", instanceID, inst.State)
	}
	if inst.DefinitionID == plan.TargetDefinitionID {
		return nil, fmt.Errorf("engine: instance %s already runs %s", instanceID, plan.TargetDefinitionID)
	}

	mapID := func(id string) string {
		if to, ok := plan.ActivityMap[id]; ok {
			return to
		}
		return id
	}

	report := &MigrationReport{InstanceID: instanceID, TargetDefinitionID: plan.TargetDefinitionID}
	issue := func(format string, args ...any) {
		report.Issues = append(report.Issues, fmt.Sprintf(format, args...))
	}

	// ---- validate every token against the target graph -------------------
	for _, tok := range inst.Tokens {
		to := mapID(tok.ElementID)
		el := target.proc.FindElement(to)
		if el == nil {
			issue("token %s: element %q does not exist in target", tok.ID, to)
			continue
		}
		// Scope path must survive the mapping.
		newPath := make([]string, len(tok.ScopePath))
		ok := true
		c := &target.proc.Container
		for i, sc := range tok.ScopePath {
			newPath[i] = mapID(sc)
			sub := c.Elements[newPath[i]]
			if sub == nil || sub.Sub == nil {
				issue("token %s: scope %q does not exist in target", tok.ID, newPath[i])
				ok = false
				break
			}
			c = sub.Sub
		}
		if !ok {
			continue
		}
		if c.Elements[to] == nil {
			issue("token %s: element %q is not inside scope %v in target", tok.ID, to, newPath)
			continue
		}

		// The wait state must stay executable on the new element.
		cur := currentElementFor(e, inst, tok)
		if cur != nil && cur.Type != el.Type {
			issue("token %s: type change %s -> %s is not migratable", tok.ID, cur.Type, el.Type)
			continue
		}
		switch tok.State {
		case store.TokenWaitTask:
			if el.Type != bpmn.TypeUserTask && el.Agent == nil {
				issue("token %s: waits on a user task but %q is a %s", tok.ID, to, el.Type)
				continue
			}
		case store.TokenWaitMessage:
			if el.Event == nil || el.Event.Kind != bpmn.KindMessage {
				issue("token %s: waits on a message but %q has no message definition", tok.ID, to)
				continue
			}
		case store.TokenWaitSignal:
			if el.Event == nil || el.Event.Kind != bpmn.KindSignal {
				issue("token %s: waits on a signal but %q has no signal definition", tok.ID, to)
				continue
			}
		case store.TokenWaitMulti:
			if el.MultiInstance == nil {
				issue("token %s: multi-instance loop but %q is not multi-instance in target", tok.ID, to)
				continue
			}
		}
		report.TokenMoves = append(report.TokenMoves, TokenMove{
			TokenID: tok.ID, From: tok.ElementID, To: to, State: string(tok.State),
		})
	}

	// Validate variable transforms before touching state.
	newVars := map[string]any{}
	for name, src := range plan.VarTransforms {
		v, err := expr.Eval(src, inst.Variables)
		if err != nil {
			issue("varTransforms.%s: %v", name, err)
			continue
		}
		newVars[name] = v
	}

	if len(report.Issues) > 0 || plan.DryRun {
		return report, nil
	}

	// ---- apply ------------------------------------------------------------
	fromDefID := inst.DefinitionID
	for _, tok := range inst.Tokens {
		to := mapID(tok.ElementID)
		tok.ElementID = to
		for i, sc := range tok.ScopePath {
			tok.ScopePath[i] = mapID(sc)
		}
	}
	inst.DefinitionID = target.def.ID
	inst.DefinitionKey = target.def.Key
	for name, v := range newVars {
		inst.Variables[name] = expr.Normalize(v)
	}

	// Carry side records over: open tasks, timer/retry jobs, message and
	// signal subscriptions. Names refresh from the target model so a
	// renamed message correlates correctly after migration.
	if tasks, err := e.st.ListTasks(store.TaskFilter{InstanceID: instanceID, State: store.TaskCreated}); err == nil {
		for _, t := range tasks {
			t.ElementID = mapID(t.ElementID)
			t.DefinitionKey = target.def.Key
			if el := target.proc.FindElement(t.ElementID); el != nil {
				if el.Name != "" {
					t.Name = el.Name
				}
				if el.FormKey != "" {
					t.FormKey = el.FormKey
				}
			}
			if err := e.st.PutTask(t); err != nil {
				return nil, err
			}
		}
	}
	if jobs, err := e.st.ListJobs(instanceID); err == nil {
		for _, j := range jobs {
			j.ElementID = mapID(j.ElementID)
			if err := e.st.PutJob(j); err != nil {
				return nil, err
			}
		}
	}
	if subs, err := e.st.ListSubscriptions(store.SubscriptionFilter{InstanceID: instanceID}); err == nil {
		for _, sub := range subs {
			sub.ElementID = mapID(sub.ElementID)
			if el := target.proc.FindElement(sub.ElementID); el != nil && el.Event != nil {
				switch sub.Kind {
				case store.SubMessage:
					if el.Event.Message != "" {
						sub.Name = el.Event.Message
					}
				case store.SubSignal:
					if el.Event.Signal != "" {
						sub.Name = el.Event.Signal
					}
				}
			}
			if err := e.st.PutSubscription(sub); err != nil {
				return nil, err
			}
		}
	}

	if err := e.st.PutInstance(inst); err != nil {
		return nil, err
	}
	rt := &runtime{e: e, pd: target, inst: inst}
	rt.emit(store.HistInstanceMigrated, "", map[string]any{
		"fromDefinitionId": fromDefID,
		"toDefinitionId":   target.def.ID,
		"tokens":           float64(len(report.TokenMoves)),
	})
	report.Applied = true
	e.log.Info("instance migrated", "instance", instanceID, "from", fromDefID, "to", target.def.ID)
	return report, nil
}

// currentElementFor resolves a token's element in the instance's current
// definition (nil when the definition or element is gone — validation
// then relies on the wait-state checks alone).
func currentElementFor(e *Engine, inst *store.Instance, tok *store.Token) *bpmn.Element {
	pd, err := e.definition(inst.DefinitionID)
	if err != nil {
		return nil
	}
	return pd.proc.FindElement(tok.ElementID)
}
