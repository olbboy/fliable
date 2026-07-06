package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/olbboy/fliable/store"
)

// harness wraps an engine with a virtual clock so timer tests are
// deterministic and instant.
type harness struct {
	t   *testing.T
	e   *Engine
	now time.Time
	st  store.Store
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, now: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC), st: store.NewMemory()}
	h.e = New(h.st, WithClock(func() time.Time { return h.now }))
	return h
}

// advance moves the clock and drains due jobs until quiet.
func (h *harness) advance(d time.Duration) {
	h.now = h.now.Add(d)
	for i := 0; i < 20; i++ {
		if h.e.RunDueJobs(100) == 0 {
			return
		}
	}
	h.t.Fatal("jobs kept firing after 20 rounds")
}

func (h *harness) deploy(xml string) *store.Definition {
	h.t.Helper()
	def, err := h.e.Deploy([]byte(xml), "")
	if err != nil {
		h.t.Fatalf("Deploy: %v", err)
	}
	return def
}

func (h *harness) start(key string, vars map[string]any) *store.Instance {
	h.t.Helper()
	inst, err := h.e.StartInstance(key, "", vars)
	if err != nil {
		h.t.Fatalf("StartInstance(%s): %v", key, err)
	}
	return inst
}

func (h *harness) instance(id string) *store.Instance {
	h.t.Helper()
	inst, err := h.e.GetInstance(id)
	if err != nil {
		h.t.Fatalf("GetInstance: %v", err)
	}
	return inst
}

func (h *harness) requireState(id string, want store.InstanceState) *store.Instance {
	h.t.Helper()
	inst := h.instance(id)
	if inst.State != want {
		h.t.Fatalf("instance state = %s, want %s (tokens: %v)", inst.State, want, tokenDump(inst))
	}
	return inst
}

func (h *harness) tasks(instID string) []*store.Task {
	h.t.Helper()
	ts, err := h.e.ListTasks(store.TaskFilter{InstanceID: instID, State: store.TaskCreated})
	if err != nil {
		h.t.Fatal(err)
	}
	return ts
}

func (h *harness) completeOnly(instID string, vars map[string]any) {
	h.t.Helper()
	ts := h.tasks(instID)
	if len(ts) != 1 {
		h.t.Fatalf("want exactly 1 open task, got %d", len(ts))
	}
	if err := h.e.CompleteTask(ts[0].ID, vars, "tester"); err != nil {
		h.t.Fatalf("CompleteTask: %v", err)
	}
}

func tokenDump(inst *store.Instance) string {
	var b strings.Builder
	for _, t := range inst.Tokens {
		fmt.Fprintf(&b, "[%s@%s %s] ", t.ID[len(t.ID)-4:], t.ElementID, t.State)
	}
	return b.String()
}

