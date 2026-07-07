package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/olbboy/fliable/store"
)

// twoAgentsXML runs the same agent twice in sequence — the budget fixture.
const twoAgentsXML = `
  <process id="twice" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="a1"/>
    <serviceTask id="a1" fliable:agent="worker">
      <extensionElements><fliable:agent><fliable:prompt>step one</fliable:prompt></fliable:agent></extensionElements>
    </serviceTask>
    <sequenceFlow id="f2" sourceRef="a1" targetRef="a2"/>
    <serviceTask id="a2" fliable:agent="worker">
      <extensionElements><fliable:agent><fliable:prompt>step two</fliable:prompt></fliable:agent></extensionElements>
    </serviceTask>
    <sequenceFlow id="f3" sourceRef="a2" targetRef="e"/>
    <endEvent id="e"/>
  </process>`

func openIncidents(t *testing.T, h *harness, instID string) []*store.Incident {
	t.Helper()
	unresolved := false
	incs, err := h.e.Store().ListIncidents(store.IncidentFilter{InstanceID: instID, Resolved: &unresolved})
	if err != nil {
		t.Fatal(err)
	}
	return incs
}

// TestAgentGuardTokenBudget: the second invocation finds the instance
// budget spent and parks on an incident instead of calling the model.
func TestAgentGuardTokenBudget(t *testing.T) {
	h := newHarness(t)
	h.e.agentGuard = &AgentGuard{MaxTokensPerInstance: 1000}
	calls := 0
	h.e.RegisterAgent("worker", AgentInvokerFunc(func(req AgentRequest) (AgentResponse, error) {
		calls++
		return AgentResponse{Text: "done", Usage: AgentUsage{InputTokens: 900, OutputTokens: 200}}, nil
	}))
	h.deploy(agentXML(twoAgentsXML))

	inst := h.start("twice", nil)
	got := h.requireState(inst.ID, store.InstanceActive)
	if calls != 1 {
		t.Fatalf("invoker called %d times, want 1 (budget breaker)", calls)
	}
	incs := openIncidents(t, h, inst.ID)
	if len(incs) != 1 || !strings.Contains(incs[0].Message, "token budget exhausted") {
		t.Fatalf("incidents: %+v", incs)
	}
	// The budget block is in the governance log.
	evs, _ := h.e.History(inst.ID, store.HistoryFilter{Type: store.HistAgentRejected})
	if len(evs) != 1 || evs[0].Detail["reason"] != "token budget exhausted" {
		t.Fatalf("rejection events: %+v", evs)
	}
	// Usage was charged onto the instance.
	if used := got.Variables[agentTokensVar]; used != 1100.0 {
		t.Fatalf("charged tokens: %v", used)
	}
}

// TestAgentGuardBudgetAllowsWithinLimit: both steps run when usage stays
// under the budget.
func TestAgentGuardBudgetAllowsWithinLimit(t *testing.T) {
	h := newHarness(t)
	h.e.agentGuard = &AgentGuard{MaxTokensPerInstance: 10000}
	h.e.RegisterAgent("worker", AgentInvokerFunc(func(req AgentRequest) (AgentResponse, error) {
		return AgentResponse{Text: "ok", Usage: AgentUsage{InputTokens: 10, OutputTokens: 5}}, nil
	}))
	h.deploy(agentXML(twoAgentsXML))
	inst := h.start("twice", nil)
	h.requireState(inst.ID, store.InstanceCompleted)
}

