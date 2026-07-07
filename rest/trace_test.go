package rest

import (
	"net/http"
	"strings"
	"testing"

	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
)

func TestTraceContextPropagation(t *testing.T) {
	e := engine.New(store.NewMemory())
	s := New(e)
	srv := newTestServer(t, s)
	c := &client{t: t, srv: srv}
	c.do("POST", "/v1/definitions", `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="t">
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="w"/>
    <serviceTask id="w" fliable:topic="work"/>
    <sequenceFlow id="f2" sourceRef="w" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`, http.StatusCreated)

	// An incoming traceparent is echoed and carried into the instance.
	tp := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	req, _ := http.NewRequest("POST", srv.URL+"/v1/instances", strings.NewReader(`{"definitionKey":"p"}`))
	req.Header.Set("traceparent", tp)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("X-Trace-Id"); got != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("X-Trace-Id = %q", got)
	}
	resp.Body.Close()

	// The external worker receives the traceparent as a variable so it can
	// continue the same trace.
	fetched := c.doListPost("/v1/external-tasks/fetch", map[string]any{"topic": "work", "workerId": "w1"}, http.StatusOK)
	if len(fetched) != 1 {
		t.Fatalf("fetched %d", len(fetched))
	}
	vars := fetched[0]["variables"].(map[string]any)
	if vars["__traceparent"] != tp {
		t.Errorf("worker did not receive traceparent: %v", vars["__traceparent"])
	}

	// A request without a traceparent still gets a generated trace id.
	req2, _ := http.NewRequest("GET", srv.URL+"/healthz", nil)
	resp2, _ := http.DefaultClient.Do(req2)
	if len(resp2.Header.Get("X-Trace-Id")) != 32 {
		t.Errorf("generated trace id malformed: %q", resp2.Header.Get("X-Trace-Id"))
	}
	resp2.Body.Close()
}
