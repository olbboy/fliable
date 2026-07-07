package engine

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/olbboy/fliable/bpmn"
	"github.com/olbboy/fliable/expr"
	"github.com/olbboy/fliable/store"
)

// maxSteps bounds a single drain to protect against modeling mistakes
// (unguarded loops with no wait states).
const maxSteps = 100000

// runtime executes one instance while its stripe lock is held.
type runtime struct {
	e    *Engine
	pd   *parsedDef
	inst *store.Instance
	// continuations run after the instance lock is released: child
	// instance starts, parent notifications, cross-instance messages.
	continuations []func()
	steps         int
}

// drain advances every active token until the instance is quiescent
// (all tokens parked on wait states) or finished. A suspended instance
// never advances: mutations persist but tokens stay put until resumed.
func (rt *runtime) drain() error {
	if rt.inst.Suspended {
		return nil
	}
	for {
		tok := rt.nextActive()
		if tok == nil {
			return nil
		}
		rt.steps++
		if rt.steps > maxSteps {
			rt.raiseIncident(tok, "", "execution exceeded step limit (possible unguarded loop)")
			return nil
		}
		if err := rt.step(tok); err != nil {
			return err
		}
		if rt.inst.State != store.InstanceActive {
			return nil
		}
	}
}

// nextActive returns an active token deterministically (lowest ID first).
func (rt *runtime) nextActive() *store.Token {
	var best *store.Token
	for _, t := range rt.inst.Tokens {
		if t.State != store.TokenActive {
			continue
		}
		if best == nil || t.ID < best.ID {
			best = t
		}
	}
	return best
}

// ---- model resolution ---------------------------------------------------------

// containerAt resolves the container for a scope path.
func (rt *runtime) containerAt(path []string) *bpmn.Container {
	c := &rt.pd.proc.Container
	for _, id := range path {
		el := c.Elements[id]
		if el == nil || el.Sub == nil {
			return nil
		}
		c = el.Sub
	}
	return c
}

// scope returns the container holding the token's current element.
func (rt *runtime) scope(tok *store.Token) *bpmn.Container {
	return rt.containerAt(tok.ScopePath)
}

// element returns the token's current element.
func (rt *runtime) element(tok *store.Token) *bpmn.Element {
	c := rt.scope(tok)
	if c == nil {
		return nil
	}
	return c.Elements[tok.ElementID]
}

// ---- history --------------------------------------------------------------------

func (rt *runtime) emit(typ, elementID string, detail map[string]any) {
	ev := &store.HistoryEvent{
		InstanceID: rt.inst.ID,
		Time:       rt.e.now(),
		Type:       typ,
		ElementID:  elementID,
		Detail:     detail,
	}
	if err := rt.e.st.AppendHistory(ev); err != nil {
		rt.e.log.Error("history append failed", "error", err)
	}
	rt.e.lmu.RLock()
	listeners := rt.e.listeners
	rt.e.lmu.RUnlock()
	for _, fn := range listeners {
		fn(ev)
	}
}

// ---- stepping --------------------------------------------------------------------

func (rt *runtime) step(tok *store.Token) error {
	el := rt.element(tok)
	if el == nil {
		rt.raiseIncident(tok, "", fmt.Sprintf("token at unknown element %q", tok.ElementID))
		return nil
	}
	switch el.Type {
	case bpmn.TypeStartEvent:
		rt.emit(store.HistElementActivated, el.ID, nil)
		return rt.leave(tok, el)

	case bpmn.TypeEndEvent:
		return rt.endEvent(tok, el)

	case bpmn.TypeUserTask, bpmn.TypeServiceTask, bpmn.TypeScriptTask,
		bpmn.TypeBusinessRuleTask, bpmn.TypeSendTask, bpmn.TypeReceiveTask,
		bpmn.TypeManualTask, bpmn.TypeTask, bpmn.TypeSubProcess, bpmn.TypeCallActivity:
		return rt.activity(tok, el)

	case bpmn.TypeExclusiveGateway:
		rt.emit(store.HistElementActivated, el.ID, nil)
		return rt.exclusiveGateway(tok, el)
	case bpmn.TypeParallelGateway:
		return rt.parallelGateway(tok, el)
	case bpmn.TypeInclusiveGateway:
		return rt.inclusiveGateway(tok, el)
	case bpmn.TypeEventBasedGateway:
		rt.emit(store.HistElementActivated, el.ID, nil)
		return rt.eventBasedGateway(tok, el)

	case bpmn.TypeIntermediateCatchEvent:
		rt.emit(store.HistElementActivated, el.ID, nil)
		return rt.intermediateCatch(tok, el)
	case bpmn.TypeIntermediateThrowEvent:
		return rt.intermediateThrow(tok, el)
	case bpmn.TypeBoundaryEvent:
		// A token sits on a boundary event only right after its trigger;
		// it simply leaves.
		return rt.leave(tok, el)
	}
	rt.raiseIncident(tok, "", fmt.Sprintf("unsupported element type %s", el.Type))
	return nil
}

