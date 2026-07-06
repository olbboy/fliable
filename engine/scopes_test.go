package engine

import (
	"testing"

	"github.com/olbboy/fliable/store"
)

// TestMultiInstanceParallelSubProcess is the regression test for
// concurrent sub-process iterations sharing a scope path: completing one
// iteration must not resume another iteration's owner token, and the
// loop only finishes when every iteration's scope is empty.
func TestMultiInstanceParallelSubProcess(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="sub"/>
    <subProcess id="sub">
      <multiInstanceLoopCharacteristics isSequential="false"
        fliable:collection="regions" fliable:elementVariable="region"/>
      <startEvent id="ss"/>
      <sequenceFlow id="sf1" sourceRef="ss" targetRef="approve"/>
      <userTask id="approve" fliable:assignee="${region}"/>
      <sequenceFlow id="sf2" sourceRef="approve" targetRef="se"/>
      <endEvent id="se"/>
    </subProcess>
    <sequenceFlow id="f2" sourceRef="sub" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", map[string]any{"regions": []any{"eu", "us", "apac"}})
	tasks := h.tasks(inst.ID)
	if len(tasks) != 3 {
		t.Fatalf("tasks = %d, want 3 (one per iteration)", len(tasks))
	}

	// Complete one iteration: the loop must NOT finish and the other two
	// tasks must stay open.
	if err := h.e.CompleteTask(tasks[0].ID, nil, tasks[0].Assignee); err != nil {
		t.Fatal(err)
	}
	h.requireState(inst.ID, store.InstanceActive)
	if open := h.tasks(inst.ID); len(open) != 2 {
		t.Fatalf("after 1 completion: %d open tasks, want 2", len(open))
	}

	if err := h.e.CompleteTask(tasks[1].ID, nil, tasks[1].Assignee); err != nil {
		t.Fatal(err)
	}
	h.requireState(inst.ID, store.InstanceActive)

	if err := h.e.CompleteTask(tasks[2].ID, nil, tasks[2].Assignee); err != nil {
		t.Fatal(err)
	}
	h.requireState(inst.ID, store.InstanceCompleted)
}

// TestParallelJoinInsideMIIterations verifies gateway joins do not merge
// tokens across concurrent iterations of a multi-instance sub-process.
func TestParallelJoinInsideMIIterations(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="sub"/>
    <subProcess id="sub">
      <multiInstanceLoopCharacteristics isSequential="false"
        fliable:collection="items" fliable:elementVariable="item"/>
      <startEvent id="ss"/>
      <sequenceFlow id="sf1" sourceRef="ss" targetRef="fork"/>
      <parallelGateway id="fork"/>
      <sequenceFlow id="sf2" sourceRef="fork" targetRef="a"/>
      <sequenceFlow id="sf3" sourceRef="fork" targetRef="b"/>
      <userTask id="a" fliable:assignee="${'a-' + item}"/>
      <userTask id="b" fliable:assignee="${'b-' + item}"/>
      <sequenceFlow id="sf4" sourceRef="a" targetRef="join"/>
      <sequenceFlow id="sf5" sourceRef="b" targetRef="join"/>
      <parallelGateway id="join"/>
      <sequenceFlow id="sf6" sourceRef="join" targetRef="se"/>
      <endEvent id="se"/>
    </subProcess>
    <sequenceFlow id="f2" sourceRef="sub" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", map[string]any{"items": []any{"x", "y"}})
	// 2 iterations x 2 branches = 4 tasks.
	if got := len(h.tasks(inst.ID)); got != 4 {
		t.Fatalf("tasks = %d, want 4", got)
	}

	// Complete a-x and b-y: two DIFFERENT iterations each have one arrived
	// branch. The joins must not fire across iterations.
	complete := func(assignee string) {
		t.Helper()
		ts, _ := h.e.ListTasks(store.TaskFilter{InstanceID: inst.ID, Assignee: assignee, State: store.TaskCreated})
		if len(ts) != 1 {
			t.Fatalf("task for %s: %d", assignee, len(ts))
		}
		if err := h.e.CompleteTask(ts[0].ID, nil, assignee); err != nil {
			t.Fatal(err)
		}
	}
	complete("a-x")
	complete("b-y")
	h.requireState(inst.ID, store.InstanceActive)
	if got := len(h.tasks(inst.ID)); got != 2 {
		t.Fatalf("after cross-iteration completes: %d open, want 2 (joins must not fire)", got)
	}

	// Finish iteration x: its join fires, iteration y still open.
	complete("b-x")
	h.requireState(inst.ID, store.InstanceActive)
	if got := len(h.tasks(inst.ID)); got != 1 {
		t.Fatalf("iteration x done: %d open, want 1", got)
	}
	complete("a-y")
	h.requireState(inst.ID, store.InstanceCompleted)
}