func procXML(body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
             xmlns:fliable="https://fliable.dev/schema/1.0"
             targetNamespace="https://fliable.dev/tests">` + body + `</definitions>`
}

// ---- basics ---------------------------------------------------------------------

func TestSequentialFlowCompletes(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p1" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="calc"/>
    <scriptTask id="calc" fliable:resultVariable="total">
      <script>price * qty</script>
    </scriptTask>
    <sequenceFlow id="f2" sourceRef="calc" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p1", map[string]any{"price": 10, "qty": 4})
	inst = h.requireState(inst.ID, store.InstanceCompleted)
	if inst.Variables["total"] != 40.0 {
		t.Errorf("total = %v", inst.Variables["total"])
	}
	if inst.EndElement != "e" {
		t.Errorf("endElement = %q", inst.EndElement)
	}
	if len(inst.Tokens) != 0 {
		t.Errorf("leftover tokens: %v", tokenDump(inst))
	}

	evs, _ := h.e.History(inst.ID, store.HistoryFilter{})
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	joined := strings.Join(types, ",")
	for _, want := range []string{store.HistInstanceStarted, store.HistElementActivated, store.HistElementCompleted, store.HistInstanceCompleted} {
		if !strings.Contains(joined, want) {
			t.Errorf("history missing %s: %v", want, types)
		}
	}
}

func TestExclusiveGateway(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="gw"/>
    <exclusiveGateway id="gw" default="fLow"/>
    <sequenceFlow id="fHigh" sourceRef="gw" targetRef="high">
      <conditionExpression>amount &gt;= 1000</conditionExpression>
    </sequenceFlow>
    <sequenceFlow id="fLow" sourceRef="gw" targetRef="low"/>
    <scriptTask id="high" fliable:resultVariable="route"><script>'high'</script></scriptTask>
    <scriptTask id="low" fliable:resultVariable="route"><script>'low'</script></scriptTask>
    <sequenceFlow id="f2" sourceRef="high" targetRef="e"/>
    <sequenceFlow id="f3" sourceRef="low" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", map[string]any{"amount": 2500})
	if got := h.requireState(inst.ID, store.InstanceCompleted).Variables["route"]; got != "high" {
		t.Errorf("route = %v, want high", got)
	}
	inst = h.start("p", map[string]any{"amount": 10})
	if got := h.requireState(inst.ID, store.InstanceCompleted).Variables["route"]; got != "low" {
		t.Errorf("route = %v, want low", got)
	}
}

func TestParallelGatewayForkJoin(t *testing.T) {
	h := newHarness(t)
	calls := map[string]int{}
	h.e.RegisterHandler("count", func(ctx Context) (map[string]any, error) {
		calls[ctx.ElementID]++
		return nil, nil
	})
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="fork"/>
    <parallelGateway id="fork"/>
    <sequenceFlow id="f2" sourceRef="fork" targetRef="a"/>
    <sequenceFlow id="f3" sourceRef="fork" targetRef="b"/>
    <sequenceFlow id="f4" sourceRef="fork" targetRef="c"/>
    <serviceTask id="a" fliable:type="count"/>
    <serviceTask id="b" fliable:type="count"/>
    <serviceTask id="c" fliable:type="count"/>
    <sequenceFlow id="f5" sourceRef="a" targetRef="join"/>
    <sequenceFlow id="f6" sourceRef="b" targetRef="join"/>
    <sequenceFlow id="f7" sourceRef="c" targetRef="join"/>
    <parallelGateway id="join"/>
    <sequenceFlow id="f8" sourceRef="join" targetRef="done"/>
    <serviceTask id="done" fliable:type="count"/>
    <sequenceFlow id="f9" sourceRef="done" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", nil)
	h.requireState(inst.ID, store.InstanceCompleted)
	if calls["a"] != 1 || calls["b"] != 1 || calls["c"] != 1 {
		t.Errorf("branch calls = %v", calls)
	}
	if calls["done"] != 1 {
		t.Errorf("join fired %d times, want 1", calls["done"])
	}
}

func TestInclusiveGateway(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="fork"/>
    <inclusiveGateway id="fork" default="fC"/>
    <sequenceFlow id="fA" sourceRef="fork" targetRef="a">
      <conditionExpression>wantA</conditionExpression>
    </sequenceFlow>
    <sequenceFlow id="fB" sourceRef="fork" targetRef="b">
      <conditionExpression>wantB</conditionExpression>
    </sequenceFlow>
    <sequenceFlow id="fC" sourceRef="fork" targetRef="c"/>
    <scriptTask id="a" fliable:resultVariable="ranA"><script>true</script></scriptTask>
    <scriptTask id="b" fliable:resultVariable="ranB"><script>true</script></scriptTask>
    <scriptTask id="c" fliable:resultVariable="ranC"><script>true</script></scriptTask>
    <sequenceFlow id="f2" sourceRef="a" targetRef="join"/>
    <sequenceFlow id="f3" sourceRef="b" targetRef="join"/>
    <sequenceFlow id="f4" sourceRef="c" targetRef="join"/>
    <inclusiveGateway id="join"/>
    <sequenceFlow id="f5" sourceRef="join" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", map[string]any{"wantA": true, "wantB": true})
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.Variables["ranA"] != true || done.Variables["ranB"] != true || done.Variables["ranC"] != nil {
		t.Errorf("vars = %v", done.Variables)
	}

	// Nothing matches: default branch only.
	inst = h.start("p", nil)
	done = h.requireState(inst.ID, store.InstanceCompleted)
	if done.Variables["ranC"] != true || done.Variables["ranA"] != nil {
		t.Errorf("default branch vars = %v", done.Variables)
	}
}

// ---- user tasks -------------------------------------------------------------------

func TestUserTaskLifecycle(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="approve"/>
    <userTask id="approve" name="Approve order" fliable:assignee="${manager}"
              fliable:candidateGroups="sales" fliable:formKey="forms/approve"
              fliable:dueDate="PT48H" fliable:priority="50"/>
    <sequenceFlow id="f2" sourceRef="approve" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", map[string]any{"manager": "alice"})
	h.requireState(inst.ID, store.InstanceActive)

	ts := h.tasks(inst.ID)
	if len(ts) != 1 {
		t.Fatalf("tasks = %d", len(ts))
	}
	task := ts[0]
	if task.Assignee != "alice" || task.Name != "Approve order" || task.FormKey != "forms/approve" {
		t.Errorf("task = %+v", task)
	}
	if task.Priority != 50 {
		t.Errorf("priority = %d", task.Priority)
	}
	if want := h.now.Add(48 * time.Hour); !task.DueAt.Equal(want) {
		t.Errorf("dueAt = %v, want %v", task.DueAt, want)
	}

	// Candidate group query.
	byGroup, _ := h.e.ListTasks(store.TaskFilter{CandidateGroup: "sales"})
	if len(byGroup) != 1 {
		t.Errorf("by group = %d", len(byGroup))
	}

	if err := h.e.CompleteTask(task.ID, map[string]any{"approved": true}, "alice"); err != nil {
		t.Fatal(err)
	}
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.Variables["approved"] != true {
		t.Errorf("vars = %v", done.Variables)
	}
	// Double complete must fail.
	if err := h.e.CompleteTask(task.ID, nil, "alice"); err == nil {
		t.Error("second complete should fail")
	}
}

// ---- service tasks -------------------------------------------------------------------

func TestServiceTaskRetryThenIncident(t *testing.T) {
	h := newHarness(t)
	attempts := 0
	h.e.RegisterHandler("flaky", func(ctx Context) (map[string]any, error) {
		attempts++
		return nil, errors.New("connection refused")
	})
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="svc"/>
    <serviceTask id="svc" fliable:type="flaky" fliable:retries="3"/>
    <sequenceFlow id="f2" sourceRef="svc" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", nil)
	h.requireState(inst.ID, store.InstanceActive)
	if attempts != 1 {
		t.Fatalf("attempts = %d", attempts)
	}
	h.advance(6 * time.Second) // first retry backoff 5s
	if attempts != 2 {
		t.Fatalf("attempts after 1st retry = %d", attempts)
	}
	h.advance(11 * time.Second) // second backoff 10s
	if attempts != 3 {
		t.Fatalf("attempts after 2nd retry = %d", attempts)
	}
	// Exhausted: incident, no more retries.
	h.advance(time.Hour)
	if attempts != 3 {
		t.Fatalf("attempts after exhaustion = %d", attempts)
	}
	f := false
	incs, _ := h.st.ListIncidents(store.IncidentFilter{InstanceID: inst.ID, Resolved: &f})
	if len(incs) != 1 {
		t.Fatalf("incidents = %d", len(incs))
	}

	// Fix the service and resolve: fresh attempts.
	h.e.RegisterHandler("flaky", func(ctx Context) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err := h.e.ResolveIncident(incs[0].ID); err != nil {
		t.Fatal(err)
	}
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.Variables["ok"] != true {
		t.Errorf("vars = %v", done.Variables)
	}
}

func TestBPMNErrorCaughtByBoundary(t *testing.T) {
	h := newHarness(t)
	h.e.RegisterHandler("charge", func(ctx Context) (map[string]any, error) {
		return nil, NewBPMNError("CARD_DECLINED", "insufficient funds")
	})
	h.deploy(procXML(`
  <error id="err1" name="CardDeclined" errorCode="CARD_DECLINED"/>
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="charge"/>
    <serviceTask id="charge" fliable:type="charge"/>
    <boundaryEvent id="declined" attachedToRef="charge">
      <errorEventDefinition errorRef="err1"/>
    </boundaryEvent>
    <sequenceFlow id="f2" sourceRef="charge" targetRef="ok"/>
    <sequenceFlow id="f3" sourceRef="declined" targetRef="fail"/>
    <endEvent id="ok"/>
    <endEvent id="fail"/>
  </process>`))

	inst := h.start("p", nil)
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.EndElement != "fail" {
		t.Errorf("endElement = %q, want fail", done.EndElement)
	}
}

func TestExpressionServiceTask(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="svc"/>
    <serviceTask id="svc" fliable:expression="${upper(name)}" fliable:resultVariable="shout"/>
    <sequenceFlow id="f2" sourceRef="svc" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))
	inst := h.start("p", map[string]any{"name": "ada"})
	if got := h.requireState(inst.ID, store.InstanceCompleted).Variables["shout"]; got != "ADA" {
		t.Errorf("shout = %v", got)
	}
}

// ---- timers -----------------------------------------------------------------------

func TestTimerIntermediateCatch(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="wait"/>
    <intermediateCatchEvent id="wait">
      <timerEventDefinition><timeDuration>PT30M</timeDuration></timerEventDefinition>
    </intermediateCatchEvent>
    <sequenceFlow id="f2" sourceRef="wait" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", nil)
	h.requireState(inst.ID, store.InstanceActive)
	h.advance(29 * time.Minute)
	h.requireState(inst.ID, store.InstanceActive)
	h.advance(2 * time.Minute)
	h.requireState(inst.ID, store.InstanceCompleted)
}

func TestInterruptingBoundaryTimerCancelsTask(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="review"/>
    <userTask id="review"/>
    <boundaryEvent id="timeout" attachedToRef="review">
      <timerEventDefinition><timeDuration>PT1H</timeDuration></timerEventDefinition>
    </boundaryEvent>
    <sequenceFlow id="f2" sourceRef="review" targetRef="done"/>
    <sequenceFlow id="f3" sourceRef="timeout" targetRef="escalated"/>
    <endEvent id="done"/>
    <endEvent id="escalated"/>
  </process>`))

	inst := h.start("p", nil)
	task := h.tasks(inst.ID)[0]
	h.advance(2 * time.Hour)

	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.EndElement != "escalated" {
		t.Errorf("endElement = %q", done.EndElement)
	}
	got, _ := h.st.GetTask(task.ID)
	if got.State != store.TaskCanceled {
		t.Errorf("task state = %s, want canceled", got.State)
	}
}