// leave completes an element and takes its outgoing flows.
func (rt *runtime) leave(tok *store.Token, el *bpmn.Element) error {
	rt.emit(store.HistElementCompleted, el.ID, nil)
	return rt.takeOutgoing(tok, el)
}

// takeOutgoing moves the token over the element's outgoing flows,
// evaluating flow conditions (implicit fork on multiple true flows for
// activities; gateways implement their own selection).
func (rt *runtime) takeOutgoing(tok *store.Token, el *bpmn.Element) error {
	c := rt.scope(tok)
	var flows []*bpmn.SequenceFlow
	for _, fid := range el.Outgoing {
		f := c.Flows[fid]
		if f == nil {
			continue
		}
		if fid == el.DefaultFlow {
			continue
		}
		if f.Condition != "" {
			ok, err := expr.EvalBool(f.Condition, rt.env(tok))
			if err != nil {
				rt.raiseIncident(tok, "", fmt.Sprintf("condition on flow %s: %v", f.ID, err))
				return nil
			}
			if !ok {
				continue
			}
		}
		flows = append(flows, f)
	}
	if len(flows) == 0 && el.DefaultFlow != "" {
		if f := c.Flows[el.DefaultFlow]; f != nil {
			flows = append(flows, f)
		}
	}
	if len(flows) == 0 {
		rt.raiseIncident(tok, "", fmt.Sprintf("no outgoing flow taken from %s", el.ID))
		return nil
	}
	return rt.moveOver(tok, flows)
}

// moveOver advances tok over the first flow and spawns siblings for the
// rest.
func (rt *runtime) moveOver(tok *store.Token, flows []*bpmn.SequenceFlow) error {
	for _, f := range flows[1:] {
		sib := rt.spawnToken(f.TargetRef, tok.ScopePath, tok.ScopeOwners, tok.Parent, nil)
		sib.ArrivedFlow = f.ID
		sib.LocalVars = cloneLocals(tok.LocalVars)
		if err := rt.maybeAsync(sib); err != nil {
			return err
		}
	}
	tok.ElementID = flows[0].TargetRef
	tok.ArrivedFlow = flows[0].ID
	tok.State = store.TokenActive
	tok.WaitRef = ""
	return rt.maybeAsync(tok)
}

// maybeAsync parks the token behind an async continuation job when the
// target element requests it.
func (rt *runtime) maybeAsync(tok *store.Token) error {
	el := rt.element(tok)
	if el == nil || !el.Async {
		return nil
	}
	job := &store.Job{
		ID:         rt.e.newID("job"),
		Kind:       store.JobAsync,
		InstanceID: rt.inst.ID,
		TokenID:    tok.ID,
		ElementID:  el.ID,
		DueAt:      rt.e.now(),
		Retries:    3,
		CreatedAt:  rt.e.now(),
	}
	if err := rt.e.st.PutJob(job); err != nil {
		return err
	}
	tok.State = store.TokenWaitRetry
	tok.WaitRef = job.ID
	return nil
}

func (rt *runtime) spawnToken(elementID string, scopePath, scopeOwners []string, parent string, locals map[string]any) *store.Token {
	t := &store.Token{
		ID:          rt.e.newID("tok"),
		ElementID:   elementID,
		State:       store.TokenActive,
		ScopePath:   append([]string(nil), scopePath...),
		ScopeOwners: append([]string(nil), scopeOwners...),
		Parent:      parent,
		LocalVars:   locals,
	}
	rt.inst.Tokens[t.ID] = t
	return t
}

