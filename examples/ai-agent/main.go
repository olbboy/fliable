// Command ai-agent is the end-to-end demo of Fliable's AI agent
// orchestration: an agent task classifies a support ticket (calling an
// MCP-style tool), the proposal is gated through a human-in-the-loop
// approval task, a guard enforces a token budget and reviews every
// result, and the whole decision trail — prompt, tool calls, usage,
// approval — is read back from the event-sourced history.
//
//	go run ./examples/ai-agent
//
// The invoker here is a deterministic stand-in so the demo runs offline
// on a clean machine. In production you implement the same AgentInvoker
// interface with a real model call (e.g. Anthropic's Go SDK in YOUR
// module — the engine stays zero-dependency and provider-agnostic), or
// run an external AI worker against /v1/agent-jobs.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
)

const processXML = `<?xml version="1.0" encoding="UTF-8"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0"
             targetNamespace="https://fliable.dev/examples">
  <process id="ticketTriage" isExecutable="true">
    <startEvent id="start"/>
    <sequenceFlow id="f1" sourceRef="start" targetRef="triage"/>

    <!-- The agent task: prompt templated from instance variables, one
         MCP-native tool, human approval before the output is applied. -->
    <serviceTask id="triage" name="Triage ticket"
                 fliable:agent="triage-agent"
                 fliable:agentModel="claude-opus-4-8"
                 fliable:agentApproval="true"
                 fliable:agentApprovalGroups="support-leads">
      <extensionElements>
        <fliable:agent>
          <fliable:system>You triage customer support tickets. Use the tools to look up the customer, then output priority (high/normal) and a suggested reply.</fliable:system>
          <fliable:prompt>Ticket from ${customerId}: ${ticket}</fliable:prompt>
          <fliable:tool name="lookupCustomer" description="Fetch a customer profile by id" mcpServer="crm"/>
        </fliable:agent>
      </extensionElements>
    </serviceTask>

    <sequenceFlow id="f2" sourceRef="triage" targetRef="route"/>
    <exclusiveGateway id="route" default="fNormal"/>
    <sequenceFlow id="fHigh" sourceRef="route" targetRef="escalate">
      <conditionExpression>priority == 'high'</conditionExpression>
    </sequenceFlow>
    <sequenceFlow id="fNormal" sourceRef="route" targetRef="autoReply"/>

    <serviceTask id="escalate" name="Escalate to on-call" fliable:type="escalate"/>
    <sequenceFlow id="f3" sourceRef="escalate" targetRef="end"/>
    <serviceTask id="autoReply" name="Send suggested reply" fliable:type="sendReply"/>
    <sequenceFlow id="f4" sourceRef="autoReply" targetRef="end"/>
    <endEvent id="end"/>
  </process>
</definitions>`

// crmLookup is the local binding of the MCP-style "lookupCustomer" tool.
// A real deployment would call an MCP server; the descriptor the agent
// sees is identical either way.
func crmLookup(id string) string {
	profiles := map[string]string{
		"cus-42": `{"name":"Dana","plan":"enterprise","openTickets":3}`,
	}
	if p, ok := profiles[id]; ok {
		return p
	}
	return `{"plan":"free"}`
}

// triageInvoker simulates the LLM: it "calls" the CRM tool and derives a
// structured decision. Swap this for a real model call in production.
func triageInvoker(req engine.AgentRequest) (engine.AgentResponse, error) {
	customerID, _ := req.Variables["customerId"].(string)
	profile := crmLookup(customerID)

	priority := "normal"
	if strings.Contains(profile, `"enterprise"`) && strings.Contains(strings.ToLower(req.Prompt), "outage") {
		priority = "high"
	}
	return engine.AgentResponse{
		Output: map[string]any{
			"priority": priority,
			"reply":    "We're on it — an engineer has been paged.",
		},
		ToolCalls: []engine.AgentToolCall{{
			Tool:   "lookupCustomer",
			Input:  map[string]any{"id": customerID},
			Result: profile,
		}},
		Usage: engine.AgentUsage{InputTokens: 480, OutputTokens: 55, Model: req.Model},
	}, nil
}

func main() {
	eng := engine.New(store.NewMemory(),
		// Governance: budget circuit-breaker + drift review + hang backstop.
		engine.WithAgentGuard(engine.AgentGuard{
			InvokeTimeout:        30 * time.Second,
			MaxTokensPerInstance: 20_000,
			Review: func(r engine.AgentReview) error {
				if _, ok := r.Output["priority"]; !ok {
					return errors.New("agent output missing required field: priority")
				}
				return nil
			},
		}),
	)
	eng.RegisterAgent("triage-agent", engine.AgentInvokerFunc(triageInvoker))
	eng.RegisterHandler("escalate", func(ctx engine.Context) (map[string]any, error) {
		fmt.Printf("  [handler] paging on-call for instance %s\n", ctx.InstanceID)
		return map[string]any{"escalated": true}, nil
	})
	eng.RegisterHandler("sendReply", func(ctx engine.Context) (map[string]any, error) {
		fmt.Printf("  [handler] sending reply: %v\n", ctx.Variables["reply"])
		return nil, nil
	})
	eng.Start()
	defer eng.Stop()

	if _, err := eng.Deploy([]byte(processXML), ""); err != nil {
		panic(err)
	}

	fmt.Println("== 1. start instance: enterprise customer reports an outage")
	inst, err := eng.StartInstance("ticketTriage", "tkt-1001", map[string]any{
		"customerId": "cus-42",
		"ticket":     "Production outage since 09:00 — nothing loads.",
	})
	if err != nil {
		panic(err)
	}

	fmt.Println("== 2. the agent ran; its proposal waits for human approval")
	tasks, err := eng.ListTasks(store.TaskFilter{InstanceID: inst.ID, State: store.TaskCreated, CandidateGroup: "support-leads"})
	if err != nil || len(tasks) != 1 {
		panic(fmt.Sprintf("expected 1 approval task, got %d (%v)", len(tasks), err))
	}
	fmt.Printf("   approval task: %q for group %v\n", tasks[0].Name, tasks[0].CandidateGroups)

	fmt.Println("== 3. a support lead approves the proposal")
	if err := eng.CompleteTask(tasks[0].ID, map[string]any{"approved": true}, "lead-anna"); err != nil {
		panic(err)
	}

	final, err := eng.GetInstance(inst.ID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("== 4. instance %s: priority=%v escalated=%v\n", final.State, final.Variables["priority"], final.Variables["escalated"])
	if final.State != store.InstanceCompleted {
		panic("demo did not complete")
	}

	fmt.Println("== 5. governance: the full agent decision trail from history")
	evs, err := eng.History(inst.ID, store.HistoryFilter{Type: "agent."})
	if err != nil {
		panic(err)
	}
	for _, ev := range evs {
		detail, _ := json.MarshalIndent(ev.Detail, "     ", "  ")
		fmt.Printf("   %-16s %s\n     %s\n", ev.Type, ev.ElementID, detail)
	}

	m := eng.Metrics()
	fmt.Printf("== 6. metrics: agentInvocations=%d inputTokens=%d outputTokens=%d\n",
		m.AgentInvocations, m.AgentInputTokens, m.AgentOutputTokens)
	fmt.Println("done: agent task -> tool call -> human approval -> routed flow, fully audited")
}
