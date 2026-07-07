package engine

import (
	"fmt"
	"strings"

	"github.com/olbboy/fliable/bpmn"
	"github.com/olbboy/fliable/expr"
	"github.com/olbboy/fliable/store"
)

// AgentTool is an MCP-native tool descriptor passed to an invoker.
type AgentTool struct {
	Name        string
	Description string
	Schema      string // raw JSON schema for the tool input
	MCPServer   string // optional MCP server that hosts the tool
}

// AgentRequest is the provider-agnostic invocation an AgentInvoker
// receives. The engine builds it from the process definition and instance
// variables; the invoker decides which model/provider to use.
type AgentRequest struct {
	InstanceID  string
	ElementID   string
	Agent       string
	Prompt      string
	System      string
	Tools       []AgentTool
	Model       string // hint, e.g. "claude-opus-4-8"
	Effort      string
	MaxTokens   int
	Variables   map[string]any
	Traceparent string // W3C trace context, for the invoker to propagate
}

// AgentResponse is what an invoker returns. Output structured data merges
// into instance variables; OutputVar (if the model returns a scalar) lands
// under the task's result variable. ToolCalls/Usage feed governance.
type AgentResponse struct {
	// Output merges into instance variables (structured output).
	Output map[string]any
	// Text is a scalar/textual result stored under the result variable.
	Text string
	// ToolCalls records what the agent invoked, for the audit trail.
	ToolCalls []AgentToolCall
	// Usage records token/cost accounting for governance dashboards.
	Usage AgentUsage
}

