// Package bpmn contains the BPMN 2.0 process model and XML parser used by
// the Fliable engine.
//
// The model is deliberately flat and engine-oriented: instead of mirroring
// the deep BPMN class hierarchy, every flow node is an *Element carrying a
// Type discriminator plus the union of properties the engine needs. This
// keeps the runtime hot path allocation-free and trivially serializable.
//
// The parser understands standard BPMN 2.0 XML and, for drop-in migration,
// also reads vendor extension attributes from Flowable, Camunda and
// Activiti namespaces (assignee, candidate users/groups, service task
// topics, form keys, async flags, ...). Fliable's own extension namespace
// is https://fliable.dev/schema/1.0.
package bpmn

// ElementType identifies the kind of a BPMN flow node.
type ElementType string

// Supported BPMN 2.0 flow node types.
const (
	TypeStartEvent             ElementType = "startEvent"
	TypeEndEvent               ElementType = "endEvent"
	TypeIntermediateCatchEvent ElementType = "intermediateCatchEvent"
	TypeIntermediateThrowEvent ElementType = "intermediateThrowEvent"
	TypeBoundaryEvent          ElementType = "boundaryEvent"
	TypeUserTask               ElementType = "userTask"
	TypeServiceTask            ElementType = "serviceTask"
	TypeScriptTask             ElementType = "scriptTask"
	TypeBusinessRuleTask       ElementType = "businessRuleTask"
	TypeSendTask               ElementType = "sendTask"
	TypeReceiveTask            ElementType = "receiveTask"
	TypeManualTask             ElementType = "manualTask"
	TypeTask                   ElementType = "task"
	TypeExclusiveGateway       ElementType = "exclusiveGateway"
	TypeParallelGateway        ElementType = "parallelGateway"
	TypeInclusiveGateway       ElementType = "inclusiveGateway"
	TypeEventBasedGateway      ElementType = "eventBasedGateway"
	TypeSubProcess             ElementType = "subProcess"
	TypeCallActivity           ElementType = "callActivity"
)

// EventKind classifies the event definition attached to an event element.
type EventKind string

// Supported event definition kinds. KindNone marks plain (blank) events.
const (
	KindNone      EventKind = "none"
	KindTimer     EventKind = "timer"
	KindMessage   EventKind = "message"
	KindSignal    EventKind = "signal"
	KindError     EventKind = "error"
	KindTerminate EventKind = "terminate"
)

// Definitions is the root of a parsed BPMN document.
type Definitions struct {
	ID              string
	TargetNamespace string
	Processes       []*Process
	Messages        map[string]string // id -> name
	Signals         map[string]string // id -> name
	Errors          map[string]Error  // id -> error
}

// Error is a named BPMN error that can be thrown and caught.
type Error struct {
	ID   string
	Name string
	Code string
}

// Process is an executable BPMN process: a container of flow nodes plus
// process-level metadata.
type Process struct {
	ID         string
	Name       string
	Executable bool
	Container
}

// Container holds the flow nodes and sequence flows of one scope (a process
// or an embedded sub-process).
type Container struct {
	Elements map[string]*Element
	Flows    map[string]*SequenceFlow
	// Order preserves document order of element IDs, used for deterministic
	// iteration (validation messages, start event lookup).
	Order []string
}

// StartEvents returns the container's start events in document order.
func (c *Container) StartEvents() []*Element {
	var out []*Element
	for _, id := range c.Order {
		if el := c.Elements[id]; el != nil && el.Type == TypeStartEvent {
			out = append(out, el)
		}
	}
	return out
}

// NoneStartEvent returns the first start event without an event definition,
// or nil if the container has none.
func (c *Container) NoneStartEvent() *Element {
	for _, el := range c.StartEvents() {
		if el.Event == nil || el.Event.Kind == KindNone {
			return el
		}
	}
	return nil
}

// SequenceFlow connects two flow nodes, optionally guarded by a condition
// expression written in the Fliable expression language.
type SequenceFlow struct {
	ID        string
	Name      string
	SourceRef string
	TargetRef string
	Condition string
}

// EventDefinition describes the trigger of an event element.
type EventDefinition struct {
	Kind EventKind

	// Timer fields hold raw expressions: ISO-8601 durations ("PT5M"),
	// RFC3339 / ISO dates, or repeating cycles ("R3/PT10S").
	TimerDate     string
	TimerDuration string
	TimerCycle    string

	// Message/Signal hold the resolved name (not the XML id).
	Message string
	Signal  string

	// ErrorCode is the resolved error code; empty matches any error.
	ErrorCode string
}

