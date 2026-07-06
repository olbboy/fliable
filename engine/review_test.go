package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/olbboy/fliable/store"
)

// TestMIBoundaryRegisteredOnce: boundary events on a multi-instance
// activity belong to the loop coordinator — one registration total, one
// firing per trigger, and full cleanup when the loop ends.
func TestMIBoundaryRegisteredOnce(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <message id="m1" name="poke"/>
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="mi"/>
    <userTask id="mi">
      <multiInstanceLoopCharacteristics isSequential="false"
        fliable:collection="xs" fliable:elementVariable="x"/>
    </userTask>
    <boundaryEvent id="poked" attachedToRef="mi" cancelActivity="false">
      <messageEventDefinition messageRef="m1"/>
    </boundaryEvent>
    <sequenceFlow id="f2" sourceRef="mi" targetRef="e"/>
    <sequenceFlow id="f3" sourceRef="poked" targetRef="note"/>
    <scriptTask id="note" fliable:resultVariable="noted"><script>true</script></scriptTask>
    <sequenceFlow id="f4" sourceRef="note" targetRef="e2"/>
    <endEvent id="e2"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", map[string]any{"xs": []any{"a", "b", "c"}})

	// Exactly one subscription for the boundary despite 3 children.
	subs, _ := h.st.ListSubscriptions(store.SubscriptionFilter{InstanceID: inst.ID})
	if len(subs) != 1 {
		t.Fatalf("boundary subscriptions = %d, want 1", len(subs))
	}
	// One message = one boundary firing.
	if n, _ := h.e.CorrelateMessage("poke", "", nil); n != 1 {
		t.Fatalf("correlate = %d, want 1", n)
	}

	// Complete the loop: everything cleans up.
	for _, task := range h.tasks(inst.ID) {
		if err := h.e.CompleteTask(task.ID, nil, "u"); err != nil {
			t.Fatal(err)
		}
	}
	h.requireState(inst.ID, store.InstanceCompleted)
	subs, _ = h.st.ListSubscriptions(store.SubscriptionFilter{InstanceID: inst.ID})
	if len(subs) != 0 {
		t.Errorf("leaked subscriptions after loop end: %d", len(subs))
	}
	jobs, _ := h.st.ListJobs(inst.ID)
	if len(jobs) != 0 {
		t.Errorf("leaked jobs after loop end: %d", len(jobs))
	}
}

// TestOrphanSubscriptionIsDroppedAndNotCounted: a subscription whose wait
// state is gone must not count as a delivery, and gets deleted so it can
// never suppress message start events.
func TestOrphanSubscriptionIsDroppedAndNotCounted(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <message id="m1" name="orderMsg"/>
  <process id="waiter" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="w"/>
    <intermediateCatchEvent id="w"><messageEventDefinition messageRef="m1"/></intermediateCatchEvent>
    <sequenceFlow id="f2" sourceRef="w" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("waiter", nil)
	// Fabricate an orphan subscription referencing a dead token.
	orphan := &store.Subscription{
		ID: "sub_orphan", Kind: store.SubMessage, Name: "orderMsg",
		InstanceID: inst.ID, TokenID: "tok_gone", ElementID: "w", CreatedAt: h.now,
	}
	if err := h.st.PutSubscription(orphan); err != nil {
		t.Fatal(err)
	}
	// Both subs match; only the real one activates.
	n, err := h.e.CorrelateMessage("orderMsg", "", nil)
	if err != nil || n != 1 {
		t.Fatalf("correlate = %d, %v (orphan must not be counted)", n, err)
	}
	h.requireState(inst.ID, store.InstanceCompleted)
	subs, _ := h.st.ListSubscriptions(store.SubscriptionFilter{Name: "orderMsg"})
	if len(subs) != 0 {
		t.Errorf("orphan subscription survived: %+v", subs)
	}
}

// TestErrorBoundaryOnMISubprocessCancelsLoop: an error escaping one
// iteration cancels ALL iterations and runs the boundary path exactly
// once.
func TestErrorBoundaryOnMISubprocessCancelsLoop(t *testing.T) {
	h := newHarness(t)
	h.e.RegisterHandler("maybeBoom", func(ctx Context) (map[string]any, error) {
		if ctx.Variables["item"] == "bad" {
			return nil, NewBPMNError("ITEM_FAIL", "bad item")
		}
		return nil, nil
	})
	h.deploy(procXML(`
  <error id="err1" errorCode="ITEM_FAIL" name="ItemFail"/>
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="sub"/>
    <subProcess id="sub">
      <multiInstanceLoopCharacteristics isSequential="false"
        fliable:collection="items" fliable:elementVariable="item"/>
      <startEvent id="ss"/>
      <sequenceFlow id="sf1" sourceRef="ss" targetRef="check"/>
      <serviceTask id="check" fliable:type="maybeBoom"/>
      <sequenceFlow id="sf2" sourceRef="check" targetRef="inner"/>
      <userTask id="inner"/>
      <sequenceFlow id="sf3" sourceRef="inner" targetRef="se"/>
      <endEvent id="se"/>
    </subProcess>
    <boundaryEvent id="failed" attachedToRef="sub">
      <errorEventDefinition errorRef="err1"/>
    </boundaryEvent>
    <sequenceFlow id="f2" sourceRef="sub" targetRef="ok"/>
    <sequenceFlow id="f3" sourceRef="failed" targetRef="handle"/>
    <userTask id="handle" fliable:assignee="fixer"/>
    <sequenceFlow id="f4" sourceRef="handle" targetRef="e2"/>
    <endEvent id="e2"/>
    <endEvent id="ok"/>
  </process>`))

	inst := h.start("p", map[string]any{"items": []any{"good1", "bad", "good2"}})

	// Whole loop cancelled: no 'inner' tasks survive, exactly one
	// 'handle' task on the boundary path.
	open := h.tasks(inst.ID)
	if len(open) != 1 || open[0].Assignee != "fixer" {
		t.Fatalf("open tasks = %+v, want exactly the boundary handler", open)
	}
	if h.instance(inst.ID).Multi != nil && len(h.instance(inst.ID).Multi) != 0 {
		t.Errorf("loop state leaked: %+v", h.instance(inst.ID).Multi)
	}
	if err := h.e.CompleteTask(open[0].ID, nil, "fixer"); err != nil {
		t.Fatal(err)
	}
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.EndElement != "e2" {
		t.Errorf("endElement = %q", done.EndElement)
	}
	if done.Variables["errorCode"] != "ITEM_FAIL" {
		t.Errorf("errorCode = %v", done.Variables["errorCode"])
	}
}

