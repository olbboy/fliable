package engine

import (
	"fmt"
	"time"

	"github.com/olbboy/fliable/bpmn"
	"github.com/olbboy/fliable/store"
)

// AgentGuard bounds and reviews AI agent invocations — the runtime half of
// agent governance (the event log is the audit half). Configure with
// WithAgentGuard; every limit violation follows the engine's standard
// failure discipline (retry → incident, never silent).
type AgentGuard struct {
	// InvokeTimeout bounds one in-process invoker call; a call still
	// running when it expires is abandoned and treated as a failure
	// (retry → incident). Zero disables. External agent jobs are bounded
	// by their fetch lock instead: an expired lock makes the job
	// re-fetchable, so a stuck worker never wedges the flow.
	InvokeTimeout time.Duration
	// MaxTokensPerInstance caps cumulative agent token usage
	// (input + output) across every agent task of one instance — the
	// cost circuit-breaker for agent loops. When the budget is already
	// exhausted the next agent task raises an incident instead of
	// invoking. Zero disables.
	MaxTokensPerInstance int
	// Review inspects every agent result — in-process or from an external
	// worker — after it is recorded in history and before it is applied
	// or routed to human approval. Returning an error rejects the result
	// through the retry → incident cycle and emits agent.rejected. Drift
	// and hallucination detectors plug in here. Runs under the instance
	// lock: keep it fast and never call back into the engine.
	Review func(AgentReview) error
}

// AgentReview is the evidence a Review hook judges.
type AgentReview struct {
	InstanceID string
	ElementID  string
	Agent      string
	Output     map[string]any
	Text       string
	ToolCalls  []AgentToolCall
	Usage      AgentUsage
	// Variables is the instance environment the agent ran against.
	Variables map[string]any
}

// WithAgentGuard installs runtime governance for agent tasks: invocation
// timeout, per-instance token budget and a result review (drift) hook.
func WithAgentGuard(g AgentGuard) Option {
	return func(e *Engine) { e.agentGuard = &g }
}

// agentTokensVar accumulates an instance's total agent token usage; it is
// a reserved variable like __traceparent, persisted and purged with the
// instance.
const agentTokensVar = "__agentTokens"

// checkAgentBudget reports whether the instance may invoke another agent,
// raising an incident when the budget is spent.
func (rt *runtime) checkAgentBudget(tok *store.Token, el *bpmn.Element) bool {
	g := rt.e.agentGuard
	if g == nil || g.MaxTokensPerInstance <= 0 {
		return true
	}
	used, _ := rt.inst.Variables[agentTokensVar].(float64)
	if int(used) >= g.MaxTokensPerInstance {
		rt.emit(store.HistAgentRejected, el.ID, map[string]any{
			"reason": "token budget exhausted", "usedTokens": used,
			"budget": float64(g.MaxTokensPerInstance),
		})
		rt.raiseIncident(tok, "", fmt.Sprintf(
			"agent task %s: instance token budget exhausted (%d used, %d allowed)",
			el.ID, int(used), g.MaxTokensPerInstance))
		return false
	}
	return true
}

// chargeAgentUsage adds one invocation's tokens to the instance budget.
func (rt *runtime) chargeAgentUsage(usage AgentUsage) {
	total := usage.InputTokens + usage.OutputTokens
	if total == 0 {
		return
	}
	used, _ := rt.inst.Variables[agentTokensVar].(float64)
	rt.inst.Variables[agentTokensVar] = used + float64(total)
}

// reviewAgentResult runs the guard's Review hook; a rejection is recorded
// and fails the activity through the standard retry → incident cycle.
// Returns true when the result may be applied.
func (rt *runtime) reviewAgentResult(tok *store.Token, el *bpmn.Element, output map[string]any, text string, calls []AgentToolCall, usage AgentUsage) bool {
	g := rt.e.agentGuard
	if g == nil || g.Review == nil {
		return true
	}
	err := g.Review(AgentReview{
		InstanceID: rt.inst.ID, ElementID: el.ID, Agent: el.Agent.Agent,
		Output: output, Text: text, ToolCalls: calls, Usage: usage,
		Variables: rt.inst.Variables,
	})
	if err == nil {
		return true
	}
	rt.emit(store.HistAgentRejected, el.ID, map[string]any{"reason": err.Error()})
	rt.handleActivityFailure(tok, el, fmt.Errorf("agent result rejected by guard: %w", err))
	return false
}

// invokeWithTimeout calls an in-process invoker, bounded by the guard's
// InvokeTimeout. On expiry the call is abandoned (its goroutine finishes
// in the background and the result is discarded) and an error returns —
// the invoker should honor Model/provider timeouts itself; this is the
// engine's backstop.
func (e *Engine) invokeWithTimeout(inv AgentInvoker, req AgentRequest) (AgentResponse, error) {
	g := e.agentGuard
	if g == nil || g.InvokeTimeout <= 0 {
		return inv.InvokeAgent(req)
	}
	type result struct {
		resp AgentResponse
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := inv.InvokeAgent(req)
		ch <- result{resp, err}
	}()
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-time.After(g.InvokeTimeout):
		return AgentResponse{}, fmt.Errorf("agent invocation exceeded guard timeout %s", g.InvokeTimeout)
	}
}
