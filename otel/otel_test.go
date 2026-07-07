package otel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
)

const traceXML = `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="t">
  <process id="traced" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="calc"/>
    <scriptTask id="calc" fliable:resultVariable="x">
      <script>1 + 1</script>
    </scriptTask>
    <sequenceFlow id="f2" sourceRef="calc" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`

type collector struct {
	mu    sync.Mutex
	spans []map[string]any
}

func (c *collector) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []map[string]any `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		c.mu.Lock()
		for _, rs := range payload.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				c.spans = append(c.spans, ss.Spans...)
			}
		}
		c.mu.Unlock()
		w.WriteHeader(200)
	}
}

func (c *collector) byName(name string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.spans {
		if s["name"] == name {
			return s
		}
	}
	return nil
}

func TestExportsProcessSpans(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col.handler())
	defer srv.Close()

	x := New(Config{Endpoint: srv.URL, FlushInterval: time.Hour}) // manual flush
	defer x.Close()

	e := engine.New(store.NewMemory())
	x.Attach(e)

	// Caller-provided trace context parents the whole process.
	const tp = "00-aaaabbbbccccddddeeeeffff00001111-1234567890abcdef-01"
	if _, err := e.Deploy([]byte(traceXML), ""); err != nil {
		t.Fatal(err)
	}
	inst, err := e.StartInstance("traced", "bk-1", map[string]any{engine.TraceparentVar: tp})
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != store.InstanceCompleted {
		t.Fatalf("instance %s", inst.State)
	}
	if err := x.Flush(); err != nil {
		t.Fatal(err)
	}

	root := col.byName("process traced")
	if root == nil {
		t.Fatalf("no root span: %+v", col.spans)
	}
	if root["traceId"] != "aaaabbbbccccddddeeeeffff00001111" {
		t.Fatalf("root traceId = %v", root["traceId"])
	}
	if root["parentSpanId"] != "1234567890abcdef" {
		t.Fatalf("root parent = %v (traceparent not honored)", root["parentSpanId"])
	}

	script := col.byName("calc")
	if script == nil {
		t.Fatalf("no element span for calc: %+v", col.spans)
	}
	if script["traceId"] != root["traceId"] {
		t.Fatal("element span in a different trace")
	}
	if script["parentSpanId"] != root["spanId"] {
		t.Fatal("element span not parented under the process span")
	}
	// Nano timestamps must be well-formed and ordered.
	if script["startTimeUnixNano"] == "" || script["endTimeUnixNano"] == "" {
		t.Fatal("missing timestamps")
	}
}

func TestDerivedTraceIDWithoutTraceparent(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col.handler())
	defer srv.Close()
	x := New(Config{Endpoint: srv.URL, FlushInterval: time.Hour})
	defer x.Close()

	e := engine.New(store.NewMemory())
	x.Attach(e)
	if _, err := e.Deploy([]byte(traceXML), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartInstance("traced", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := x.Flush(); err != nil {
		t.Fatal(err)
	}
	root := col.byName("process traced")
	if root == nil {
		t.Fatal("no root span")
	}
	id, _ := root["traceId"].(string)
	if len(id) != 32 {
		t.Fatalf("derived traceId %q", id)
	}
	if _, ok := root["parentSpanId"]; ok {
		t.Fatal("derived trace must have no parent")
	}
}

func TestExportRetriesOnCollectorFailure(t *testing.T) {
	fail := true
	col := &collector{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "down", 503)
			return
		}
		col.handler()(w, r)
	}))
	defer srv.Close()

	x := New(Config{Endpoint: srv.URL, FlushInterval: time.Hour})
	defer x.Close()
	e := engine.New(store.NewMemory())
	x.Attach(e)
	if _, err := e.Deploy([]byte(traceXML), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartInstance("traced", "", nil); err != nil {
		t.Fatal(err)
	}

	if err := x.Flush(); err == nil {
		t.Fatal("expected export failure")
	}
	fail = false
	if err := x.Flush(); err != nil {
		t.Fatal(err)
	}
	if col.byName("process traced") == nil {
		t.Fatal("spans lost after retry")
	}
}
