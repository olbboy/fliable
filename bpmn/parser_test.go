package bpmn

import (
	"strings"
	"testing"
)

const orderXML = `<?xml version="1.0" encoding="UTF-8"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
             xmlns:fliable="https://fliable.dev/schema/1.0"
             id="defs1" targetNamespace="https://example.com/orders">
  <message id="msg_payment" name="paymentReceived"/>
  <signal id="sig_cancel" name="cancelAll"/>
  <error id="err_stock" name="OutOfStock" errorCode="OUT_OF_STOCK"/>
  <process id="orderProcess" name="Order fulfilment" isExecutable="true">
    <startEvent id="start" name="Order received"/>
    <sequenceFlow id="f1" sourceRef="start" targetRef="checkStock"/>
    <serviceTask id="checkStock" name="Check stock" fliable:type="stock.check" fliable:retries="5"/>
    <boundaryEvent id="stockErr" attachedToRef="checkStock">
      <errorEventDefinition errorRef="err_stock"/>
    </boundaryEvent>
    <sequenceFlow id="fErr" sourceRef="stockErr" targetRef="endRejected"/>
    <sequenceFlow id="f2" sourceRef="checkStock" targetRef="decide"/>
    <exclusiveGateway id="decide" name="In stock?" default="fNo"/>
    <sequenceFlow id="fYes" sourceRef="decide" targetRef="approve">
      <conditionExpression xsi:type="tFormalExpression">${stock &gt;= qty}</conditionExpression>
    </sequenceFlow>
    <sequenceFlow id="fNo" sourceRef="decide" targetRef="endRejected"/>
    <userTask id="approve" name="Approve order" fliable:assignee="alice"
              fliable:candidateGroups="sales, managers" fliable:formKey="forms/approve"/>
    <sequenceFlow id="f3" sourceRef="approve" targetRef="waitPay"/>
    <intermediateCatchEvent id="waitPay" name="Wait for payment">
      <messageEventDefinition messageRef="msg_payment"/>
    </intermediateCatchEvent>
    <sequenceFlow id="f4" sourceRef="waitPay" targetRef="ship"/>
    <subProcess id="ship" name="Shipping">
      <multiInstanceLoopCharacteristics isSequential="false" fliable:collection="parcels" fliable:elementVariable="parcel"/>
      <startEvent id="shipStart"/>
      <sequenceFlow id="sf1" sourceRef="shipStart" targetRef="doShip"/>
      <serviceTask id="doShip" fliable:topic="shipping"/>
      <sequenceFlow id="sf2" sourceRef="doShip" targetRef="shipEnd"/>
      <endEvent id="shipEnd"/>
    </subProcess>
    <sequenceFlow id="f5" sourceRef="ship" targetRef="end"/>
    <endEvent id="end" name="Done"/>
    <endEvent id="endRejected" name="Rejected"/>
  </process>
</definitions>`