func TestNonInterruptingBoundaryTimerRepeats(t *testing.T) {
	h := newHarness(t)
	reminders := 0
	h.e.RegisterHandler("remind", func(ctx Context) (map[string]any, error) {
		reminders++
		return nil, nil
	})
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="review"/>
    <userTask id="review"/>
    <boundaryEvent id="nudge" attachedToRef="review" cancelActivity="false">
      <timerEventDefinition><timeCycle>R3/PT10M</timeCycle></timerEventDefinition>
    </boundaryEvent>
    <sequenceFlow id="f2" sourceRef="review" targetRef="e"/>
    <sequenceFlow id="f3" sourceRef="nudge" targetRef="send"/>
    <serviceTask id="send" fliable:type="remind"/>
    <sequenceFlow id="f4" sourceRef="send" targetRef="e2"/>
    <endEvent id="e2"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", nil)
	h.advance(35 * time.Minute)
	if reminders != 3 {
		t.Errorf("reminders = %d, want 3", reminders)
	}
	// Task still open; completing it finishes the instance.
	h.requireState(inst.ID, store.InstanceActive)
	h.completeOnly(inst.ID, nil)
	h.requireState(inst.ID, store.InstanceCompleted)
}

// ---- messages & signals ---------------------------------------------------------------

func TestMessageCatchAndCorrelation(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <message id="m1" name="paymentReceived"/>
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="wait"/>
    <intermediateCatchEvent id="wait">
      <messageEventDefinition messageRef="m1"/>
    </intermediateCatchEvent>
    <sequenceFlow id="f2" sourceRef="wait" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	i1, _ := h.e.StartInstance("p", "order-1", nil)
	i2, _ := h.e.StartInstance("p", "order-2", nil)

	n, err := h.e.CorrelateMessage("paymentReceived", "order-2", map[string]any{"amount": 99})
	if err != nil || n != 1 {
		t.Fatalf("correlate = %d, %v", n, err)
	}
	h.requireState(i1.ID, store.InstanceActive)
	done := h.requireState(i2.ID, store.InstanceCompleted)
	if done.Variables["amount"] != 99.0 {
		t.Errorf("vars = %v", done.Variables)
	}

	// Broadcast without key hits the remaining one.
	n, _ = h.e.CorrelateMessage("paymentReceived", "", nil)
	if n != 1 {
		t.Errorf("second correlate = %d", n)
	}
	h.requireState(i1.ID, store.InstanceCompleted)
}

