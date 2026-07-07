package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/olbboy/fliable/store"
)

const migV1 = `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="t">
  <process id="onb" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="review"/>
    <userTask id="review" name="Review v1"/>
    <sequenceFlow id="f2" sourceRef="review" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`

// v2 renames the task and inserts a second human step after it.
const migV2 = `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="t">
  <process id="onb" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="triage"/>
    <userTask id="triage" name="Triage v2" fliable:formKey="triageForm"/>
    <sequenceFlow id="f2" sourceRef="triage" targetRef="signoff"/>
    <userTask id="signoff" name="Sign-off"/>
    <sequenceFlow id="f3" sourceRef="signoff" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`

func TestMigrateUserTaskWait(t *testing.T) {
	h := newHarness(t)
	h.deploy(migV1)
	inst := h.start("onb", map[string]any{"amount": 100.0})
	v2 := h.deploy(migV2)

	// Dry run first: valid plan, nothing applied.
	rep, err := h.e.MigrateInstance(inst.ID, MigrationPlan{
		TargetDefinitionID: v2.ID,
		ActivityMap:        map[string]string{"review": "triage"},
		DryRun:             true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Applied || len(rep.Issues) != 0 || len(rep.TokenMoves) != 1 {
		t.Fatalf("dry run: %+v", rep)
	}
	if got := h.instance(inst.ID); got.DefinitionID != inst.DefinitionID {
		t.Fatal("dry run mutated the instance")
	}

	// Apply for real, with a variable transform.
	rep, err = h.e.MigrateInstance(inst.ID, MigrationPlan{
		TargetDefinitionID: v2.ID,
		ActivityMap:        map[string]string{"review": "triage"},
		VarTransforms:      map[string]string{"tier": "amount > 50 ? 'high' : 'low'"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied {
		t.Fatalf("not applied: %v", rep.Issues)
	}

	got := h.instance(inst.ID)
	if got.DefinitionID != v2.ID {
		t.Fatalf("definition not switched: %s", got.DefinitionID)
	}
	if got.Variables["tier"] != "high" {
		t.Fatalf("var transform: %v", got.Variables["tier"])
	}

	// The open task moved onto the renamed element with refreshed metadata.
	tasks := h.tasks(inst.ID)
	if len(tasks) != 1 || tasks[0].ElementID != "triage" {
		t.Fatalf("task not remapped: %+v", tasks)
	}
	if tasks[0].Name != "Triage v2" || tasks[0].FormKey != "triageForm" {
		t.Fatalf("task metadata not refreshed: %+v", tasks[0])
	}

	// Completing it continues on the v2 path (the new sign-off step).
	h.completeOnly(inst.ID, nil)
	h.requireState(inst.ID, store.InstanceActive)
	tasks = h.tasks(inst.ID)
	if len(tasks) != 1 || tasks[0].ElementID != "signoff" {
		t.Fatalf("v2 continuation: %+v", tasks)
	}
	h.completeOnly(inst.ID, nil)
	h.requireState(inst.ID, store.InstanceCompleted)

	// The audit trail records the migration.
	evs, _ := h.e.History(inst.ID, store.HistoryFilter{Type: store.HistInstanceMigrated})
	if len(evs) != 1 {
		t.Fatalf("migration event missing")
	}
}

const migTimerV1 = `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <process id="tp" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="wait"/>
    <intermediateCatchEvent id="wait">
      <timerEventDefinition><timeDuration>PT1H</timeDuration></timerEventDefinition>
    </intermediateCatchEvent>
    <sequenceFlow id="f2" sourceRef="wait" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`

const migTimerV2 = `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <process id="tp" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="pause"/>
    <intermediateCatchEvent id="pause">
      <timerEventDefinition><timeDuration>PT1H</timeDuration></timerEventDefinition>
    </intermediateCatchEvent>
    <sequenceFlow id="f2" sourceRef="pause" targetRef="after"/>
    <userTask id="after" name="After timer"/>
    <sequenceFlow id="f3" sourceRef="after" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`

func TestMigrateTimerWaitKeepsDueTime(t *testing.T) {
	h := newHarness(t)
	h.deploy(migTimerV1)
	inst := h.start("tp", nil)
	v2 := h.deploy(migTimerV2)

	rep, err := h.e.MigrateInstance(inst.ID, MigrationPlan{
		TargetDefinitionID: v2.ID,
		ActivityMap:        map[string]string{"wait": "pause"},
	})
	if err != nil || !rep.Applied {
		t.Fatalf("migrate: %v %+v", err, rep)
	}

	// The original timer fires on schedule and execution continues on v2.
	h.advance(61 * time.Minute)
	tasks := h.tasks(inst.ID)
	if len(tasks) != 1 || tasks[0].ElementID != "after" {
		t.Fatalf("timer continuation on v2: %+v", tasks)
	}
}

func TestMigrateValidationRejects(t *testing.T) {
	h := newHarness(t)
	h.deploy(migV1)
	inst := h.start("onb", nil)
	v2 := h.deploy(migTimerV2) // wrong process entirely

	// Unmapped token: "review" doesn't exist in the timer process.
	rep, err := h.e.MigrateInstance(inst.ID, MigrationPlan{TargetDefinitionID: v2.ID})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Applied || len(rep.Issues) == 0 {
		t.Fatalf("expected issues: %+v", rep)
	}
	if !strings.Contains(rep.Issues[0], "does not exist in target") {
		t.Fatalf("issue: %v", rep.Issues)
	}
	// Nothing changed.
	if got := h.instance(inst.ID); got.DefinitionID != inst.DefinitionID {
		t.Fatal("failed validation mutated the instance")
	}

	// Type change: user task -> timer catch is rejected.
	rep, err = h.e.MigrateInstance(inst.ID, MigrationPlan{
		TargetDefinitionID: v2.ID,
		ActivityMap:        map[string]string{"review": "pause"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Applied || len(rep.Issues) == 0 {
		t.Fatalf("type change accepted: %+v", rep)
	}

	// Completed instances cannot migrate.
	h.completeOnly(inst.ID, nil)
	if _, err := h.e.MigrateInstance(inst.ID, MigrationPlan{TargetDefinitionID: v2.ID}); err == nil {
		t.Fatal("migrated a completed instance")
	}
}

const migMsgV1 = `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <message id="m1" name="paid"/>
  <process id="mp" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="waitPay"/>
    <intermediateCatchEvent id="waitPay">
      <messageEventDefinition messageRef="m1"/>
    </intermediateCatchEvent>
    <sequenceFlow id="f2" sourceRef="waitPay" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`

const migMsgV2 = `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <message id="m2" name="paymentSettled"/>
  <process id="mp" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="waitPay"/>
    <intermediateCatchEvent id="waitPay">
      <messageEventDefinition messageRef="m2"/>
    </intermediateCatchEvent>
    <sequenceFlow id="f2" sourceRef="waitPay" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`

func TestMigrateMessageWaitRefreshesName(t *testing.T) {
	h := newHarness(t)
	h.deploy(migMsgV1)
	inst := h.start("mp", nil)
	v2 := h.deploy(migMsgV2)

	rep, err := h.e.MigrateInstance(inst.ID, MigrationPlan{TargetDefinitionID: v2.ID})
	if err != nil || !rep.Applied {
		t.Fatalf("migrate: %v %+v", err, rep)
	}

	// The old message name no longer correlates...
	if n, _ := h.e.CorrelateMessage("paid", "", nil); n != 0 {
		t.Fatalf("old message name still correlates: %d", n)
	}
	// ...the renamed one does.
	n, err := h.e.CorrelateMessage("paymentSettled", "", nil)
	if err != nil || n != 1 {
		t.Fatalf("new message name: %d %v", n, err)
	}
	h.requireState(inst.ID, store.InstanceCompleted)
}
