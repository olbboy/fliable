package rest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
)

const webhookProcessXML = `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <message id="m1" name="orderReceived"/>
  <process id="orderFlow" isExecutable="true">
    <startEvent id="s">
      <messageEventDefinition messageRef="m1"/>
    </startEvent>
    <sequenceFlow id="f1" sourceRef="s" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`

func TestWebhookChannelEndToEnd(t *testing.T) {
	st := store.NewMemory()
	e := engine.New(st)
	srv := New(e, WithAPIKey("admin"))

	do := func(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	admin := map[string]string{"X-Api-Key": "admin"}

	if rec := do("POST", "/v1/definitions", webhookProcessXML, admin); rec.Code != http.StatusCreated {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}

	// Channel management requires auth.
	if rec := do("PUT", "/v1/webhooks/orders", `{"event":"orderReceived"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated config: %d", rec.Code)
	}
	rec := do("PUT", "/v1/webhooks/orders", `{
		"event": "orderReceived",
		"secret": "hook-secret",
		"dedupeExpr": "order.id",
		"vars": {"orderId": "order.id", "amount": "order.amount"}
	}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("configure: %d %s", rec.Code, rec.Body)
	}

	// Event without the channel secret is rejected.
	if rec := do("POST", "/v1/webhooks/orders", `{"order":{"id":"o-1","amount":42}}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no secret: %d", rec.Code)
	}

	// Valid event starts an instance via the message start event —
	// no platform API key needed, only the channel secret.
	hook := map[string]string{"X-Webhook-Secret": "hook-secret"}
	rec = do("POST", "/v1/webhooks/orders", `{"order":{"id":"o-1","amount":42}}`, hook)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"activated":1`) {
		t.Fatalf("event: %d %s", rec.Code, rec.Body)
	}

	// The same idempotency key is swallowed.
	rec = do("POST", "/v1/webhooks/orders", `{"order":{"id":"o-1","amount":42}}`, hook)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deduplicated":true`) {
		t.Fatalf("dedupe: %d %s", rec.Code, rec.Body)
	}

	// A new key delivers again.
	rec = do("POST", "/v1/webhooks/orders", `{"order":{"id":"o-2","amount":7}}`, hook)
	if !strings.Contains(rec.Body.String(), `"activated":1`) {
		t.Fatalf("second event: %d %s", rec.Code, rec.Body)
	}

	insts, _ := e.ListInstances(store.InstanceFilter{DefinitionKey: "orderFlow"})
	if len(insts) != 2 {
		t.Fatalf("instances: %d", len(insts))
	}
	// Variable mapping extracted fields from the payload.
	found := false
	for _, in := range insts {
		if in.Variables["orderId"] == "o-1" && in.Variables["amount"] == float64(42) {
			found = true
		}
	}
	if !found {
		t.Fatalf("vars not mapped: %+v", insts[0].Variables)
	}

	// Listing never echoes secrets.
	rec = do("GET", "/v1/webhooks", "", admin)
	if strings.Contains(rec.Body.String(), "hook-secret") {
		t.Fatal("secret leaked in listing")
	}

	// Unknown channel 404s.
	if rec := do("POST", "/v1/webhooks/nope", `{}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown channel: %d", rec.Code)
	}
}

func TestAnalyticsEndpoint(t *testing.T) {
	st := store.NewMemory()
	e := engine.New(st)
	srv := New(e)

	if _, err := e.Deploy([]byte(`<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="t">
  <process id="quick" isExecutable="true">
    <startEvent id="s"/><sequenceFlow id="f" sourceRef="s" targetRef="e"/><endEvent id="e"/>
  </process>
</definitions>`), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Deploy([]byte(`<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="t">
  <process id="waiting" isExecutable="true">
    <startEvent id="s"/><sequenceFlow id="f" sourceRef="s" targetRef="u"/>
    <userTask id="u" name="Wait"/>
    <sequenceFlow id="f2" sourceRef="u" targetRef="e"/><endEvent id="e"/>
  </process>
</definitions>`), ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.StartInstance("quick", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.StartInstance("waiting", "", nil); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v1/analytics/processes", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("analytics: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"definitionKey":"quick"`) || !strings.Contains(body, `"completed":3`) {
		t.Fatalf("quick stats missing: %s", body)
	}
	if !strings.Contains(body, `"definitionKey":"waiting"`) || !strings.Contains(body, `"openTasks":1`) {
		t.Fatalf("waiting stats missing: %s", body)
	}
}
