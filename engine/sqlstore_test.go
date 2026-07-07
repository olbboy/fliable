package engine

import (
	"testing"
	"time"

	"github.com/olbboy/fliable/store"
	"github.com/olbboy/fliable/store/sqltest"
)

// The full engine running on the SQL store: deploy, gateway branching,
// service handler, user task, timer — the same lifecycle the memory and
// journal stores run, through database/sql.
func TestEngineOnSQLStore(t *testing.T) {
	db, err := sqltest.OpenDB()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.NewSQL(db, store.SQLOptions{Dialect: store.DialectSQLite})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	e := New(st, WithClock(func() time.Time { return now }))
	handled := false
	e.RegisterHandler("notify", func(ctx Context) (map[string]any, error) {
		handled = true
		return map[string]any{"notified": true}, nil
	})

	if _, err := e.Deploy([]byte(`<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="t">
  <process id="sqlFlow" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="gw"/>
    <exclusiveGateway id="gw" default="fLow"/>
    <sequenceFlow id="fHigh" sourceRef="gw" targetRef="review">
      <conditionExpression>amount &gt; 500</conditionExpression>
    </sequenceFlow>
    <sequenceFlow id="fLow" sourceRef="gw" targetRef="wait"/>
    <userTask id="review" name="Manual review"/>
    <sequenceFlow id="f2" sourceRef="review" targetRef="notify"/>
    <intermediateCatchEvent id="wait">
      <timerEventDefinition><timeDuration>PT10M</timeDuration></timerEventDefinition>
    </intermediateCatchEvent>
    <sequenceFlow id="f3" sourceRef="wait" targetRef="notify"/>
    <serviceTask id="notify" fliable:type="notify"/>
    <sequenceFlow id="f4" sourceRef="notify" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`), ""); err != nil {
		t.Fatal(err)
	}

	// High-amount path: user task -> handler -> done.
	high, err := e.StartInstance("sqlFlow", "hi-1", map[string]any{"amount": 900.0})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := e.ListTasks(store.TaskFilter{InstanceID: high.ID, State: store.TaskCreated})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks: %v %d", err, len(tasks))
	}
	if err := e.CompleteTask(tasks[0].ID, map[string]any{"ok": true}, "alice"); err != nil {
		t.Fatal(err)
	}
	got, _ := e.GetInstance(high.ID)
	if got.State != store.InstanceCompleted || !handled {
		t.Fatalf("high path: %s handled=%v", got.State, handled)
	}
	if got.Variables["notified"] != true {
		t.Fatalf("handler output lost: %v", got.Variables)
	}

	// Low-amount path parks on the timer; advancing the clock fires it.
	low, err := e.StartInstance("sqlFlow", "lo-1", map[string]any{"amount": 10.0})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Minute)
	for i := 0; i < 5 && e.RunDueJobs(10) > 0; i++ {
	}
	got, _ = e.GetInstance(low.ID)
	if got.State != store.InstanceCompleted {
		t.Fatalf("low path: %s", got.State)
	}

	// History is complete and ordered on SQL too.
	evs, err := e.History(high.ID, store.HistoryFilter{})
	if err != nil || len(evs) == 0 {
		t.Fatalf("history: %v %d", err, len(evs))
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq <= evs[i-1].Seq {
			t.Fatalf("history out of order at %d", i)
		}
	}
}