// TestBoundaryTimerFiresDuringRetryBackoff: boundaries on inline service
// tasks arm at activity start, so an interrupting timer can cut a long
// retry loop short.
func TestBoundaryTimerFiresDuringRetryBackoff(t *testing.T) {
	h := newHarness(t)
	h.e.RegisterHandler("alwaysFails", func(ctx Context) (map[string]any, error) {
		return nil, errors.New("downstream unavailable")
	})
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="svc"/>
    <serviceTask id="svc" fliable:type="alwaysFails" fliable:retries="10"/>
    <boundaryEvent id="giveUp" attachedToRef="svc">
      <timerEventDefinition><timeDuration>PT30S</timeDuration></timerEventDefinition>
    </boundaryEvent>
    <sequenceFlow id="f2" sourceRef="svc" targetRef="ok"/>
    <sequenceFlow id="f3" sourceRef="giveUp" targetRef="fallback"/>
    <endEvent id="ok"/>
    <endEvent id="fallback"/>
  </process>`))

	inst := h.start("p", nil)
	h.requireState(inst.ID, store.InstanceActive) // parked on retry backoff
	h.advance(45 * time.Second)                   // boundary timer < next retry
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.EndElement != "fallback" {
		t.Errorf("endElement = %q, want fallback", done.EndElement)
	}
	// The pending retry job must be gone.
	if jobs, _ := h.st.ListJobs(inst.ID); len(jobs) != 0 {
		t.Errorf("leftover jobs: %v", jobs)
	}
}

// TestReconcileRepairsCrashArtifacts simulates the crash windows the
// journal cannot cover and verifies Start() repairs them.
func TestReconcileRepairsCrashArtifacts(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="child" isExecutable="true">
    <startEvent id="cs"/>
    <sequenceFlow id="cf" sourceRef="cs" targetRef="ce"/>
    <endEvent id="ce"/>
  </process>`))
	h.deploy(procXML(`
  <process id="parent" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="call"/>
    <callActivity id="call" calledElement="child"/>
    <sequenceFlow id="f2" sourceRef="call" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	// Craft the post-crash state by hand: parent parked on a child that
	// was never created (continuation lost).
	inst := &store.Instance{
		ID: "inst_crashed", DefinitionID: mustLatest(t, h, "parent").ID, DefinitionKey: "parent",
		State:     store.InstanceActive,
		Variables: map[string]any{},
		Tokens: map[string]*store.Token{
			"tok1": {ID: "tok1", ElementID: "call", State: store.TokenWaitChild, WaitRef: "inst_ghost"},
		},
		StartedAt: h.now,
	}
	if err := h.st.PutInstance(inst); err != nil {
		t.Fatal(err)
	}

	h.e.reconcile()

	f := false
	incs, _ := h.st.ListIncidents(store.IncidentFilter{InstanceID: "inst_crashed", Resolved: &f})
	if len(incs) != 1 {
		t.Fatalf("incidents after reconcile = %d, want 1", len(incs))
	}
	// Resolving the incident re-runs the call activity and completes the
	// parent (the child is trivial).
	if err := h.e.ResolveIncident(incs[0].ID); err != nil {
		t.Fatal(err)
	}
	h.requireState("inst_crashed", store.InstanceCompleted)

	// Lost-job repair: a token waiting on a timer job that no longer
	// exists re-arms itself.
	h.deploy(procXML(`
  <process id="waiter" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="w"/>
    <intermediateCatchEvent id="w">
      <timerEventDefinition><timeDuration>PT10M</timeDuration></timerEventDefinition>
    </intermediateCatchEvent>
    <sequenceFlow id="f2" sourceRef="w" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))
	winst := h.start("waiter", nil)
	// Simulate the claimed-then-lost job.
	jobs, _ := h.st.ListJobs(winst.ID)
	if len(jobs) != 1 {
		t.Fatal("expected one timer job")
	}
	_ = h.st.DeleteJob(jobs[0].ID)

	h.e.reconcile()
	jobs, _ = h.st.ListJobs(winst.ID)
	if len(jobs) != 1 {
		t.Fatalf("timer job not re-armed after reconcile: %d jobs", len(jobs))
	}
	h.advance(11 * time.Minute)
	h.requireState(winst.ID, store.InstanceCompleted)
}

func mustLatest(t *testing.T, h *harness, key string) *store.Definition {
	t.Helper()
	d, err := h.st.LatestDefinition(key)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
