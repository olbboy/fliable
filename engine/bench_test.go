package engine

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/olbboy/fliable/store"
)

const benchXML = `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="bench">
  <process id="bench" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="calc"/>
    <scriptTask id="calc" fliable:resultVariable="total"><script>amount * 1.2</script></scriptTask>
    <sequenceFlow id="f2" sourceRef="calc" targetRef="gw"/>
    <exclusiveGateway id="gw" default="fLow"/>
    <sequenceFlow id="fHigh" sourceRef="gw" targetRef="svc">
      <conditionExpression>total &gt; 100</conditionExpression>
    </sequenceFlow>
    <sequenceFlow id="fLow" sourceRef="gw" targetRef="e"/>
    <serviceTask id="svc" fliable:type="noop"/>
    <sequenceFlow id="f3" sourceRef="svc" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`

// BenchmarkProcessExecution measures full instance lifecycles per second
// on the in-memory store: start -> script -> gateway -> service -> end,
// including history writes.
func BenchmarkProcessExecution(b *testing.B) {
	e := New(store.NewMemory())
	e.RegisterHandler("noop", func(ctx Context) (map[string]any, error) { return nil, nil })
	if _, err := e.Deploy([]byte(benchXML), ""); err != nil {
		b.Fatal(err)
	}
	vars := map[string]any{"amount": 100.0}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := e.StartInstance("bench", "", vars); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProcessExecutionParallel exercises the striped instance locks.
func BenchmarkProcessExecutionParallel(b *testing.B) {
	e := New(store.NewMemory())
	e.RegisterHandler("noop", func(ctx Context) (map[string]any, error) { return nil, nil })
	if _, err := e.Deploy([]byte(benchXML), ""); err != nil {
		b.Fatal(err)
	}
	vars := map[string]any{"amount": 100.0}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := e.StartInstance("bench", "", vars); err != nil {
				b.Fatal(err)
			}
		}
	})
}

const benchAgentXML = `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="bench">
  <process id="benchAgent" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="agent"/>
    <serviceTask id="agent" fliable:agent="fast">
      <extensionElements>
        <fliable:agent>
          <fliable:prompt>Classify: ${ticket}</fliable:prompt>
          <fliable:tool name="lookup" mcpServer="crm"/>
        </fliable:agent>
      </extensionElements>
    </serviceTask>
    <sequenceFlow id="f2" sourceRef="agent" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`

// BenchmarkAgentTask measures the engine-side overhead of one governed
// agent invocation (prompt interpolation, governance events, usage
// charging, guard review) with a zero-latency invoker — i.e. everything
// except the model call itself.
func BenchmarkAgentTask(b *testing.B) {
	e := New(store.NewMemory())
	e.agentGuard = &AgentGuard{
		MaxTokensPerInstance: 1 << 30,
		Review:               func(AgentReview) error { return nil },
	}
	e.RegisterAgent("fast", AgentInvokerFunc(func(req AgentRequest) (AgentResponse, error) {
		return AgentResponse{
			Output: map[string]any{"category": "billing"},
			Usage:  AgentUsage{InputTokens: 100, OutputTokens: 20},
		}, nil
	}))
	if _, err := e.Deploy([]byte(benchAgentXML), ""); err != nil {
		b.Fatal(err)
	}
	vars := map[string]any{"ticket": "double charge"}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := e.StartInstance("benchAgent", "", vars); err != nil {
			b.Fatal(err)
		}
	}
}

// TestConcurrentExecution hammers the engine from many goroutines to
// surface races (run with -race).
func TestConcurrentExecution(t *testing.T) {
	e := New(store.NewMemory())
	e.RegisterHandler("noop", func(ctx Context) (map[string]any, error) {
		return map[string]any{"handled": true}, nil
	})
	if _, err := e.Deploy([]byte(benchXML), ""); err != nil {
		t.Fatal(err)
	}

	const workers, perWorker = 8, 50
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				inst, err := e.StartInstance("bench", fmt.Sprintf("bk-%d-%d", w, i), map[string]any{"amount": float64(i * 10)})
				if err != nil {
					errs <- err
					return
				}
				if inst.State != store.InstanceCompleted {
					errs <- fmt.Errorf("instance %s not completed: %s", inst.ID, inst.State)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	m := e.Metrics()
	if m.InstancesCompleted != workers*perWorker {
		t.Errorf("completed = %d, want %d", m.InstancesCompleted, workers*perWorker)
	}
}

// TestConcurrentUserTaskCompletion races task completion against
// cancellation and duplicate completes.
func TestConcurrentTaskCompletes(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="u"/>
    <userTask id="u"/>
    <sequenceFlow id="f2" sourceRef="u" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	for i := 0; i < 20; i++ {
		inst := h.start("p", nil)
		task := h.tasks(inst.ID)[0]
		var wg sync.WaitGroup
		successes := make(chan struct{}, 4)
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := h.e.CompleteTask(task.ID, nil, "racer"); err == nil {
					successes <- struct{}{}
				}
			}()
		}
		wg.Wait()
		close(successes)
		n := 0
		for range successes {
			n++
		}
		if n != 1 {
			t.Fatalf("task completed %d times, want exactly 1", n)
		}
		h.requireState(inst.ID, store.InstanceCompleted)
	}
}

// TestSchedulerLifecycle exercises Start/Stop with the real clock.
func TestSchedulerLifecycle(t *testing.T) {
	e := New(store.NewMemory(), WithJobPollInterval(5*time.Millisecond))
	if _, err := e.Deploy([]byte(`<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="w"/>
    <intermediateCatchEvent id="w">
      <timerEventDefinition><timeDuration>PT0.01S</timeDuration></timerEventDefinition>
    </intermediateCatchEvent>
    <sequenceFlow id="f2" sourceRef="w" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`), ""); err != nil {
		t.Fatal(err)
	}
	e.Start()
	defer e.Stop()
	inst, err := e.StartInstance("p", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := e.GetInstance(inst.ID)
		if got.State == store.InstanceCompleted {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timer never fired under real scheduler")
}