func TestParseOrderProcess(t *testing.T) {
	defs, err := Parse([]byte(orderXML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if defs.Messages["msg_payment"] != "paymentReceived" {
		t.Errorf("message name = %q", defs.Messages["msg_payment"])
	}
	if defs.Errors["err_stock"].Code != "OUT_OF_STOCK" {
		t.Errorf("error code = %q", defs.Errors["err_stock"].Code)
	}

	p := defs.ProcessByID("orderProcess")
	if p == nil || !p.Executable {
		t.Fatal("orderProcess missing or not executable")
	}
	if got := len(p.Elements); got != 9 {
		t.Errorf("top-level elements = %d, want 9", got)
	}

	st := p.Elements["checkStock"]
	if st.TaskType != "stock.check" || st.Retries != 5 {
		t.Errorf("serviceTask = %+v", st)
	}

	be := p.Elements["stockErr"]
	if be.AttachedTo != "checkStock" || !be.CancelActivity || be.Event.Kind != KindError || be.Event.ErrorCode != "OUT_OF_STOCK" {
		t.Errorf("boundary = %+v ev=%+v", be, be.Event)
	}

	gw := p.Elements["decide"]
	if gw.DefaultFlow != "fNo" || len(gw.Outgoing) != 2 {
		t.Errorf("gateway = %+v", gw)
	}
	if cond := p.Flows["fYes"].Condition; cond != "stock >= qty" {
		t.Errorf("condition = %q (JUEL wrapper should be stripped)", cond)
	}

	ut := p.Elements["approve"]
	if ut.Assignee != "alice" || len(ut.CandidateGroups) != 2 || ut.CandidateGroups[1] != "managers" {
		t.Errorf("userTask = %+v", ut)
	}
	if ut.FormKey != "forms/approve" {
		t.Errorf("formKey = %q", ut.FormKey)
	}

	catch := p.Elements["waitPay"]
	if catch.Event.Kind != KindMessage || catch.Event.Message != "paymentReceived" {
		t.Errorf("catch event = %+v", catch.Event)
	}

	sub := p.Elements["ship"]
	if sub.Sub == nil || len(sub.Sub.Elements) != 3 {
		t.Fatalf("subprocess not parsed: %+v", sub)
	}
	if sub.MultiInstance == nil || sub.MultiInstance.Collection != "parcels" || sub.MultiInstance.Sequential {
		t.Errorf("multiInstance = %+v", sub.MultiInstance)
	}
	if sub.Sub.Elements["doShip"].Topic != "shipping" {
		t.Errorf("external topic = %q", sub.Sub.Elements["doShip"].Topic)
	}

	if scope := p.FindScope("doShip"); scope != sub.Sub {
		t.Error("FindScope failed for nested element")
	}
}

func TestParseFlowableCompatibility(t *testing.T) {
	xmlDoc := `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:flowable="http://flowable.org/bpmn" targetNamespace="t">
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="t1"/>
    <userTask id="t1" flowable:assignee="${initiator}" flowable:candidateGroups="ops"/>
    <sequenceFlow id="f2" sourceRef="t1" targetRef="t2"/>
    <serviceTask id="t2" flowable:delegateExpression="${sendInvoice}"/>
    <sequenceFlow id="f3" sourceRef="t2" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`
	defs, err := Parse([]byte(xmlDoc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := defs.Processes[0]
	if got := p.Elements["t1"].Assignee; got != "${initiator}" {
		t.Errorf("flowable assignee = %q", got)
	}
	if got := p.Elements["t2"].TaskType; got != "sendInvoice" {
		t.Errorf("delegateExpression should map to TaskType, got %q", got)
	}
}

func TestParseTimersAndSignals(t *testing.T) {
	xmlDoc := `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <signal id="sig1" name="go"/>
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="wait"/>
    <intermediateCatchEvent id="wait">
      <timerEventDefinition><timeDuration>PT10M</timeDuration></timerEventDefinition>
    </intermediateCatchEvent>
    <sequenceFlow id="f2" sourceRef="wait" targetRef="waitSig"/>
    <intermediateCatchEvent id="waitSig">
      <signalEventDefinition signalRef="sig1"/>
    </intermediateCatchEvent>
    <sequenceFlow id="f3" sourceRef="waitSig" targetRef="e"/>
    <endEvent id="e"><terminateEventDefinition/></endEvent>
  </process>
</definitions>`
	defs, err := Parse([]byte(xmlDoc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := defs.Processes[0]
	if ev := p.Elements["wait"].Event; ev.Kind != KindTimer || ev.TimerDuration != "PT10M" {
		t.Errorf("timer = %+v", ev)
	}
	if ev := p.Elements["waitSig"].Event; ev.Kind != KindSignal || ev.Signal != "go" {
		t.Errorf("signal = %+v", ev)
	}
	if ev := p.Elements["e"].Event; ev.Kind != KindTerminate {
		t.Errorf("terminate = %+v", ev)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		xml  string
		want string
	}{
		{
			name: "no start event",
			xml: `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <process id="p" isExecutable="true">
    <userTask id="t1"/>
    <sequenceFlow id="f" sourceRef="t1" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`,
			want: "no start event",
		},
		{
			name: "dangling flow target",
			xml: `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f" sourceRef="s" targetRef="ghost"/>
  </process>
</definitions>`,
			want: "unknown target",
		},
		{
			name: "boundary on unknown activity",
			xml: `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="e"/>
    <boundaryEvent id="b" attachedToRef="nope">
      <timerEventDefinition><timeDuration>PT1M</timeDuration></timerEventDefinition>
    </boundaryEvent>
    <sequenceFlow id="f2" sourceRef="b" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`,
			want: "unknown activity",
		},
		{
			name: "dead end task",
			xml: `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="t1"/>
    <userTask id="t1"/>
  </process>
</definitions>`,
			want: "dead end",
		},
		{
			name: "duplicate ids",
			xml: `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <userTask id="s"/>
  </process>
</definitions>`,
			want: "duplicate element id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.xml))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidationCollectsAllProblems(t *testing.T) {
	xmlDoc := `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <process id="p" isExecutable="true">
    <userTask id="t1"/>
    <sequenceFlow id="f" sourceRef="t1" targetRef="ghost"/>
  </process>
</definitions>`
	_, err := Parse([]byte(xmlDoc))
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsValidationError(err) {
		t.Fatalf("want ValidationError, got %T", err)
	}
	msg := err.Error()
	for _, want := range []string{"no start event", "unknown target"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error missing %q in:\n%s", want, msg)
		}
	}
}

func TestNonExecutableProcessSkipsValidation(t *testing.T) {
	xmlDoc := `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <process id="doc-only" isExecutable="false">
    <userTask id="orphan"/>
  </process>
  <process id="real" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f" sourceRef="s" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`
	defs, err := Parse([]byte(xmlDoc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := defs.FirstExecutable(); got == nil || got.ID != "real" {
		t.Errorf("FirstExecutable = %v", got)
	}
}
