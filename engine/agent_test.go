package engine

import (
	"strings"
	"testing"

	"github.com/olbboy/fliable/store"
)

const triageXML = `
  <process id="triage" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="classify"/>
    <serviceTask id="classify" fliable:agent="classifier" fliable:agentModel="claude-opus-4-8"
                 fliable:resultVariable="category">
      <extensionElements>
        <fliable:agent>
          <fliable:system>You classify support tickets.</fliable:system>
          <fliable:prompt>Classify this ticket: ${ticket}</fliable:prompt>
          <fliable:tool name="lookupAccount" description="Look up an account by id" mcpServer="crm"/>
        </fliable:agent>
      </extensionElements>
    </serviceTask>
    <sequenceFlow id="f2" sourceRef="classify" targetRef="e"/>
    <endEvent id="e"/>
  </process>`

func agentXML(body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0"
             targetNamespace="https://fliable.dev/tests">` + body + `</definitions>`
}

// TestAgentTaskInProcessInvoker: an in-process agent invoker returns
// structured output that merges into variables, and every prompt/output is
// recorded in history for governance/replay.
func TestAgentTaskInProcessInvoker(t *testing.T) {
	h := newHarness(t)
	var gotPrompt, gotSystem, gotModel string
	var gotTools []string
	h.e.RegisterAgent("classifier", AgentInvokerFunc(func(req AgentRequest) (AgentResponse, error) {
		gotPrompt = req.Prompt
		gotSystem = req.System
		gotModel = req.Model
		for _, tl := range req.Tools {
			gotTools = append(gotTools, tl.Name+"@"+tl.MCPServer)
		}
		return AgentResponse{
			Output:    map[string]any{"category": "billing", "confidence": 0.9},
			ToolCalls: []AgentToolCall{{Tool: "lookupAccount", Input: map[string]any{"id": "A1"}, Result: "ok"}},
			Usage:     AgentUsage{InputTokens: 120, OutputTokens: 15, Model: "claude-opus-4-8"},
		}, nil
	}))
	h.deploy(agentXML(triageXML))

	inst := h.start("triage", map[string]any{"ticket": "I was double charged"})
	done := h.requireState(inst.ID, store.InstanceCompleted)

	// Prompt template interpolated; system + model + tools passed through.
	if gotPrompt != "Classify this ticket: I was double charged" {
		t.Errorf("prompt = %q", gotPrompt)
	}
	if gotSystem != "You classify support tickets." || gotModel != "claude-opus-4-8" {
		t.Errorf("system/model = %q / %q", gotSystem, gotModel)
	}
	if len(gotTools) != 1 || gotTools[0] != "lookupAccount@crm" {
		t.Errorf("tools = %v", gotTools)
	}
	// Structured output merged into variables.
	if done.Variables["category"] != "billing" || done.Variables["confidence"] != 0.9 {
		t.Errorf("vars = %v", done.Variables)
	}

	// Governance: history carries the invocation, the tool call, usage and output.
	evs, _ := h.e.History(inst.ID, store.HistoryFilter{})
	var invoked, completed *store.HistoryEvent
	for _, ev := range evs {
		switch ev.Type {
		case store.HistAgentInvoked:
			invoked = ev
		case store.HistAgentCompleted:
			completed = ev
		}
	}
	if invoked == nil || invoked.Detail["prompt"] != gotPrompt {
		t.Fatalf("agent.invoked event missing/prompt = %+v", invoked)
	}
	if completed == nil {
		t.Fatal("agent.completed event missing")
	}
	if _, ok := completed.Detail["toolCalls"].([]any); !ok {
		t.Errorf("tool calls not recorded for governance: %+v", completed.Detail)
	}
	if _, ok := completed.Detail["usage"].(map[string]any); !ok {
		t.Errorf("usage not recorded: %+v", completed.Detail)
	}
	if m := h.e.Metrics(); m.AgentInvocations != 1 || m.AgentInputTokens != 120 {
		t.Errorf("metrics = %+v", m)
	}
}

// TestAgentExternalWorker: an agent task with a topic parks on an agent job
// that an external AI worker fetches and completes over the API.
func TestAgentExternalWorker(t *testing.T) {
	h := newHarness(t)
	h.deploy(agentXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="draft"/>
    <serviceTask id="draft" fliable:agentTopic="drafting" fliable:resultVariable="reply">
      <extensionElements>
        <fliable:agent>
          <fliable:prompt>Draft a reply to: ${question}</fliable:prompt>
        </fliable:agent>
      </extensionElements>
    </serviceTask>
    <sequenceFlow id="f2" sourceRef="draft" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", map[string]any{"question": "how do I reset my password?"})
	h.requireState(inst.ID, store.InstanceActive)

	jobs, err := h.e.FetchAgentJobs("drafting", "ai-worker-1", 60_000_000_000, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("fetch = %v, %v", jobs, err)
	}
	job := jobs[0]
	if job.Prompt != "Draft a reply to: how do I reset my password?" {
		t.Errorf("job prompt = %q", job.Prompt)
	}
	// The worker (some AI assistant) ran a model and returns the result.
	if err := h.e.CompleteAgentJob(job.ID, "ai-worker-1", nil, "Click 'Forgot password'.",
		nil, AgentUsage{InputTokens: 50, OutputTokens: 8, Model: "claude-opus-4-8"}); err != nil {
		t.Fatal(err)
	}
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.Variables["reply"] != "Click 'Forgot password'." {
		t.Errorf("reply = %v", done.Variables["reply"])
	}
	// Wrong worker cannot complete.
	if err := h.e.CompleteAgentJob(job.ID, "intruder", nil, "x", nil, AgentUsage{}); err == nil {
		t.Error("wrong worker completed the agent job")
	}
}

// TestAgentHumanApprovalGate: with HumanApproval, the agent's proposal
// waits on a user task; approving applies it, rejecting discards it.
func TestAgentHumanApprovalGate(t *testing.T) {
	run := func(approve bool) *store.Instance {
		h := newHarness(t)
		h.e.RegisterAgent("writer", AgentInvokerFunc(func(req AgentRequest) (AgentResponse, error) {
			return AgentResponse{Output: map[string]any{"draft": "Dear customer, ..."}}, nil
		}))
		h.deploy(agentXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="write"/>
    <serviceTask id="write" fliable:agent="writer" fliable:agentApproval="true"
                 fliable:agentApprovalGroups="editors" fliable:resultVariable="draft">
      <extensionElements><fliable:agent><fliable:prompt>Write it</fliable:prompt></fliable:agent></extensionElements>
    </serviceTask>
    <sequenceFlow id="f2" sourceRef="write" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))
		inst := h.start("p", nil)
		h.requireState(inst.ID, store.InstanceActive)
		tasks := h.tasks(inst.ID)
		if len(tasks) != 1 || !strings.HasPrefix(tasks[0].Name, "Approve:") {
			t.Fatalf("approval task missing: %+v", tasks)
		}
		if len(tasks[0].CandidateGroups) != 1 || tasks[0].CandidateGroups[0] != "editors" {
			t.Errorf("approval groups = %v", tasks[0].CandidateGroups)
		}
		if err := h.e.CompleteTask(tasks[0].ID, map[string]any{"approved": approve}, "editor-1"); err != nil {
			t.Fatal(err)
		}
		return h.requireState(inst.ID, store.InstanceCompleted)
	}

	approved := run(true)
	if approved.Variables["draft"] != "Dear customer, ..." {
		t.Errorf("approved output not applied: %v", approved.Variables["draft"])
	}
	rejected := run(false)
	if _, set := rejected.Variables["draft"]; set {
		t.Errorf("rejected output was applied: %v", rejected.Variables["draft"])
	}
}

// TestAgentFailureRaisesIncident: an invoker error runs the retry/incident
// cycle like any other activity.
func TestAgentFailureBPMNError(t *testing.T) {
	h := newHarness(t)
	h.e.RegisterAgent("guard", AgentInvokerFunc(func(req AgentRequest) (AgentResponse, error) {
		return AgentResponse{}, NewBPMNError("UNSAFE", "content policy")
	}))
	h.deploy(agentXML(`
  <error id="err1" errorCode="UNSAFE" name="Unsafe"/>
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="a"/>
    <serviceTask id="a" fliable:agent="guard" fliable:resultVariable="out">
      <extensionElements><fliable:agent><fliable:prompt>go</fliable:prompt></fliable:agent></extensionElements>
    </serviceTask>
    <boundaryEvent id="unsafe" attachedToRef="a">
      <errorEventDefinition errorRef="err1"/>
    </boundaryEvent>
    <sequenceFlow id="f2" sourceRef="a" targetRef="ok"/>
    <sequenceFlow id="f3" sourceRef="unsafe" targetRef="blocked"/>
    <endEvent id="ok"/>
    <endEvent id="blocked"/>
  </process>`))

	inst := h.start("p", nil)
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.EndElement != "blocked" {
		t.Errorf("endElement = %q, want blocked (BPMN error routed)", done.EndElement)
	}
}