func TestMessageStartEvent(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <message id="m1" name="newOrder"/>
  <process id="p" isExecutable="true">
    <startEvent id="s">
      <messageEventDefinition messageRef="m1"/>
    </startEvent>
    <sequenceFlow id="f1" sourceRef="s" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	n, err := h.e.CorrelateMessage("newOrder", "bk-7", map[string]any{"sku": "X"})
	if err != nil || n != 1 {
		t.Fatalf("correlate = %d, %v", n, err)
	}
	insts, _ := h.e.ListInstances(store.InstanceFilter{DefinitionKey: "p"})
	if len(insts) != 1 || insts[0].State != store.InstanceCompleted || insts[0].BusinessKey != "bk-7" {
		t.Fatalf("instances = %+v", insts)
	}
	if insts[0].Variables["sku"] != "X" {
		t.Errorf("vars = %v", insts[0].Variables)
	}
}

func TestSignalBroadcast(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <signal id="sig1" name="marketClosed"/>
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="wait"/>
    <intermediateCatchEvent id="wait">
      <signalEventDefinition signalRef="sig1"/>
    </intermediateCatchEvent>
    <sequenceFlow id="f2" sourceRef="wait" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	i1 := h.start("p", nil)
	i2 := h.start("p", nil)
	n, err := h.e.BroadcastSignal("marketClosed", nil)
	if err != nil || n != 2 {
		t.Fatalf("broadcast = %d, %v", n, err)
	}
	h.requireState(i1.ID, store.InstanceCompleted)
	h.requireState(i2.ID, store.InstanceCompleted)
}

