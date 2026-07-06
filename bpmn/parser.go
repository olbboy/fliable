package bpmn

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// Extension namespaces accepted by the parser. Fliable reads its own
// namespace plus the Flowable/Camunda/Activiti namespaces so existing
// models migrate without edits.
var extensionNamespaces = map[string]bool{
	"https://fliable.dev/schema/1.0":     true,
	"http://flowable.org/bpmn":           true,
	"http://camunda.org/schema/1.0/bpmn": true,
	"http://activiti.org/bpmn":           true,
}

const bpmnNS = "http://www.omg.org/spec/BPMN/20100524/MODEL"

// Parse parses a BPMN 2.0 XML document into Definitions and validates its
// structure. It returns the first error encountered.
func Parse(data []byte) (*Definitions, error) {
	var root xDefinitions
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("bpmn: malformed XML: %w", err)
	}
	defs := &Definitions{
		ID:              root.ID,
		TargetNamespace: root.TargetNamespace,
		Messages:        map[string]string{},
		Signals:         map[string]string{},
		Errors:          map[string]Error{},
	}
	for _, m := range root.Messages {
		name := m.Name
		if name == "" {
			name = m.ID
		}
		defs.Messages[m.ID] = name
	}
	for _, s := range root.Signals {
		name := s.Name
		if name == "" {
			name = s.ID
		}
		defs.Signals[s.ID] = name
	}
	for _, e := range root.Errors {
		code := e.ErrorCode
		if code == "" {
			code = e.Name
		}
		defs.Errors[e.ID] = Error{ID: e.ID, Name: e.Name, Code: code}
	}
	for _, xp := range root.Processes {
		p := &Process{
			ID:         xp.ID,
			Name:       xp.Name,
			Executable: xp.Executable == "" || xp.Executable == "true",
		}
		c, err := buildContainer(defs, xp.xContainer)
		if err != nil {
			return nil, fmt.Errorf("bpmn: process %q: %w", xp.ID, err)
		}
		p.Container = *c
		defs.Processes = append(defs.Processes, p)
	}
	if err := Validate(defs); err != nil {
		return nil, err
	}
	return defs, nil
}