func cloneLocals(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// consume removes a token and completes enclosing scopes / the instance
// when it was the last one.
func (rt *runtime) consume(tok *store.Token) error {
	delete(rt.inst.Tokens, tok.ID)
	return rt.checkScope(tok.ScopePath, tok.ScopeOwners)
}

// checkScope completes a sub-process scope (or the instance) when no
// tokens remain in it. Owners disambiguate concurrent multi-instance
// iterations that share the same scope path.
func (rt *runtime) checkScope(path, owners []string) error {
	for _, t := range rt.inst.Tokens {
		if hasPrefix(t.ScopePath, path) && hasPrefix(t.ScopeOwners, owners) {
			return nil // scope still busy
		}
	}
	if len(path) == 0 {
		// Only complete when no waiting tokens remain anywhere.
		if len(rt.inst.Tokens) == 0 {
			rt.completeInstance("")
		}
		return nil
	}
	parentPath := path[:len(path)-1]
	parentOwners := owners[:len(owners)-1]
	scopeID := path[len(path)-1]
	ownerID := owners[len(owners)-1]
	parentC := rt.containerAt(parentPath)
	if parentC == nil {
		return nil
	}
	scopeEl := parentC.Elements[scopeID]
	if scopeEl != nil && scopeEl.TriggeredByEvent {
		// Event sub-process finished: nothing owns it; the parent scope
		// may now be empty as well.
		return rt.checkScope(parentPath, parentOwners)
	}
	// Resume the owner token waiting at the sub-process element.
	if ownerID != "" {
		if t := rt.inst.Tokens[ownerID]; t != nil && t.ElementID == scopeID && t.State == store.TokenWaitChild {
			rt.emit(store.HistElementCompleted, scopeID, nil)
			if scopeEl != nil {
				rt.applyOutputs(scopeEl, t)
			}
			rt.cancelBoundaries(t)
			rt.completeActivity(t)
			return nil
		}
	}
	return rt.checkScope(parentPath, parentOwners)
}

func hasPrefix(path, prefix []string) bool {
	if len(path) < len(prefix) {
		return false
	}
	for i := range prefix {
		if path[i] != prefix[i] {
			return false
		}
	}
	return true
}

func samePath(a, b []string) bool {
	return len(a) == len(b) && hasPrefix(a, b)
}

// completeActivity routes an activity completion either to the
// multi-instance collector (for MI children) or onward through outgoing
// flows.
func (rt *runtime) completeActivity(tok *store.Token) {
	if tok.Parent != "" {
		el := rt.element(tok)
		if mi := rt.inst.Multi[tok.Parent]; mi != nil && el != nil && isMIChild(tok, el) {
			rt.miChildCompleted(tok)
			return
		}
	}
	rt.cancelBoundaries(tok)
	el := rt.element(tok)
	if el == nil {
		_ = rt.consume(tok)
		return
	}
	if err := rt.takeOutgoing(tok, el); err != nil {
		rt.e.log.Error("takeOutgoing failed", "element", el.ID, "error", err)
	}
}

// ---- instance end ---------------------------------------------------------------

func (rt *runtime) completeInstance(endElement string) {
	rt.inst.State = store.InstanceCompleted
	rt.inst.EndedAt = rt.e.now()
	if endElement != "" {
		rt.inst.EndElement = endElement
	}
	rt.cleanupInstanceRecords()
	rt.e.metrics.InstancesCompleted.Add(1)
	rt.emit(store.HistInstanceCompleted, endElement, nil)
	rt.notifyParent()
}

func (rt *runtime) terminate(reason string) {
	for id := range rt.inst.Tokens {
		delete(rt.inst.Tokens, id)
	}
	rt.inst.State = store.InstanceTerminated
	rt.inst.EndedAt = rt.e.now()
	rt.cleanupInstanceRecords()
	rt.e.metrics.InstancesTerminated.Add(1)
	rt.emit(store.HistInstanceTerminated, "", map[string]any{"reason": reason})
	rt.notifyParent()
}

// cleanupInstanceRecords cancels every outstanding wait record of the
// instance.
func (rt *runtime) cleanupInstanceRecords() {
	if tasks, err := rt.e.st.ListTasks(store.TaskFilter{InstanceID: rt.inst.ID, State: store.TaskCreated}); err == nil {
		for _, t := range tasks {
			t.State = store.TaskCanceled
			_ = rt.e.st.PutTask(t)
			rt.emit(store.HistTaskCanceled, t.ElementID, map[string]any{"taskId": t.ID})
		}
	}
	if jobs, err := rt.e.st.ListJobs(rt.inst.ID); err == nil {
		for _, j := range jobs {
			_ = rt.e.st.DeleteJob(j.ID)
		}
	}
	if subs, err := rt.e.st.ListSubscriptions(store.SubscriptionFilter{InstanceID: rt.inst.ID}); err == nil {
		for _, s := range subs {
			_ = rt.e.st.DeleteSubscription(s.ID)
		}
	}
	if exts, err := rt.e.st.ListExternalTasks(rt.inst.ID); err == nil {
		for _, x := range exts {
			if x.State == store.ExternalPending {
				x.State = store.ExternalFailed
				_ = rt.e.st.PutExternalTask(x)
			}
		}
	}
	// Cancel running call-activity children.
	if children, err := rt.e.st.ListInstances(store.InstanceFilter{ParentID: rt.inst.ID, State: store.InstanceActive}); err == nil {
		e := rt.e
		for _, ch := range children {
			childID := ch.ID
			rt.continuations = append(rt.continuations, func() {
				if err := e.CancelInstance(childID, "parent ended"); err != nil {
					e.log.Warn("cancel child failed", "child", childID, "error", err)
				}
			})
		}
	}
}

// notifyParent resumes the call-activity token in the parent instance
// after this child ends.
func (rt *runtime) notifyParent() {
	if rt.inst.ParentID == "" {
		return
	}
	e := rt.e
	parentID := rt.inst.ParentID
	parentTok := rt.inst.ParentTokenID
	childID := rt.inst.ID
	childVars := cloneLocals(rt.inst.Variables)
	rt.continuations = append(rt.continuations, func() {
		err := e.resume(parentID, func(prt *runtime) (bool, error) {
			tok := prt.inst.Tokens[parentTok]
			if tok == nil || tok.State != store.TokenWaitChild || tok.WaitRef != childID {
				return false, nil
			}
			el := prt.element(tok)
			if el != nil {
				// Map child outputs into the parent scope.
				for _, m := range el.Outputs {
					v, err := expr.Eval(m.Source, childVars)
					if err != nil {
						prt.raiseIncident(tok, "", fmt.Sprintf("output mapping %s: %v", m.Target, err))
						return true, nil
					}
					prt.inst.Variables[m.Target] = v
				}
				prt.emit(store.HistElementCompleted, el.ID, map[string]any{"childInstanceId": childID})
			}
			prt.completeActivity(tok)
			return true, nil
		})
		if err != nil {
			e.log.Warn("parent notification failed", "parent", parentID, "error", err)
		}
	})
}

// ---- end events --------------------------------------------------------------------

func (rt *runtime) endEvent(tok *store.Token, el *bpmn.Element) error {
	rt.emit(store.HistElementActivated, el.ID, nil)
	kind := bpmn.KindNone
	if el.Event != nil {
		kind = el.Event.Kind
	}
	switch kind {
	case bpmn.KindTerminate:
		if len(tok.ScopePath) == 0 {
			for id := range rt.inst.Tokens {
				delete(rt.inst.Tokens, id)
			}
			rt.completeInstance(el.ID)
			return nil
		}
		// Terminate only the enclosing sub-process scope.
		path, owners := tok.ScopePath, tok.ScopeOwners
		for id, t := range rt.inst.Tokens {
			if hasPrefix(t.ScopePath, path) && hasPrefix(t.ScopeOwners, owners) {
				rt.cancelWaits(t)
				delete(rt.inst.Tokens, id)
			}
		}
		return rt.checkScope(path, owners)

	case bpmn.KindError:
		code := el.Event.ErrorCode
		delete(rt.inst.Tokens, tok.ID)
		rt.emit(store.HistErrorThrown, el.ID, map[string]any{"code": code})
		rt.propagateError(tok, code, "error end event "+el.ID)
		return nil

	case bpmn.KindMessage:
		// Message end event: emit + internal correlation as continuation.
		rt.throwMessage(el)
	case bpmn.KindSignal:
		rt.throwSignal(el)
	}
	// Record which end event finished this path; if the instance completes
	// now, this is its end element.
	if len(tok.ScopePath) == 0 {
		rt.inst.EndElement = el.ID
	}
	return rt.consume(tok)
}

// intermediateThrow handles throw events (none, message, signal).
func (rt *runtime) intermediateThrow(tok *store.Token, el *bpmn.Element) error {
	rt.emit(store.HistElementActivated, el.ID, nil)
	if el.Event != nil {
		switch el.Event.Kind {
		case bpmn.KindMessage:
			rt.throwMessage(el)
		case bpmn.KindSignal:
			rt.throwSignal(el)
		}
	}
	return rt.leave(tok, el)
}

// throwMessage correlates a thrown message to other instances after the
// lock is released.
func (rt *runtime) throwMessage(el *bpmn.Element) {
	if el.Event == nil || el.Event.Message == "" {
		return
	}
	e := rt.e
	name := el.Event.Message
	vars := cloneLocals(rt.inst.Variables)
	rt.continuations = append(rt.continuations, func() {
		if _, err := e.CorrelateMessage(name, "", vars); err != nil {
			e.log.Warn("message throw correlation failed", "message", name, "error", err)
		}
	})
}

func (rt *runtime) throwSignal(el *bpmn.Element) {
	if el.Event == nil || el.Event.Signal == "" {
		return
	}
	e := rt.e
	name := el.Event.Signal
	vars := cloneLocals(rt.inst.Variables)
	rt.continuations = append(rt.continuations, func() {
		if _, err := e.BroadcastSignal(name, vars); err != nil {
			e.log.Warn("signal broadcast failed", "signal", name, "error", err)
		}
	})
}

// ---- gateways ------------------------------------------------------------------------

func (rt *runtime) exclusiveGateway(tok *store.Token, el *bpmn.Element) error {
	c := rt.scope(tok)
	env := rt.env(tok)
	for _, fid := range el.Outgoing {
		if fid == el.DefaultFlow {
			continue
		}
		f := c.Flows[fid]
		if f == nil {
			continue
		}
		take := f.Condition == ""
		if !take {
			ok, err := expr.EvalBool(f.Condition, env)
			if err != nil {
				rt.raiseIncident(tok, "", fmt.Sprintf("condition on flow %s: %v", f.ID, err))
				return nil
			}
			take = ok
		}
		if take {
			rt.emit(store.HistElementCompleted, el.ID, map[string]any{"flow": f.ID})
			return rt.moveOver(tok, []*bpmn.SequenceFlow{f})
		}
	}
	if el.DefaultFlow != "" {
		if f := c.Flows[el.DefaultFlow]; f != nil {
			rt.emit(store.HistElementCompleted, el.ID, map[string]any{"flow": f.ID})
			return rt.moveOver(tok, []*bpmn.SequenceFlow{f})
		}
	}
	rt.raiseIncident(tok, "", fmt.Sprintf("exclusive gateway %s: no condition matched and no default flow", el.ID))
	return nil
}

func (rt *runtime) parallelGateway(tok *store.Token, el *bpmn.Element) error {
	if len(el.Incoming) > 1 {
		tok.State = store.TokenJoining
		if !rt.parallelJoinReady(tok, el) {
			return nil
		}
		rt.mergeJoiningTokens(tok, el)
	}
	rt.emit(store.HistElementCompleted, el.ID, nil)
	return rt.forkAll(tok, el)
}

// parallelJoinReady reports whether every incoming flow has a joining
// token.
func (rt *runtime) parallelJoinReady(tok *store.Token, el *bpmn.Element) bool {
	arrived := map[string]bool{}
	for _, t := range rt.inst.Tokens {
		if t.ElementID == el.ID && t.State == store.TokenJoining && sameScope(t, tok) {
			arrived[t.ArrivedFlow] = true
		}
	}
	for _, fid := range el.Incoming {
		if !arrived[fid] {
			return false
		}
	}
	return true
}

// mergeJoiningTokens consumes one token per incoming flow, keeping tok.
func (rt *runtime) mergeJoiningTokens(tok *store.Token, el *bpmn.Element) {
	consumed := map[string]bool{tok.ArrivedFlow: true}
	for id, t := range rt.inst.Tokens {
		if t.ID == tok.ID || t.ElementID != el.ID || t.State != store.TokenJoining || !sameScope(t, tok) {
			continue
		}
		if consumed[t.ArrivedFlow] {
			continue
		}
		consumed[t.ArrivedFlow] = true
		delete(rt.inst.Tokens, id)
	}
	tok.State = store.TokenActive
}

// forkAll takes every outgoing flow unconditionally (parallel semantics).
func (rt *runtime) forkAll(tok *store.Token, el *bpmn.Element) error {
	c := rt.scope(tok)
	var flows []*bpmn.SequenceFlow
	for _, fid := range el.Outgoing {
		if f := c.Flows[fid]; f != nil {
			flows = append(flows, f)
		}
	}
	if len(flows) == 0 {
		rt.raiseIncident(tok, "", fmt.Sprintf("gateway %s has no outgoing flows", el.ID))
		return nil
	}
	return rt.moveOver(tok, flows)
}

func (rt *runtime) inclusiveGateway(tok *store.Token, el *bpmn.Element) error {
	if len(el.Incoming) > 1 {
		tok.State = store.TokenJoining
		if !rt.inclusiveJoinReady(tok, el) {
			return nil
		}
		// Merge every token currently joining here.
		for id, t := range rt.inst.Tokens {
			if t.ID != tok.ID && t.ElementID == el.ID && t.State == store.TokenJoining && sameScope(t, tok) {
				delete(rt.inst.Tokens, id)
			}
		}
		tok.State = store.TokenActive
	}
	rt.emit(store.HistElementCompleted, el.ID, nil)

	// Fork: take all flows with true conditions (or no condition).
	c := rt.scope(tok)
	env := rt.env(tok)
	var flows []*bpmn.SequenceFlow
	for _, fid := range el.Outgoing {
		if fid == el.DefaultFlow {
			continue
		}
		f := c.Flows[fid]
		if f == nil {
			continue
		}
		if f.Condition != "" {
			ok, err := expr.EvalBool(f.Condition, env)
			if err != nil {
				rt.raiseIncident(tok, "", fmt.Sprintf("condition on flow %s: %v", f.ID, err))
				return nil
			}
			if !ok {
				continue
			}
		}
		flows = append(flows, f)
	}
	if len(flows) == 0 && el.DefaultFlow != "" {
		if f := c.Flows[el.DefaultFlow]; f != nil {
			flows = append(flows, f)
		}
	}
	if len(flows) == 0 {
		rt.raiseIncident(tok, "", fmt.Sprintf("inclusive gateway %s: no flow taken", el.ID))
		return nil
	}
	return rt.moveOver(tok, flows)
}

// inclusiveJoinReady implements inclusive join semantics: fire when no
// other token in the instance could still reach this gateway.
func (rt *runtime) inclusiveJoinReady(tok *store.Token, el *bpmn.Element) bool {
	c := rt.scope(tok)
	for _, t := range rt.inst.Tokens {
		if t.ID == tok.ID {
			continue
		}
		if t.ElementID == el.ID && t.State == store.TokenJoining && sameScope(t, tok) {
			continue // already here
		}
		pos, ok := rt.positionInScope(t, tok)
		if !ok {
			continue // token in an unrelated scope
		}
		if rt.canReach(c, pos, el.ID) {
			return false
		}
	}
	return true
}

// positionInScope projects a token onto an element of the reference
// token's scope: tokens inside nested sub-processes count as sitting on
// the sub-process element. Tokens from other multi-instance iterations
// (different scope owners) are unrelated.
func (rt *runtime) positionInScope(t, ref *store.Token) (string, bool) {
	if !hasPrefix(t.ScopeOwners, ref.ScopeOwners) {
		return "", false
	}
	if samePath(t.ScopePath, ref.ScopePath) {
		return t.ElementID, true
	}
	if hasPrefix(t.ScopePath, ref.ScopePath) && len(t.ScopePath) > len(ref.ScopePath) {
		return t.ScopePath[len(ref.ScopePath)], true
	}
	return "", false
}

// sameScope reports whether two tokens run in the same scope instance
// (same path and same owner chain).
func sameScope(a, b *store.Token) bool {
	return samePath(a.ScopePath, b.ScopePath) && samePath(a.ScopeOwners, b.ScopeOwners)
}

// canReach walks sequence flows from fromID looking for toID.
func (rt *runtime) canReach(c *bpmn.Container, fromID, toID string) bool {
	if fromID == toID {
		return true
	}
	seen := map[string]bool{fromID: true}
	queue := []string{fromID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		el := c.Elements[cur]
		if el == nil {
			continue
		}
		next := el.Outgoing
		// Boundary events extend reachability from their host activity.
		for _, b := range c.BoundaryEvents(cur) {
			next = append(next, b.Outgoing...)
		}
		for _, fid := range next {
			f := c.Flows[fid]
			if f == nil || seen[f.TargetRef] {
				continue
			}
			if f.TargetRef == toID {
				return true
			}
			seen[f.TargetRef] = true
			queue = append(queue, f.TargetRef)
		}
	}
	return false
}

// eventBasedGateway parks the token racing several catch events; the
// first trigger wins and cancels the rest.
func (rt *runtime) eventBasedGateway(tok *store.Token, el *bpmn.Element) error {
	c := rt.scope(tok)
	tok.State = store.TokenWaitEvents
	tok.WaitRef = ""
	for _, fid := range el.Outgoing {
		f := c.Flows[fid]
		if f == nil {
			continue
		}
		target := c.Elements[f.TargetRef]
		if target == nil || target.Event == nil {
			continue
		}
		if err := rt.createEventWait(tok, target, target.Event, false); err != nil {
			return err
		}
	}
	return nil
}

// ---- catch events & waits ------------------------------------------------------------

// intermediateCatch parks the token on its event definition.
func (rt *runtime) intermediateCatch(tok *store.Token, el *bpmn.Element) error {
	switch el.Event.Kind {
	case bpmn.KindTimer:
		tok.State = store.TokenWaitTimer
	case bpmn.KindMessage:
		tok.State = store.TokenWaitMessage
	case bpmn.KindSignal:
		tok.State = store.TokenWaitSignal
	default:
		rt.raiseIncident(tok, "", fmt.Sprintf("unsupported catch event kind %s", el.Event.Kind))
		return nil
	}
	return rt.createEventWait(tok, el, el.Event, true)
}

// createEventWait creates the job/subscription backing a catch event for
// the given token. setWaitRef ties the token's WaitRef to the created
// record (single-wait states); boundary events and event-based gateway
// targets pass false.
func (rt *runtime) createEventWait(tok *store.Token, el *bpmn.Element, ev *bpmn.EventDefinition, setWaitRef bool) error {
	switch ev.Kind {
	case bpmn.KindTimer:
		due, repeats, interval, err := rt.e.timerSchedule(ev, rt.env(tok))
		if err != nil {
			rt.raiseIncident(tok, "", fmt.Sprintf("timer on %s: %v", el.ID, err))
			return nil
		}
		job := &store.Job{
			ID:         rt.e.newID("job"),
			Kind:       store.JobTimer,
			InstanceID: rt.inst.ID,
			TokenID:    tok.ID,
			ElementID:  el.ID,
			DueAt:      due,
			Repeats:    repeats,
			Interval:   interval,
			Retries:    3,
			CreatedAt:  rt.e.now(),
		}
		if err := rt.e.st.PutJob(job); err != nil {
			return err
		}
		if setWaitRef {
			tok.WaitRef = job.ID
		}
		rt.emit(store.HistTimerScheduled, el.ID, map[string]any{"dueAt": due.Format(time.RFC3339)})
	case bpmn.KindMessage:
		sub := &store.Subscription{
			ID:             rt.e.newID("sub"),
			Kind:           store.SubMessage,
			Name:           ev.Message,
			CorrelationKey: rt.inst.BusinessKey,
			InstanceID:     rt.inst.ID,
			TokenID:        tok.ID,
			ElementID:      el.ID,
			CreatedAt:      rt.e.now(),
		}
		if err := rt.e.st.PutSubscription(sub); err != nil {
			return err
		}
		if setWaitRef {
			tok.WaitRef = sub.ID
		}
	case bpmn.KindSignal:
		sub := &store.Subscription{
			ID:         rt.e.newID("sub"),
			Kind:       store.SubSignal,
			Name:       ev.Signal,
			InstanceID: rt.inst.ID,
			TokenID:    tok.ID,
			ElementID:  el.ID,
			CreatedAt:  rt.e.now(),
		}
		if err := rt.e.st.PutSubscription(sub); err != nil {
			return err
		}
		if setWaitRef {
			tok.WaitRef = sub.ID
		}
	}
	return nil
}

// ---- incidents & errors ------------------------------------------------------------------

// raiseIncident parks the token and records an incident for humans to
// resolve. The instance stays alive — failures are never silent.
func (rt *runtime) raiseIncident(tok *store.Token, code, message string) {
	inc := &store.Incident{
		ID:         rt.e.newID("incd"),
		InstanceID: rt.inst.ID,
		TokenID:    tok.ID,
		ElementID:  tok.ElementID,
		Message:    message,
		Code:       code,
		CreatedAt:  rt.e.now(),
	}
	if err := rt.e.st.PutIncident(inc); err != nil {
		rt.e.log.Error("incident write failed", "error", err)
	}
	if t, ok := rt.inst.Tokens[tok.ID]; ok {
		t.State = store.TokenWaitRetry
		t.WaitRef = inc.ID
	}
	rt.e.metrics.IncidentsCreated.Add(1)
	rt.emit(store.HistIncidentCreated, tok.ElementID, map[string]any{"incidentId": inc.ID, "message": message})
	rt.e.log.Warn("incident", "instance", rt.inst.ID, "element", tok.ElementID, "message", message)
}

// throwError implements BPMN error semantics from a failing activity.
func (rt *runtime) throwError(tok *store.Token, code, message string) {
	rt.emit(store.HistErrorThrown, tok.ElementID, map[string]any{"code": code, "message": message})
	// A boundary error event directly on the activity wins.
	c := rt.scope(tok)
	for _, b := range c.BoundaryEvents(tok.ElementID) {
		if b.Event.Kind == bpmn.KindError && (b.Event.ErrorCode == "" || b.Event.ErrorCode == code) {
			rt.inst.Variables["errorCode"] = code
			rt.inst.Variables["errorMessage"] = message
			// An error escaping one multi-instance iteration cancels the
			// whole loop; the boundary path continues once, with the
			// coordinator token.
			if coord := rt.miCoordinatorFor(tok); coord != nil {
				rt.cancelWaits(coord) // sweeps all iterations incl. tok
				rt.moveToBoundary(coord, b)
				return
			}
			rt.cancelWaits(tok)
			rt.moveToBoundary(tok, b)
			return
		}
	}
	rt.cancelWaits(tok)
	delete(rt.inst.Tokens, tok.ID)
	rt.propagateError(tok, code, message)
}

// miCoordinatorFor returns the multi-instance coordinator token when tok
// is a loop iteration, or nil.
func (rt *runtime) miCoordinatorFor(tok *store.Token) *store.Token {
	if tok == nil || tok.Parent == "" || rt.inst.Multi[tok.Parent] == nil {
		return nil
	}
	return rt.inst.Tokens[tok.Parent]
}

// propagateError walks scopes outward looking for an error handler
// (event sub-process with a matching error start, or an error boundary on
// the enclosing sub-process). Unhandled errors terminate the instance
// with an incident — and propagate to a call-activity parent.
func (rt *runtime) propagateError(tok *store.Token, code, message string) {
	path := append([]string(nil), tok.ScopePath...)
	owners := append([]string(nil), tok.ScopeOwners...)
	for depth := len(path); depth >= 0; depth-- {
		scopePath := path[:depth]
		scopeOwners := owners[:depth]
		c := rt.containerAt(scopePath)
		if c == nil {
			continue
		}
		// 1) Event sub-process with matching error start event.
		for _, id := range c.Order {
			es := c.Elements[id]
			if es == nil || es.Type != bpmn.TypeSubProcess || !es.TriggeredByEvent || es.Sub == nil {
				continue
			}
			for _, se := range es.Sub.StartEvents() {
				if se.Event != nil && se.Event.Kind == bpmn.KindError && (se.Event.ErrorCode == "" || se.Event.ErrorCode == code) {
					rt.killScopeTokens(scopePath, scopeOwners, id)
					rt.inst.Variables["errorCode"] = code
					rt.inst.Variables["errorMessage"] = message
					rt.spawnToken(se.ID, append(scopePath, id), append(scopeOwners, ""), "", nil)
					return
				}
			}
		}
		// 2) Error boundary on the enclosing sub-process element.
		if depth > 0 {
			parentPath := path[:depth-1]
			pc := rt.containerAt(parentPath)
			scopeID := path[depth-1]
			ownerID := owners[depth-1]
			if pc != nil {
				for _, b := range pc.BoundaryEvents(scopeID) {
					if b.Event.Kind == bpmn.KindError && (b.Event.ErrorCode == "" || b.Event.ErrorCode == code) {
						// Cancel the whole scope, then continue from the
						// boundary event.
						rt.killScopeTokens(scopePath, scopeOwners, "")
						rt.inst.Variables["errorCode"] = code
						rt.inst.Variables["errorMessage"] = message
						owner := rt.inst.Tokens[ownerID]
						// A failing iteration of a multi-instance
						// sub-process cancels the entire loop; the
						// coordinator continues down the boundary path.
						if coord := rt.miCoordinatorFor(owner); coord != nil {
							rt.cancelWaits(coord)
							rt.moveToBoundary(coord, b)
							return
						}
						if owner != nil && owner.ElementID == scopeID {
							rt.cancelBoundaries(owner)
							owner.ElementID = b.ID
							owner.State = store.TokenActive
							owner.WaitRef = ""
						} else {
							rt.spawnToken(b.ID, parentPath, owners[:depth-1], "", nil)
						}
						return
					}
				}
			}
		}
	}
	// Unhandled: record and end the instance (parents are notified).
	inc := &store.Incident{
		ID:         rt.e.newID("incd"),
		InstanceID: rt.inst.ID,
		TokenID:    tok.ID,
		ElementID:  tok.ElementID,
		Message:    fmt.Sprintf("unhandled BPMN error %q: %s", code, message),
		Code:       code,
		CreatedAt:  rt.e.now(),
	}
	_ = rt.e.st.PutIncident(inc)
	rt.e.metrics.IncidentsCreated.Add(1)
	rt.emit(store.HistIncidentCreated, tok.ElementID, map[string]any{"incidentId": inc.ID, "message": inc.Message})
	rt.terminate("unhandled error " + code)
}

// killScopeTokens removes every token inside the scope instance
// identified by scopePath+scopeOwners (except tokens in the excluded
// child scope, e.g. the event sub-process being started).
func (rt *runtime) killScopeTokens(scopePath, scopeOwners []string, excludeChild string) {
	for id, t := range rt.inst.Tokens {
		if !hasPrefix(t.ScopePath, scopePath) || !hasPrefix(t.ScopeOwners, scopeOwners) {
			continue
		}
		if excludeChild != "" && len(t.ScopePath) > len(scopePath) && t.ScopePath[len(scopePath)] == excludeChild {
			continue
		}
		// Do not kill the owner token of the scope itself (it sits in the
		// parent scope), only tokens inside.
		rt.cancelWaits(t)
		delete(rt.inst.Tokens, id)
	}
}

// cancelWaits cancels whatever the token is waiting for.
func (rt *runtime) cancelWaits(t *store.Token) {
	switch t.State {
	case store.TokenWaitTask:
		if task, err := rt.e.st.GetTask(t.WaitRef); err == nil && task.State == store.TaskCreated {
			task.State = store.TaskCanceled
			_ = rt.e.st.PutTask(task)
			rt.emit(store.HistTaskCanceled, task.ElementID, map[string]any{"taskId": task.ID})
		}
	case store.TokenWaitTimer, store.TokenWaitRetry:
		if t.WaitRef != "" {
			_ = rt.e.st.DeleteJob(t.WaitRef)
		}
	case store.TokenWaitMessage, store.TokenWaitSignal:
		if t.WaitRef != "" {
			_ = rt.e.st.DeleteSubscription(t.WaitRef)
		}
	case store.TokenWaitExternal:
		if ext, err := rt.e.st.GetExternalTask(t.WaitRef); err == nil && ext.State == store.ExternalPending {
			ext.State = store.ExternalFailed
			_ = rt.e.st.PutExternalTask(ext)
		}
	case store.TokenWaitChild:
		if t.WaitRef != "" && t.WaitRef != scopeWaitRef {
			e := rt.e
			childID := t.WaitRef
			rt.continuations = append(rt.continuations, func() {
				if err := e.CancelInstance(childID, "parent scope cancelled"); err != nil && !errors.Is(err, store.ErrNotFound) {
					e.log.Debug("child cancel skipped", "child", childID, "error", err)
				}
			})
		}
	case store.TokenWaitEvents:
		rt.dropTokenEventWaits(t.ID)
	}
	// Boundary registrations always go.
	rt.cancelBoundaries(t)
	// Multi-instance children die with their parent.
	if rt.inst.Multi[t.ID] != nil {
		for id, ch := range rt.inst.Tokens {
			if ch.Parent == t.ID {
				rt.cancelActivity(ch)
				delete(rt.inst.Tokens, id)
			}
		}
		delete(rt.inst.Multi, t.ID)
	}
}

// dropTokenEventWaits deletes all jobs and subscriptions bound to a token
// (event-based gateway cleanup).
func (rt *runtime) dropTokenEventWaits(tokenID string) {
	if jobs, err := rt.e.st.ListJobs(rt.inst.ID); err == nil {
		for _, j := range jobs {
			if j.TokenID == tokenID {
				_ = rt.e.st.DeleteJob(j.ID)
			}
		}
	}
	if subs, err := rt.e.st.ListSubscriptions(store.SubscriptionFilter{InstanceID: rt.inst.ID}); err == nil {
		for _, s := range subs {
			if s.TokenID == tokenID {
				_ = rt.e.st.DeleteSubscription(s.ID)
			}
		}
	}
}

// ---- misc ----------------------------------------------------------------------------------

func atoiSafe(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