func TestEventBasedGatewayRace(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <message id="m1" name="reply"/>
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="gw"/>
    <eventBasedGateway id="gw"/>
    <sequenceFlow id="f2" sourceRef="gw" targetRef="onMsg"/>
    <sequenceFlow id="f3" sourceRef="gw" targetRef="onTimeout"/>
    <intermediateCatchEvent id="onMsg">
      <messageEventDefinition messageRef="m1"/>
    </intermediateCatchEvent>
    <intermediateCatchEvent id="onTimeout">
      <timerEventDefinition><timeDuration>PT1H</timeDuration></timerEventDefinition>
    </intermediateCatchEvent>
    <sequenceFlow id="f4" sourceRef="onMsg" targetRef="ok"/>
    <sequenceFlow id="f5" sourceRef="onTimeout" targetRef="late"/>
    <endEvent id="ok"/>
    <endEvent id="late"/>
  </process>`))

	// Message arrives first.
	i1 := h.start("p", nil)
	if n, _ := h.e.CorrelateMessage("reply", "", nil); n != 1 {
		t.Fatal("message not delivered")
	}
	if done := h.requireState(i1.ID, store.InstanceCompleted); done.EndElement != "ok" {
		t.Errorf("endElement = %q", done.EndElement)
	}
	// Timer must be cancelled: advancing time fires nothing.
	jobs, _ := h.st.ListJobs(i1.ID)
	if len(jobs) != 0 {
		t.Errorf("leftover jobs: %v", jobs)
	}

	// Timeout wins on the second instance.
	i2 := h.start("p", nil)
	h.advance(2 * time.Hour)
	if done := h.requireState(i2.ID, store.InstanceCompleted); done.EndElement != "late" {
		t.Errorf("endElement = %q", done.EndElement)
	}
	// Message subscription cleaned up.
	subs, _ := h.st.ListSubscriptions(store.SubscriptionFilter{InstanceID: i2.ID})
	if len(subs) != 0 {
		t.Errorf("leftover subs: %v", subs)
	}
}

// ---- subprocess & call activity ----------------------------------------------------------

func TestSubProcessAndTerminate(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="sub"/>
    <subProcess id="sub">
      <startEvent id="ss"/>
      <sequenceFlow id="sf1" sourceRef="ss" targetRef="inner"/>
      <scriptTask id="inner" fliable:resultVariable="innerRan"><script>true</script></scriptTask>
      <sequenceFlow id="sf2" sourceRef="inner" targetRef="se"/>
      <endEvent id="se"/>
    </subProcess>
    <sequenceFlow id="f2" sourceRef="sub" targetRef="after"/>
    <scriptTask id="after" fliable:resultVariable="afterRan"><script>true</script></scriptTask>
    <sequenceFlow id="f3" sourceRef="after" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", nil)
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.Variables["innerRan"] != true || done.Variables["afterRan"] != true {
		t.Errorf("vars = %v", done.Variables)
	}
}

func TestTerminateEndEvent(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="fork"/>
    <parallelGateway id="fork"/>
    <sequenceFlow id="f2" sourceRef="fork" targetRef="work"/>
    <sequenceFlow id="f3" sourceRef="fork" targetRef="kill"/>
    <userTask id="work"/>
    <sequenceFlow id="f4" sourceRef="work" targetRef="e1"/>
    <endEvent id="e1"/>
    <endEvent id="kill"><terminateEventDefinition/></endEvent>
  </process>`))

	inst := h.start("p", nil)
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.EndElement != "kill" {
		t.Errorf("endElement = %q", done.EndElement)
	}
	// The parallel user task was cancelled.
	open := h.tasks(inst.ID)
	if len(open) != 0 {
		t.Errorf("open tasks after terminate: %d", len(open))
	}
}