// buildContainer converts one parsed XML scope into a model Container.
func buildContainer(defs *Definitions, xc xContainer) (*Container, error) {
	c := &Container{
		Elements: map[string]*Element{},
		Flows:    map[string]*SequenceFlow{},
	}
	add := func(el *Element) error {
		if el.ID == "" {
			return fmt.Errorf("element of type %s has no id", el.Type)
		}
		if _, dup := c.Elements[el.ID]; dup {
			return fmt.Errorf("duplicate element id %q", el.ID)
		}
		c.Elements[el.ID] = el
		c.Order = append(c.Order, el.ID)
		return nil
	}

	for i := range xc.StartEvents {
		x := &xc.StartEvents[i]
		el := baseElement(x.xFlowNode, TypeStartEvent)
		ev, err := eventDef(defs, x.xEventDefs)
		if err != nil {
			return nil, fmt.Errorf("startEvent %q: %w", x.ID, err)
		}
		el.Event = ev
		el.FormKey = x.FormKey.value()
		// Event sub-process start events may be non-interrupting; reuse
		// CancelActivity ("true" unless explicitly disabled).
		el.CancelActivity = x.Interrupting == "" || x.Interrupting == "true"
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.EndEvents {
		x := &xc.EndEvents[i]
		el := baseElement(x.xFlowNode, TypeEndEvent)
		ev, err := eventDef(defs, x.xEventDefs)
		if err != nil {
			return nil, fmt.Errorf("endEvent %q: %w", x.ID, err)
		}
		el.Event = ev
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.IntermediateCatch {
		x := &xc.IntermediateCatch[i]
		el := baseElement(x.xFlowNode, TypeIntermediateCatchEvent)
		ev, err := eventDef(defs, x.xEventDefs)
		if err != nil {
			return nil, fmt.Errorf("intermediateCatchEvent %q: %w", x.ID, err)
		}
		if ev == nil || ev.Kind == KindNone {
			return nil, fmt.Errorf("intermediateCatchEvent %q: missing event definition", x.ID)
		}
		el.Event = ev
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.IntermediateThrow {
		x := &xc.IntermediateThrow[i]
		el := baseElement(x.xFlowNode, TypeIntermediateThrowEvent)
		ev, err := eventDef(defs, x.xEventDefs)
		if err != nil {
			return nil, fmt.Errorf("intermediateThrowEvent %q: %w", x.ID, err)
		}
		el.Event = ev
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.BoundaryEvents {
		x := &xc.BoundaryEvents[i]
		el := baseElement(x.xFlowNode, TypeBoundaryEvent)
		ev, err := eventDef(defs, x.xEventDefs)
		if err != nil {
			return nil, fmt.Errorf("boundaryEvent %q: %w", x.ID, err)
		}
		if ev == nil || ev.Kind == KindNone {
			return nil, fmt.Errorf("boundaryEvent %q: missing event definition", x.ID)
		}
		el.Event = ev
		el.AttachedTo = x.AttachedToRef
		el.CancelActivity = x.CancelActivity == "" || x.CancelActivity == "true"
		if err := add(el); err != nil {
			return nil, err
		}
	}

	for i := range xc.UserTasks {
		x := &xc.UserTasks[i]
		el := baseElement(x.xFlowNode, TypeUserTask)
		el.Assignee = x.Assignee.value()
		el.CandidateUsers = splitList(x.CandidateUsers.value())
		el.CandidateGroups = splitList(x.CandidateGroups.value())
		el.FormKey = x.FormKey.value()
		el.DueDate = x.DueDate.value()
		el.Priority = x.Priority.value()
		applyActivity(el, &x.xActivity)
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.ServiceTasks {
		x := &xc.ServiceTasks[i]
		el := baseElement(x.xFlowNode, TypeServiceTask)
		el.TaskType = stripExprWrapper(firstNonEmpty(x.TaskType.value(), x.DelegateExpression.value(), x.Class.value()))
		el.Topic = x.Topic.value()
		el.Expression = stripExprWrapper(x.Expression.value())
		el.ResultVar = x.ResultVar.value()
		el.Retries = atoiDefault(x.Retries.value(), 3)
		applyActivity(el, &x.xActivity)
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.ScriptTasks {
		x := &xc.ScriptTasks[i]
		el := baseElement(x.xFlowNode, TypeScriptTask)
		el.Expression = stripExprWrapper(x.Script)
		el.ResultVar = firstNonEmpty(x.ResultVar.value(), x.ResultVariable)
		applyActivity(el, &x.xActivity)
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.BusinessRuleTasks {
		x := &xc.BusinessRuleTasks[i]
		el := baseElement(x.xFlowNode, TypeBusinessRuleTask)
		el.DecisionRef = x.DecisionRef.value()
		el.ResultVar = x.ResultVar.value()
		applyActivity(el, &x.xActivity)
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.SendTasks {
		x := &xc.SendTasks[i]
		el := baseElement(x.xFlowNode, TypeSendTask)
		el.TaskType = x.TaskType.value()
		el.Topic = x.Topic.value()
		if x.MessageRef != "" {
			el.Event = &EventDefinition{Kind: KindMessage, Message: defs.Messages[x.MessageRef]}
		}
		applyActivity(el, &x.xActivity)
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.ReceiveTasks {
		x := &xc.ReceiveTasks[i]
		el := baseElement(x.xFlowNode, TypeReceiveTask)
		msg := defs.Messages[x.MessageRef]
		if msg == "" {
			msg = x.MessageRef
		}
		el.Event = &EventDefinition{Kind: KindMessage, Message: msg}
		applyActivity(el, &x.xActivity)
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.ManualTasks {
		x := &xc.ManualTasks[i]
		el := baseElement(x.xFlowNode, TypeManualTask)
		applyActivity(el, &x.xActivity)
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.Tasks {
		x := &xc.Tasks[i]
		el := baseElement(x.xFlowNode, TypeTask)
		applyActivity(el, &x.xActivity)
		if err := add(el); err != nil {
			return nil, err
		}
	}

	for i := range xc.ExclusiveGateways {
		x := &xc.ExclusiveGateways[i]
		el := baseElement(x.xFlowNode, TypeExclusiveGateway)
		el.DefaultFlow = x.Default
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.ParallelGateways {
		x := &xc.ParallelGateways[i]
		el := baseElement(x.xFlowNode, TypeParallelGateway)
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.InclusiveGateways {
		x := &xc.InclusiveGateways[i]
		el := baseElement(x.xFlowNode, TypeInclusiveGateway)
		el.DefaultFlow = x.Default
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.EventBasedGateways {
		x := &xc.EventBasedGateways[i]
		el := baseElement(x.xFlowNode, TypeEventBasedGateway)
		if err := add(el); err != nil {
			return nil, err
		}
	}

	for i := range xc.SubProcesses {
		x := &xc.SubProcesses[i]
		el := baseElement(x.xFlowNode, TypeSubProcess)
		el.TriggeredByEvent = x.TriggeredByEvent == "true"
		sub, err := buildContainer(defs, x.xContainer)
		if err != nil {
			return nil, fmt.Errorf("subProcess %q: %w", x.ID, err)
		}
		el.Sub = sub
		applyActivity(el, &x.xActivity)
		if err := add(el); err != nil {
			return nil, err
		}
	}
	for i := range xc.CallActivities {
		x := &xc.CallActivities[i]
		el := baseElement(x.xFlowNode, TypeCallActivity)
		el.CalledElement = x.CalledElement
		applyActivity(el, &x.xActivity)
		if err := add(el); err != nil {
			return nil, err
		}
	}

	// Build flows and wire incoming/outgoing in document order; the
	// <incoming>/<outgoing> children are optional in many modelers.
	for i := range xc.SequenceFlows {
		x := &xc.SequenceFlows[i]
		if x.ID == "" {
			return nil, fmt.Errorf("sequenceFlow from %q to %q has no id", x.SourceRef, x.TargetRef)
		}
		if _, dup := c.Flows[x.ID]; dup {
			return nil, fmt.Errorf("duplicate sequenceFlow id %q", x.ID)
		}
		f := &SequenceFlow{
			ID:        x.ID,
			Name:      x.Name,
			SourceRef: x.SourceRef,
			TargetRef: x.TargetRef,
			Condition: stripExprWrapper(x.Condition),
		}
		c.Flows[f.ID] = f
		if src, ok := c.Elements[f.SourceRef]; ok {
			src.Outgoing = append(src.Outgoing, f.ID)
		}
		if dst, ok := c.Elements[f.TargetRef]; ok {
			dst.Incoming = append(dst.Incoming, f.ID)
		}
	}
	return c, nil
}

// stripExprWrapper removes JUEL-style ${...} / #{...} wrappers so Flowable
// and Camunda condition expressions evaluate as plain Fliable expressions.
func stripExprWrapper(s string) string {
	s = strings.TrimSpace(s)
	if (strings.HasPrefix(s, "${") || strings.HasPrefix(s, "#{")) && strings.HasSuffix(s, "}") {
		return strings.TrimSpace(s[2 : len(s)-1])
	}
	return s
}

func baseElement(x xFlowNode, t ElementType) *Element {
	return &Element{
		ID:            x.ID,
		Name:          x.Name,
		Type:          t,
		Documentation: strings.TrimSpace(x.Documentation),
	}
}

func applyActivity(el *Element, a *xActivity) {
	el.DefaultFlow = a.Default
	el.Async = a.Async.value() == "true" || a.AsyncBefore.value() == "true"
	if a.MultiInstance != nil {
		mi := &MultiInstance{
			Sequential:          a.MultiInstance.IsSequential == "true",
			Cardinality:         stripExprWrapper(a.MultiInstance.Cardinality),
			Collection:          stripExprWrapper(firstNonEmpty(a.MultiInstance.Collection.value(), strings.TrimSpace(a.MultiInstance.DataInput))),
			ElementVariable:     a.MultiInstance.ElementVariable.value(),
			CompletionCondition: stripExprWrapper(a.MultiInstance.CompletionCondition),
			OutputCollection:    a.MultiInstance.OutputCollection.value(),
			OutputElement:       stripExprWrapper(a.MultiInstance.OutputElement.value()),
		}
		el.MultiInstance = mi
	}
	for _, in := range a.Inputs {
		el.Inputs = append(el.Inputs, IOMapping{Source: stripExprWrapper(in.Source), Target: in.Target})
	}
	for _, out := range a.Outputs {
		el.Outputs = append(el.Outputs, IOMapping{Source: stripExprWrapper(out.Source), Target: out.Target})
	}
}

func eventDef(defs *Definitions, x xEventDefs) (*EventDefinition, error) {
	switch {
	case x.Timer != nil:
		ev := &EventDefinition{
			Kind:          KindTimer,
			TimerDate:     strings.TrimSpace(x.Timer.TimeDate),
			TimerDuration: strings.TrimSpace(x.Timer.TimeDuration),
			TimerCycle:    strings.TrimSpace(x.Timer.TimeCycle),
		}
		if ev.TimerDate == "" && ev.TimerDuration == "" && ev.TimerCycle == "" {
			return nil, fmt.Errorf("timer event has no timeDate, timeDuration or timeCycle")
		}
		return ev, nil
	case x.Message != nil:
		name := defs.Messages[x.Message.MessageRef]
		if name == "" {
			name = x.Message.MessageRef
		}
		if name == "" {
			return nil, fmt.Errorf("message event has no messageRef")
		}
		return &EventDefinition{Kind: KindMessage, Message: name}, nil
	case x.Signal != nil:
		name := defs.Signals[x.Signal.SignalRef]
		if name == "" {
			name = x.Signal.SignalRef
		}
		if name == "" {
			return nil, fmt.Errorf("signal event has no signalRef")
		}
		return &EventDefinition{Kind: KindSignal, Signal: name}, nil
	case x.Error != nil:
		code := ""
		if e, ok := defs.Errors[x.Error.ErrorRef]; ok {
			code = e.Code
		} else {
			code = x.Error.ErrorRef
		}
		return &EventDefinition{Kind: KindError, ErrorCode: code}, nil
	case x.Terminate != nil:
		return &EventDefinition{Kind: KindTerminate}, nil
	}
	return &EventDefinition{Kind: KindNone}, nil
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return def
	}
	return n
}

// ---- raw XML shapes -------------------------------------------------------

// extAttr is an attribute that may come from any accepted extension
// namespace (fliable, flowable, camunda, activiti) or no namespace at all.
type extAttr struct {
	vals []string
}

func (a *extAttr) value() string {
	for _, v := range a.vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// UnmarshalXMLAttr collects the attribute value regardless of namespace;
// callers declare the local attribute name via the `xml` tag.
func (a *extAttr) UnmarshalXMLAttr(attr xml.Attr) error {
	if attr.Name.Space == "" || extensionNamespaces[attr.Name.Space] {
		a.vals = append(a.vals, attr.Value)
	}
	return nil
}

type xDefinitions struct {
	XMLName         xml.Name   `xml:"definitions"`
	ID              string     `xml:"id,attr"`
	TargetNamespace string     `xml:"targetNamespace,attr"`
	Messages        []xNamed   `xml:"message"`
	Signals         []xNamed   `xml:"signal"`
	Errors          []xError   `xml:"error"`
	Processes       []xProcess `xml:"process"`
}

type xNamed struct {
	ID   string `xml:"id,attr"`
	Name string `xml:"name,attr"`
}

type xError struct {
	ID        string `xml:"id,attr"`
	Name      string `xml:"name,attr"`
	ErrorCode string `xml:"errorCode,attr"`
}

type xProcess struct {
	ID         string `xml:"id,attr"`
	Name       string `xml:"name,attr"`
	Executable string `xml:"isExecutable,attr"`
	xContainer
}

type xContainer struct {
	StartEvents        []xCatchEvent   `xml:"startEvent"`
	EndEvents          []xCatchEvent   `xml:"endEvent"`
	IntermediateCatch  []xCatchEvent   `xml:"intermediateCatchEvent"`
	IntermediateThrow  []xCatchEvent   `xml:"intermediateThrowEvent"`
	BoundaryEvents     []xBoundary     `xml:"boundaryEvent"`
	UserTasks          []xUserTask     `xml:"userTask"`
	ServiceTasks       []xServiceTask  `xml:"serviceTask"`
	ScriptTasks        []xScriptTask   `xml:"scriptTask"`
	BusinessRuleTasks  []xRuleTask     `xml:"businessRuleTask"`
	SendTasks          []xSendTask     `xml:"sendTask"`
	ReceiveTasks       []xReceiveTask  `xml:"receiveTask"`
	ManualTasks        []xPlainTask    `xml:"manualTask"`
	Tasks              []xPlainTask    `xml:"task"`
	ExclusiveGateways  []xGateway      `xml:"exclusiveGateway"`
	ParallelGateways   []xGateway      `xml:"parallelGateway"`
	InclusiveGateways  []xGateway      `xml:"inclusiveGateway"`
	EventBasedGateways []xGateway      `xml:"eventBasedGateway"`
	SubProcesses       []xSubProcess   `xml:"subProcess"`
	CallActivities     []xCallActivity `xml:"callActivity"`
	SequenceFlows      []xSequenceFlow `xml:"sequenceFlow"`
}

type xFlowNode struct {
	ID            string `xml:"id,attr"`
	Name          string `xml:"name,attr"`
	Documentation string `xml:"documentation"`
}

type xEventDefs struct {
	Timer     *xTimerDef   `xml:"timerEventDefinition"`
	Message   *xMessageDef `xml:"messageEventDefinition"`
	Signal    *xSignalDef  `xml:"signalEventDefinition"`
	Error     *xErrorDef   `xml:"errorEventDefinition"`
	Terminate *struct{}    `xml:"terminateEventDefinition"`
}

type xTimerDef struct {
	TimeDate     string `xml:"timeDate"`
	TimeDuration string `xml:"timeDuration"`
	TimeCycle    string `xml:"timeCycle"`
}

type xMessageDef struct {
	MessageRef string `xml:"messageRef,attr"`
}

type xSignalDef struct {
	SignalRef string `xml:"signalRef,attr"`
}

type xErrorDef struct {
	ErrorRef string `xml:"errorRef,attr"`
}

type xCatchEvent struct {
	xFlowNode
	xEventDefs
	FormKey      extAttr `xml:"formKey,attr"`
	Interrupting string  `xml:"isInterrupting,attr"`
}

type xBoundary struct {
	xFlowNode
	xEventDefs
	AttachedToRef  string `xml:"attachedToRef,attr"`
	CancelActivity string `xml:"cancelActivity,attr"`
}

type xMulti struct {
	IsSequential        string  `xml:"isSequential,attr"`
	Cardinality         string  `xml:"loopCardinality"`
	CompletionCondition string  `xml:"completionCondition"`
	DataInput           string  `xml:"loopDataInputRef"`
	Collection          extAttr `xml:"collection,attr"`
	ElementVariable     extAttr `xml:"elementVariable,attr"`
	OutputCollection    extAttr `xml:"outputCollection,attr"`
	OutputElement       extAttr `xml:"outputElement,attr"`
}

type xExtIO struct {
	Inputs  []xIOEntry `xml:"extensionElements>inputOutput>inputParameter"`
	Outputs []xIOEntry `xml:"extensionElements>inputOutput>outputParameter"`
}

type xIOEntry struct {
	Target string `xml:"name,attr"`
	Source string `xml:",chardata"`
}

type xActivity struct {
	Default       string  `xml:"default,attr"`
	Async         extAttr `xml:"async,attr"`
	AsyncBefore   extAttr `xml:"asyncBefore,attr"`
	MultiInstance *xMulti `xml:"multiInstanceLoopCharacteristics"`
	xExtIO
}

type xUserTask struct {
	xFlowNode
	xActivity
	Assignee        extAttr `xml:"assignee,attr"`
	CandidateUsers  extAttr `xml:"candidateUsers,attr"`
	CandidateGroups extAttr `xml:"candidateGroups,attr"`
	FormKey         extAttr `xml:"formKey,attr"`
	DueDate         extAttr `xml:"dueDate,attr"`
	Priority        extAttr `xml:"priority,attr"`
}

type xServiceTask struct {
	xFlowNode
	xActivity
	TaskType           extAttr `xml:"type,attr"`
	Topic              extAttr `xml:"topic,attr"`
	DelegateExpression extAttr `xml:"delegateExpression,attr"`
	Class              extAttr `xml:"class,attr"`
	Expression         extAttr `xml:"expression,attr"`
	ResultVar          extAttr `xml:"resultVariable,attr"`
	Retries            extAttr `xml:"retries,attr"`
}

type xScriptTask struct {
	xFlowNode
	xActivity
	ScriptFormat   string  `xml:"scriptFormat,attr"`
	Script         string  `xml:"script"`
	ResultVar      extAttr `xml:"resultVariable,attr"`
	ResultVariable string  `xml:"resultVariableName,attr"`
}

type xRuleTask struct {
	xFlowNode
	xActivity
	DecisionRef extAttr `xml:"decisionRef,attr"`
	ResultVar   extAttr `xml:"resultVariable,attr"`
}

type xSendTask struct {
	xFlowNode
	xActivity
	TaskType   extAttr `xml:"type,attr"`
	Topic      extAttr `xml:"topic,attr"`
	MessageRef string  `xml:"messageRef,attr"`
}

type xReceiveTask struct {
	xFlowNode
	xActivity
	MessageRef string `xml:"messageRef,attr"`
}

type xPlainTask struct {
	xFlowNode
	xActivity
}

type xGateway struct {
	xFlowNode
	Default string `xml:"default,attr"`
}

type xSubProcess struct {
	xFlowNode
	xActivity
	TriggeredByEvent string `xml:"triggeredByEvent,attr"`
	xContainer
}

type xCallActivity struct {
	xFlowNode
	xActivity
	CalledElement string `xml:"calledElement,attr"`
}

type xSequenceFlow struct {
	ID        string `xml:"id,attr"`
	Name      string `xml:"name,attr"`
	SourceRef string `xml:"sourceRef,attr"`
	TargetRef string `xml:"targetRef,attr"`
	Condition string `xml:"conditionExpression"`
}