// TestAgentGuardReviewRejects: a drift hook rejecting the result routes
// the element into the retry → incident cycle and emits agent.rejected.
func TestAgentGuardReviewRejects(t *testing.T) {
	h := newHarness(t)
	h.e.agentGuard = &AgentGuard{Review: func(r AgentReview) error {
		if r.Output["confidence"] == 0.1 {
			return errors.New("confidence below drift threshold")
		}
		return nil
	}}
	h.e.RegisterAgent("classifier", AgentInvokerFunc(func(req AgentRequest) (AgentResponse, error) {
		return AgentResponse{Output: map[string]any{"category": "spam", "confidence": 0.1}}, nil
	}))
	h.deploy(agentXML(triageXML))

	inst := h.start("triage", map[string]any{"ticket": "hi"})
	got := h.requireState(inst.ID, store.InstanceActive)
	// Rejected output must not have been applied.
	if _, applied := got.Variables["category"]; applied {
		t.Fatal("rejected output was applied")
	}
	evs, _ := h.e.History(inst.ID, store.HistoryFilter{Type: store.HistAgentRejected})
	if len(evs) != 1 || !strings.Contains(evs[0].Detail["reason"].(string), "drift threshold") {
		t.Fatalf("rejection events: %+v", evs)
	}
	// The element retries with backoff (default 3 attempts) and then
	// dead-letters into an incident — governed like any failing task.
	h.advance(6 * time.Second)
	h.advance(11 * time.Second)
	incs := openIncidents(t, h, inst.ID)
	if len(incs) != 1 || !strings.Contains(incs[0].Message, "rejected by guard") {
		t.Fatalf("incidents: %+v", incs)
	}
}

// TestAgentGuardReviewOnExternalJob: the same review hook governs results
// completed by external AI workers over the job API.
func TestAgentGuardReviewOnExternalJob(t *testing.T) {
	h := newHarness(t)
	rejected := true
	h.e.agentGuard = &AgentGuard{Review: func(r AgentReview) error {
		if rejected {
			return errors.New("output failed policy check")
		}
		return nil
	}}
	h.deploy(agentXML(`
  <process id="ext" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="a"/>
    <serviceTask id="a" fliable:agent="drafter" fliable:agentTopic="llm" fliable:retries="2">
      <extensionElements><fliable:agent><fliable:prompt>draft</fliable:prompt></fliable:agent></extensionElements>
    </serviceTask>
    <sequenceFlow id="f2" sourceRef="a" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("ext", nil)
	jobs, err := h.e.FetchAgentJobs("llm", "ai-1", time.Minute, 5)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("fetch: %v %d", err, len(jobs))
	}
	// The worker's completion is rejected by the guard: the element
	// retries, producing a fresh job after the backoff.
	if err := h.e.CompleteAgentJob(jobs[0].ID, "ai-1", map[string]any{"draft": "bad"}, "", nil, AgentUsage{}); err != nil {
		t.Fatal(err)
	}
	evs, _ := h.e.History(inst.ID, store.HistoryFilter{Type: store.HistAgentRejected})
	if len(evs) != 1 {
		t.Fatalf("rejection not recorded: %+v", evs)
	}
	h.advance(6 * time.Second)
	jobs, err = h.e.FetchAgentJobs("llm", "ai-1", time.Minute, 5)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("retry job: %v %d", err, len(jobs))
	}
	// A passing result completes the flow.
	rejected = false
	if err := h.e.CompleteAgentJob(jobs[0].ID, "ai-1", map[string]any{"draft": "good"}, "", nil, AgentUsage{InputTokens: 5}); err != nil {
		t.Fatal(err)
	}
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.Variables["draft"] != "good" {
		t.Fatalf("vars: %v", done.Variables)
	}
}

// TestAgentGuardInvokeTimeout: an invoker that never returns is abandoned
// at the guard timeout and fails through the retry cycle.
func TestAgentGuardInvokeTimeout(t *testing.T) {
	h := newHarness(t)
	h.e.agentGuard = &AgentGuard{InvokeTimeout: 20 * time.Millisecond}
	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // release the leaked goroutine
	h.e.RegisterAgent("classifier", AgentInvokerFunc(func(req AgentRequest) (AgentResponse, error) {
		<-block // simulates a hung provider call
		return AgentResponse{}, nil
	}))
	h.deploy(agentXML(triageXML))

	inst := h.start("triage", map[string]any{"ticket": "hi"})
	got := h.instance(inst.ID)
	if got.State != store.InstanceActive {
		t.Fatalf("state: %s", got.State)
	}
	// The token is parked on a retry — the engine survived the hang.
	found := false
	for _, tok := range got.Tokens {
		if tok.State == store.TokenWaitRetry {
			found = true
		}
	}
	if !found {
		t.Fatalf("no retry wait after timeout: %+v", got.Tokens)
	}
}