func TestCallActivity(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="child" isExecutable="true">
    <startEvent id="cs"/>
    <sequenceFlow id="cf1" sourceRef="cs" targetRef="calc"/>
    <scriptTask id="calc" fliable:resultVariable="doubled"><script>n * 2</script></scriptTask>
    <sequenceFlow id="cf2" sourceRef="calc" targetRef="ce"/>
    <endEvent id="ce"/>
  </process>`))
	h.deploy(procXML(`
  <process id="parent" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="call"/>
    <callActivity id="call" calledElement="child">
      <extensionElements>
        <inputOutput>
          <inputParameter name="n">amount</inputParameter>
          <outputParameter name="result">doubled</outputParameter>
        </inputOutput>
      </extensionElements>
    </callActivity>
    <sequenceFlow id="f2" sourceRef="call" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("parent", map[string]any{"amount": 21})
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.Variables["result"] != 42.0 {
		t.Errorf("result = %v", done.Variables["result"])
	}
	children, _ := h.e.ListInstances(store.InstanceFilter{ParentID: inst.ID})
	if len(children) != 1 || children[0].State != store.InstanceCompleted {
		t.Errorf("children = %+v", children)
	}
}

// ---- multi-instance ------------------------------------------------------------------------

func TestMultiInstanceParallel(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="mi"/>
    <scriptTask id="mi" fliable:resultVariable="ignored">
      <multiInstanceLoopCharacteristics isSequential="false"
        fliable:collection="nums" fliable:elementVariable="n"
        fliable:outputCollection="squares" fliable:outputElement="${n * n}"/>
      <script>n</script>
    </scriptTask>
    <sequenceFlow id="f2" sourceRef="mi" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", map[string]any{"nums": []any{2, 3, 4}})
	done := h.requireState(inst.ID, store.InstanceCompleted)
	squares, ok := done.Variables["squares"].([]any)
	if !ok || len(squares) != 3 {
		t.Fatalf("squares = %v", done.Variables["squares"])
	}
	sum := 0.0
	for _, v := range squares {
		sum += v.(float64)
	}
	if sum != 4+9+16 {
		t.Errorf("squares = %v", squares)
	}
}

func TestMultiInstanceSequentialUserTasks(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="mi"/>
    <userTask id="mi" fliable:assignee="${approver}">
      <multiInstanceLoopCharacteristics isSequential="true"
        fliable:collection="approvers" fliable:elementVariable="approver"/>
    </userTask>
    <sequenceFlow id="f2" sourceRef="mi" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", map[string]any{"approvers": []any{"ann", "bob"}})
	ts := h.tasks(inst.ID)
	if len(ts) != 1 || ts[0].Assignee != "ann" {
		t.Fatalf("first task = %+v", ts)
	}
	h.completeOnly(inst.ID, nil)
	ts = h.tasks(inst.ID)
	if len(ts) != 1 || ts[0].Assignee != "bob" {
		t.Fatalf("second task = %+v", ts)
	}
	h.completeOnly(inst.ID, nil)
	h.requireState(inst.ID, store.InstanceCompleted)
}

func TestMultiInstanceCompletionCondition(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="mi"/>
    <userTask id="mi">
      <multiInstanceLoopCharacteristics isSequential="false" fliable:collection="voters" fliable:elementVariable="voter">
        <completionCondition>nrOfCompletedInstances &gt;= 2</completionCondition>
      </multiInstanceLoopCharacteristics>
    </userTask>
    <sequenceFlow id="f2" sourceRef="mi" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", map[string]any{"voters": []any{"a", "b", "c", "d"}})
	ts := h.tasks(inst.ID)
	if len(ts) != 4 {
		t.Fatalf("tasks = %d", len(ts))
	}
	_ = h.e.CompleteTask(ts[0].ID, nil, "a")
	h.requireState(inst.ID, store.InstanceActive)
	_ = h.e.CompleteTask(ts[1].ID, nil, "b")
	h.requireState(inst.ID, store.InstanceCompleted)
	// Remaining tasks were cancelled.
	if left := h.tasks(inst.ID); len(left) != 0 {
		t.Errorf("leftover tasks = %d", len(left))
	}
}