// AgentToolCall is one tool invocation the agent made, recorded verbatim
// in history for replay and audit.
type AgentToolCall struct {
	Tool   string         `json:"tool"`
	Input  map[string]any `json:"input,omitempty"`
	Result string         `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// AgentUsage is per-invocation accounting.
type AgentUsage struct {
	InputTokens  int     `json:"inputTokens,omitempty"`
	OutputTokens int     `json:"outputTokens,omitempty"`
	CostUSD      float64 `json:"costUsd,omitempty"`
	Model        string  `json:"model,omitempty"`
}

// AgentInvoker runs an AI agent in-process. Implementations call an
// LLM/agent (Anthropic, MCP tools, a local model — the engine does not
// care) and return structured output. Like ServiceHandler, an invoker
// runs under the instance lock, so it must not call back into the engine
// synchronously.
type AgentInvoker interface {
	InvokeAgent(req AgentRequest) (AgentResponse, error)
}

// AgentInvokerFunc adapts a function to AgentInvoker.
type AgentInvokerFunc func(req AgentRequest) (AgentResponse, error)

// InvokeAgent implements AgentInvoker.
func (f AgentInvokerFunc) InvokeAgent(req AgentRequest) (AgentResponse, error) { return f(req) }

// RegisterAgent binds an agent name to an in-process invoker. Agent tasks
// naming this agent (and without an external topic) call it directly.
func (e *Engine) RegisterAgent(name string, inv AgentInvoker) {
	e.amu.Lock()
	defer e.amu.Unlock()
	if e.agents == nil {
		e.agents = map[string]AgentInvoker{}
	}
	e.agents[name] = inv
}

func (e *Engine) agentInvoker(name string) AgentInvoker {
	e.amu.RLock()
	defer e.amu.RUnlock()
	if inv, ok := e.agents[name]; ok {
		return inv
	}
	return e.defaultAgent
}

// agentTask executes an AI agent activity: resolve the prompt, either call
// an in-process invoker or hand the work to external AI workers, then
// (optionally) gate the result through a human approval task. Every step
// is recorded in history for deterministic replay and governance.
func (rt *runtime) agentTask(tok *store.Token, el *bpmn.Element) error {
	spec := el.Agent
	// Cost circuit-breaker: an instance whose agent token budget is spent
	// parks on an incident instead of invoking again.
	if !rt.checkAgentBudget(tok, el) {
		return nil
	}
	env := rt.env(tok)
	prompt := interpolate(spec.Prompt, env)
	system := interpolate(spec.System, env)

	tools := make([]store.AgentToolSpec, len(spec.Tools))
	for i, t := range spec.Tools {
		tools[i] = store.AgentToolSpec{Name: t.Name, Description: t.Description, Schema: t.Schema, MCPServer: t.MCPServer}
	}

	rt.emit(store.HistAgentInvoked, el.ID, map[string]any{
		"agent": spec.Agent, "model": spec.Model, "topic": spec.Topic,
		"prompt": prompt, "tools": toolNames(tools),
	})
	rt.e.metrics.AgentInvocations.Add(1)

	// External AI worker path: park the token on an agent job that an AI
	// assistant fetches, runs, and completes over the REST API.
	if spec.Topic != "" {
		job := &store.AgentJob{
			ID:         rt.e.newID("agj"),
			TenantID:   rt.inst.TenantID,
			InstanceID: rt.inst.ID,
			TokenID:    tok.ID,
			ElementID:  el.ID,
			Agent:      spec.Agent,
			Topic:      spec.Topic,
			Prompt:     prompt,
			System:     system,
			Tools:      tools,
			Model:      spec.Model,
			Effort:     spec.Effort,
			MaxTokens:  spec.MaxTokens,
			Variables:  env,
			State:      store.AgentPending,
			Retries:    orDefaultInt(spec.Retries, 3),
			CreatedAt:  rt.e.now(),
		}
		if err := rt.e.st.PutAgentJob(job); err != nil {
			return err
		}
		tok.State = store.TokenWaitAgent
		tok.WaitRef = job.ID
		return nil
	}

	// In-process invoker path.
	inv := rt.e.agentInvoker(spec.Agent)
	if inv == nil {
		rt.raiseIncident(tok, "", fmt.Sprintf("agent task %s: no invoker registered for agent %q and no external topic", el.ID, spec.Agent))
		return nil
	}
	agentTools := make([]AgentTool, len(spec.Tools))
	for i, t := range spec.Tools {
		agentTools[i] = AgentTool{Name: t.Name, Description: t.Description, Schema: t.Schema, MCPServer: t.MCPServer}
	}
	resp, err := rt.e.invokeWithTimeout(inv, AgentRequest{
		InstanceID: rt.inst.ID, ElementID: el.ID, Agent: spec.Agent,
		Prompt: prompt, System: system, Tools: agentTools,
		Model: spec.Model, Effort: spec.Effort, MaxTokens: spec.MaxTokens,
		Variables: env, Traceparent: traceparentOf(env),
	})
	if err != nil {
		rt.handleActivityFailure(tok, el, err)
		return nil
	}
	rt.recordAgentResult(tok, el, resp.Output, resp.Text, resp.ToolCalls, resp.Usage)
	rt.chargeAgentUsage(resp.Usage)
	if !rt.reviewAgentResult(tok, el, resp.Output, resp.Text, resp.ToolCalls, resp.Usage) {
		return nil
	}
	return rt.finishAgent(tok, el, spec, resp.Output, resp.Text)
}

// recordAgentResult writes the governance trail for one agent decision.
func (rt *runtime) recordAgentResult(tok *store.Token, el *bpmn.Element, output map[string]any, text string, calls []AgentToolCall, usage AgentUsage) {
	detail := map[string]any{"agent": el.Agent.Agent}
	if len(calls) > 0 {
		cs := make([]any, len(calls))
		for i, c := range calls {
			cs[i] = map[string]any{"tool": c.Tool, "input": c.Input, "result": c.Result, "error": c.Error}
		}
		detail["toolCalls"] = cs
	}
	if usage != (AgentUsage{}) {
		detail["usage"] = map[string]any{
			"inputTokens": float64(usage.InputTokens), "outputTokens": float64(usage.OutputTokens),
			"costUsd": usage.CostUSD, "model": usage.Model,
		}
		rt.e.metrics.AgentInputTokens.Add(int64(usage.InputTokens))
		rt.e.metrics.AgentOutputTokens.Add(int64(usage.OutputTokens))
	}
	if text != "" {
		detail["output"] = text
	} else if len(output) > 0 {
		detail["output"] = output
	}
	rt.emit(store.HistAgentCompleted, el.ID, detail)
}

// finishAgent applies the agent's output — directly, or through a
// human-in-the-loop approval task when the spec requires sign-off.
func (rt *runtime) finishAgent(tok *store.Token, el *bpmn.Element, spec *bpmn.AgentSpec, output map[string]any, text string) error {
	if spec.HumanApproval {
		return rt.createApprovalTask(tok, el, spec, output, text)
	}
	rt.applyAgentOutput(el, spec, output, text)
	rt.finishSyncActivity(tok, el)
	return nil
}

// applyAgentOutput merges structured output and/or the scalar result into
// instance variables.
func (rt *runtime) applyAgentOutput(el *bpmn.Element, spec *bpmn.AgentSpec, output map[string]any, text string) {
	for k, v := range output {
		rt.inst.Variables[k] = expr.Normalize(v)
	}
	if spec.OutputVar != "" {
		if text != "" {
			rt.inst.Variables[spec.OutputVar] = text
		} else if _, set := output[spec.OutputVar]; !set && len(output) > 0 {
			rt.inst.Variables[spec.OutputVar] = output
		}
	}
}

// createApprovalTask routes the agent's proposed output to a user task; on
// completion the flow either applies the output or takes an error path.
func (rt *runtime) createApprovalTask(tok *store.Token, el *bpmn.Element, spec *bpmn.AgentSpec, output map[string]any, text string) error {
	proposal := text
	if proposal == "" {
		proposal = expr.Stringify(mapToValue(output))
	}
	task := &store.Task{
		ID:              rt.e.newID("task"),
		TenantID:        rt.inst.TenantID,
		InstanceID:      rt.inst.ID,
		TokenID:         tok.ID,
		ElementID:       el.ID,
		DefinitionKey:   rt.inst.DefinitionKey,
		Name:            "Approve: " + orDefault(el.Name, el.ID),
		State:           store.TaskCreated,
		CandidateGroups: spec.ApprovalGroups,
		FormKey:         "agent/approval",
		CreatedAt:       rt.e.now(),
	}
	if err := rt.e.st.PutTask(task); err != nil {
		return err
	}
	// Stash the proposal so completion can apply it.
	if tok.LocalVars == nil {
		tok.LocalVars = map[string]any{}
	}
	tok.LocalVars[agentProposalVar] = map[string]any{"output": output, "text": text}
	tok.State = store.TokenWaitTask
	tok.WaitRef = task.ID
	rt.e.metrics.TasksCreated.Add(1)
	rt.emit(store.HistTaskCreated, el.ID, map[string]any{"taskId": task.ID, "name": task.Name, "agentApproval": true, "proposal": proposal})
	return nil
}

const agentProposalVar = "__agentProposal"

// completeAgentJob applies an external AI worker's result and resumes the
// flow. Called from the REST layer via the engine.
func (e *Engine) completeAgentJobResult(job *store.AgentJob, output map[string]any, text string, calls []AgentToolCall, usage AgentUsage) error {
	return e.resume(job.InstanceID, func(rt *runtime) (bool, error) {
		tok := rt.inst.Tokens[job.TokenID]
		if tok == nil || tok.State != store.TokenWaitAgent || tok.WaitRef != job.ID {
			return false, fmt.Errorf("engine: agent job %s is no longer active", job.ID)
		}
		el := rt.element(tok)
		if el == nil || el.Agent == nil {
			return false, fmt.Errorf("engine: agent job %s element gone", job.ID)
		}
		job.State = store.AgentDone
		if err := e.st.PutAgentJob(job); err != nil {
			return false, err
		}
		rt.recordAgentResult(tok, el, output, text, calls, usage)
		rt.chargeAgentUsage(usage)
		// A guard rejection fails the element through the normal retry
		// cycle: the activity re-executes and creates a fresh agent job.
		if !rt.reviewAgentResult(tok, el, output, text, calls, usage) {
			return true, nil
		}
		return true, rt.finishAgent(tok, el, el.Agent, output, text)
	})
}

// ---- helpers ---------------------------------------------------------------

// interpolate expands ${expr} and {{var}} templates against vars. Unknown
// references render empty.
func interpolate(tmpl string, vars map[string]any) string {
	if tmpl == "" {
		return ""
	}
	out := tmpl
	out = expandDelim(out, "${", "}", vars)
	out = expandDelim(out, "{{", "}}", vars)
	return out
}

func expandDelim(s, open, close string, vars map[string]any) string {
	var b strings.Builder
	for {
		i := strings.Index(s, open)
		if i < 0 {
			b.WriteString(s)
			break
		}
		j := strings.Index(s[i+len(open):], close)
		if j < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		src := strings.TrimSpace(s[i+len(open) : i+len(open)+j])
		if v, err := expr.Eval(src, vars); err == nil {
			b.WriteString(expr.Stringify(v))
		}
		s = s[i+len(open)+j+len(close):]
	}
	return b.String()
}

func toolNames(tools []store.AgentToolSpec) []any {
	out := make([]any, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

func mapToValue(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

func traceparentOf(vars map[string]any) string {
	if v, ok := vars["__traceparent"].(string); ok {
		return v
	}
	return ""
}