// TestEventSubprocessNonInterruptingMessage covers repeatable event
// sub-process triggers while the main flow continues.
func TestEventSubprocessNonInterruptingMessage(t *testing.T) {
	h := newHarness(t)
	notes := 0
	h.e.RegisterHandler("note", func(ctx Context) (map[string]any, error) {
		notes++
		return nil, nil
	})
	h.deploy(procXML(`
  <message id="m1" name="addNote"/>
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="work"/>
    <userTask id="work"/>
    <sequenceFlow id="f2" sourceRef="work" targetRef="e"/>
    <endEvent id="e"/>
    <subProcess id="notes" triggeredByEvent="true">
      <startEvent id="ns" isInterrupting="false">
        <messageEventDefinition messageRef="m1"/>
      </startEvent>
      <sequenceFlow id="nf1" sourceRef="ns" targetRef="record"/>
      <serviceTask id="record" fliable:type="note"/>
      <sequenceFlow id="nf2" sourceRef="record" targetRef="ne"/>
      <endEvent id="ne"/>
    </subProcess>
  </process>`))

	inst := h.start("p", nil)
	for i := 0; i < 3; i++ {
		if n, err := h.e.CorrelateMessage("addNote", "", nil); err != nil || n != 1 {
			t.Fatalf("correlate #%d = %d, %v", i, n, err)
		}
	}
	if notes != 3 {
		t.Errorf("notes = %d, want 3 (non-interrupting is repeatable)", notes)
	}
	// Main flow unaffected.
	h.requireState(inst.ID, store.InstanceActive)
	h.completeOnly(inst.ID, nil)
	h.requireState(inst.ID, store.InstanceCompleted)
}

// TestBoundaryTimerOnSubProcess: interrupting boundary on a sub-process
// kills everything inside it.
func TestBoundaryTimerOnSubProcess(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="sub"/>
    <subProcess id="sub">
      <startEvent id="ss"/>
      <sequenceFlow id="sf1" sourceRef="ss" targetRef="inner"/>
      <userTask id="inner"/>
      <sequenceFlow id="sf2" sourceRef="inner" targetRef="se"/>
      <endEvent id="se"/>
    </subProcess>
    <boundaryEvent id="slaBreach" attachedToRef="sub">
      <timerEventDefinition><timeDuration>PT4H</timeDuration></timerEventDefinition>
    </boundaryEvent>
    <sequenceFlow id="f2" sourceRef="sub" targetRef="ok"/>
    <sequenceFlow id="f3" sourceRef="slaBreach" targetRef="escalate"/>
    <endEvent id="ok"/>
    <endEvent id="escalate"/>
  </process>`))

	inst := h.start("p", nil)
	if len(h.tasks(inst.ID)) != 1 {
		t.Fatal("inner task missing")
	}
	h.advance(5 * 60 * 60 * 1e9) // 5h
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.EndElement != "escalate" {
		t.Errorf("endElement = %q", done.EndElement)
	}
	if open := h.tasks(inst.ID); len(open) != 0 {
		t.Errorf("inner task survived scope cancellation: %d", len(open))
	}
}