// ---- external tasks -----------------------------------------------------------------------------

func TestExternalWorkerFlow(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="ship"/>
    <serviceTask id="ship" fliable:topic="shipping"/>
    <sequenceFlow id="f2" sourceRef="ship" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", map[string]any{"orderId": "o-1"})
	tasks, err := h.e.FetchExternalTasks("shipping", "worker-1", time.Minute, 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("fetch = %v, %v", tasks, err)
	}
	if tasks[0].Variables["orderId"] != "o-1" {
		t.Errorf("task vars = %v", tasks[0].Variables)
	}
	// Wrong worker can't complete.
	if err := h.e.CompleteExternalTask(tasks[0].ID, "intruder", nil); err == nil {
		t.Error("wrong worker completed the task")
	}
	if err := h.e.CompleteExternalTask(tasks[0].ID, "worker-1", map[string]any{"trackingId": "T1"}); err != nil {
		t.Fatal(err)
	}
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.Variables["trackingId"] != "T1" {
		t.Errorf("vars = %v", done.Variables)
	}
}

func TestExternalWorkerFailWithErrorCode(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <error id="err1" errorCode="NO_STOCK" name="NoStock"/>
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="pick"/>
    <serviceTask id="pick" fliable:topic="warehouse"/>
    <boundaryEvent id="noStock" attachedToRef="pick">
      <errorEventDefinition errorRef="err1"/>
    </boundaryEvent>
    <sequenceFlow id="f2" sourceRef="pick" targetRef="ok"/>
    <sequenceFlow id="f3" sourceRef="noStock" targetRef="reorder"/>
    <endEvent id="ok"/>
    <endEvent id="reorder"/>
  </process>`))

	inst := h.start("p", nil)
	tasks, _ := h.e.FetchExternalTasks("warehouse", "w1", time.Minute, 1)
	if err := h.e.FailExternalTask(tasks[0].ID, "w1", "shelf empty", "NO_STOCK"); err != nil {
		t.Fatal(err)
	}
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.EndElement != "reorder" {
		t.Errorf("endElement = %q", done.EndElement)
	}
}

// ---- event subprocess & error propagation ----------------------------------------------------------

func TestEventSubprocessInterruptingOnError(t *testing.T) {
	h := newHarness(t)
	h.e.RegisterHandler("boom", func(ctx Context) (map[string]any, error) {
		return nil, NewBPMNError("FATAL", "kaput")
	})
	h.deploy(procXML(`
  <error id="err1" errorCode="FATAL" name="Fatal"/>
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="svc"/>
    <serviceTask id="svc" fliable:type="boom"/>
    <sequenceFlow id="f2" sourceRef="svc" targetRef="e"/>
    <endEvent id="e"/>
    <subProcess id="onError" triggeredByEvent="true">
      <startEvent id="es">
        <errorEventDefinition errorRef="err1"/>
      </startEvent>
      <sequenceFlow id="ef1" sourceRef="es" targetRef="cleanup"/>
      <scriptTask id="cleanup" fliable:resultVariable="cleaned"><script>true</script></scriptTask>
      <sequenceFlow id="ef2" sourceRef="cleanup" targetRef="ee"/>
      <endEvent id="ee"/>
    </subProcess>
  </process>`))

	inst := h.start("p", nil)
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.Variables["cleaned"] != true {
		t.Errorf("vars = %v", done.Variables)
	}
	if done.Variables["errorCode"] != "FATAL" {
		t.Errorf("errorCode var = %v", done.Variables["errorCode"])
	}
}

func TestUnhandledErrorTerminatesWithIncident(t *testing.T) {
	h := newHarness(t)
	h.e.RegisterHandler("boom", func(ctx Context) (map[string]any, error) {
		return nil, NewBPMNError("UNCAUGHT", "nobody catches this")
	})
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="svc"/>
    <serviceTask id="svc" fliable:type="boom"/>
    <sequenceFlow id="f2" sourceRef="svc" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", nil)
	h.requireState(inst.ID, store.InstanceTerminated)
	incs, _ := h.st.ListIncidents(store.IncidentFilter{InstanceID: inst.ID})
	if len(incs) != 1 || !strings.Contains(incs[0].Message, "UNCAUGHT") {
		t.Errorf("incidents = %+v", incs)
	}
}

func TestErrorEndEventInSubprocessCaughtByBoundary(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <error id="err1" errorCode="ESCALATE" name="Esc"/>
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="sub"/>
    <subProcess id="sub">
      <startEvent id="ss"/>
      <sequenceFlow id="sf1" sourceRef="ss" targetRef="innerEnd"/>
      <endEvent id="innerEnd">
        <errorEventDefinition errorRef="err1"/>
      </endEvent>
    </subProcess>
    <boundaryEvent id="caught" attachedToRef="sub">
      <errorEventDefinition errorRef="err1"/>
    </boundaryEvent>
    <sequenceFlow id="f2" sourceRef="sub" targetRef="normal"/>
    <sequenceFlow id="f3" sourceRef="caught" targetRef="handled"/>
    <endEvent id="normal"/>
    <endEvent id="handled"/>
  </process>`))

	inst := h.start("p", nil)
	done := h.requireState(inst.ID, store.InstanceCompleted)
	if done.EndElement != "handled" {
		t.Errorf("endElement = %q", done.EndElement)
	}
}