// MultiInstance configures multi-instance (loop) execution of an activity.
type MultiInstance struct {
	Sequential          bool
	Cardinality         string // expression yielding the number of instances
	Collection          string // expression yielding a slice to iterate
	ElementVariable     string // variable holding the current item
	CompletionCondition string // expression checked after each completion
	OutputCollection    string // variable collecting per-instance results
	OutputElement       string // expression evaluated per instance for output
}

// IOMapping maps a variable between scopes (call activity, subprocess or
// task input/output).
type IOMapping struct {
	Source string // expression evaluated in the source scope
	Target string // variable name in the target scope
}

// Element is a single BPMN flow node. It is a flat union of the fields
// needed by every supported element type; unused fields stay zero.
type Element struct {
	ID   string
	Name string
	Type ElementType

	Incoming []string // sequence flow IDs
	Outgoing []string // sequence flow IDs

	// Documentation from the BPMN <documentation> child.
	Documentation string

	// Event holds the event definition for event elements.
	Event *EventDefinition

	// Boundary event attachment.
	AttachedTo     string
	CancelActivity bool // true = interrupting boundary event

	// DefaultFlow is the default sequence flow of a gateway or activity.
	DefaultFlow string

	// User task properties.
	Assignee        string
	CandidateUsers  []string
	CandidateGroups []string
	FormKey         string
	DueDate         string // expression
	Priority        string // expression

	// Service/send task properties. TaskType names a registered Go handler;
	// Topic marks the task for external workers instead.
	TaskType string
	Topic    string
	Retries  int

	// Script task / expression-based service task.
	Expression string
	ResultVar  string

	// Business rule task.
	DecisionRef string

	// Call activity.
	CalledElement string

	// Receive task / message events resolved message name is in Event.

	// Embedded sub-process body.
	Sub              *Container
	TriggeredByEvent bool // event sub-process

	// Multi-instance loop characteristics, nil if not multi-instance.
	MultiInstance *MultiInstance

	// Input/output variable mappings.
	Inputs  []IOMapping
	Outputs []IOMapping

	// Async requests asynchronous continuation before this element.
	Async bool
}

// IsActivity reports whether the element is an activity that boundary
// events may attach to and multi-instance may apply to.
func (e *Element) IsActivity() bool {
	switch e.Type {
	case TypeUserTask, TypeServiceTask, TypeScriptTask, TypeBusinessRuleTask,
		TypeSendTask, TypeReceiveTask, TypeManualTask, TypeTask,
		TypeSubProcess, TypeCallActivity:
		return true
	}
	return false
}

// IsGateway reports whether the element is a gateway.
func (e *Element) IsGateway() bool {
	switch e.Type {
	case TypeExclusiveGateway, TypeParallelGateway, TypeInclusiveGateway, TypeEventBasedGateway:
		return true
	}
	return false
}

// ProcessByID returns the process with the given id, or nil.
func (d *Definitions) ProcessByID(id string) *Process {
	for _, p := range d.Processes {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// FirstExecutable returns the first executable process, or nil.
func (d *Definitions) FirstExecutable() *Process {
	for _, p := range d.Processes {
		if p.Executable {
			return p
		}
	}
	return nil
}

// FindScope locates the container that directly holds elementID, searching
// the process scope and nested sub-processes depth-first. Returns nil if
// the element is unknown.
func (p *Process) FindScope(elementID string) *Container {
	return findScope(&p.Container, elementID)
}

func findScope(c *Container, elementID string) *Container {
	if _, ok := c.Elements[elementID]; ok {
		return c
	}
	for _, el := range c.Elements {
		if el.Sub != nil {
			if s := findScope(el.Sub, elementID); s != nil {
				return s
			}
		}
	}
	return nil
}

// FindElement locates an element anywhere in the process, or nil.
func (p *Process) FindElement(elementID string) *Element {
	if s := p.FindScope(elementID); s != nil {
		return s.Elements[elementID]
	}
	return nil
}

// BoundaryEvents returns the boundary events attached to the given activity
// within the scope, in document order.
func (c *Container) BoundaryEvents(activityID string) []*Element {
	var out []*Element
	for _, id := range c.Order {
		el := c.Elements[id]
		if el != nil && el.Type == TypeBoundaryEvent && el.AttachedTo == activityID {
			out = append(out, el)
		}
	}
	return out
}