// ---- misc ------------------------------------------------------------------------------------------

func TestCancelInstance(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="wait"/>
    <userTask id="wait"/>
    <sequenceFlow id="f2" sourceRef="wait" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", nil)
	if err := h.e.CancelInstance(inst.ID, "user request"); err != nil {
		t.Fatal(err)
	}
	h.requireState(inst.ID, store.InstanceTerminated)
	if open := h.tasks(inst.ID); len(open) != 0 {
		t.Errorf("open tasks after cancel: %d", len(open))
	}
}

func TestDefinitionVersioning(t *testing.T) {
	h := newHarness(t)
	v1 := h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="wait"/>
    <userTask id="wait" name="v1 task"/>
    <sequenceFlow id="f2" sourceRef="wait" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))
	inst1 := h.start("p", nil)

	v2 := h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="wait"/>
    <userTask id="wait" name="v2 task"/>
    <sequenceFlow id="f2" sourceRef="wait" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))
	if v2.Version != v1.Version+1 {
		t.Errorf("versions: %d then %d", v1.Version, v2.Version)
	}
	inst2 := h.start("p", nil)

	// New instances use v2; in-flight instances stay on v1.
	if h.instance(inst1.ID).DefinitionID != v1.ID {
		t.Error("inst1 migrated unexpectedly")
	}
	if h.instance(inst2.ID).DefinitionID != v2.ID {
		t.Error("inst2 not on v2")
	}
	if h.tasks(inst2.ID)[0].Name != "v2 task" {
		t.Error("v2 task name wrong")
	}
	h.completeOnly(inst1.ID, nil)
	h.requireState(inst1.ID, store.InstanceCompleted)
}

func TestAsyncContinuation(t *testing.T) {
	h := newHarness(t)
	ran := false
	h.e.RegisterHandler("later", func(ctx Context) (map[string]any, error) {
		ran = true
		return nil, nil
	})
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="svc"/>
    <serviceTask id="svc" fliable:type="later" fliable:async="true"/>
    <sequenceFlow id="f2" sourceRef="svc" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", nil)
	// StartInstance returned before the service ran.
	if ran {
		t.Fatal("async task ran synchronously")
	}
	h.requireState(inst.ID, store.InstanceActive)
	h.advance(time.Second)
	if !ran {
		t.Fatal("async task never ran")
	}
	h.requireState(inst.ID, store.InstanceCompleted)
}

func TestTimerStartEvent(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="cron" isExecutable="true">
    <startEvent id="s">
      <timerEventDefinition><timeCycle>R2/PT1H</timeCycle></timerEventDefinition>
    </startEvent>
    <sequenceFlow id="f1" sourceRef="s" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	h.advance(61 * time.Minute)
	insts, _ := h.e.ListInstances(store.InstanceFilter{DefinitionKey: "cron"})
	if len(insts) != 1 {
		t.Fatalf("after 1h: %d instances", len(insts))
	}
	h.advance(time.Hour)
	insts, _ = h.e.ListInstances(store.InstanceFilter{DefinitionKey: "cron"})
	if len(insts) != 2 {
		t.Fatalf("after 2h: %d instances", len(insts))
	}
	// R2: no third run.
	h.advance(3 * time.Hour)
	insts, _ = h.e.ListInstances(store.InstanceFilter{DefinitionKey: "cron"})
	if len(insts) != 2 {
		t.Fatalf("after 5h: %d instances", len(insts))
	}
}
