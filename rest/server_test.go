package rest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/olbboy/fliable/dmn"
	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
)

const approvalXML = `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="t">
  <process id="approval" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="review"/>
    <userTask id="review" name="Review" fliable:assignee="alice"/>
    <sequenceFlow id="f2" sourceRef="review" targetRef="ship"/>
    <serviceTask id="ship" fliable:topic="shipping"/>
    <sequenceFlow id="f3" sourceRef="ship" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`

type client struct {
	t      *testing.T
	srv    *httptest.Server
	key    string
	bearer string
}

// newTestServer starts an httptest server for a configured Server and
// registers cleanup.
func newTestServer(t *testing.T, s *Server) *httptest.Server {
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

// auth sets credentials on a request from the client's configured key or
// bearer token.
func (c *client) auth(req *http.Request) {
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
}

// doAuth issues a bodyless request and asserts the status.
func (c *client) doAuth(method, path string, want int) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.srv.URL+path, nil)
	c.auth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d, want %d", method, path, resp.StatusCode, want)
	}
}

// doAuthBody issues a request with a raw body and asserts the status.
func (c *client) doAuthBody(method, path, body string, want int) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.srv.URL+path, strings.NewReader(body))
	c.auth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d, want %d", method, path, resp.StatusCode, want)
	}
}

// doAuthPage issues a GET and returns the page envelope items.
func (c *client) doAuthPage(method, path string, want int) []map[string]any {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.srv.URL+path, nil)
	c.auth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d, want %d", method, path, resp.StatusCode, want)
	}
	var env struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return env.Items
}

func newClient(t *testing.T, opts ...Option) *client {
	e := engine.New(store.NewMemory())
	reg := dmn.NewRegistry()
	opts = append(opts, WithDecisions(reg))
	s := New(e, opts...)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

func (c *client) do(method, path string, body any, want int) map[string]any {
	c.t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case nil:
	case string:
		buf.WriteString(b)
	default:
		if err := json.NewEncoder(&buf).Encode(b); err != nil {
			c.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, c.srv.URL+path, &buf)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	dec := json.NewDecoder(resp.Body)
	_ = dec.Decode(&out)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d, want %d (body: %v)", method, path, resp.StatusCode, want, out)
	}
	return out
}

func (c *client) doList(method, path string, want int) []map[string]any {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.srv.URL+path, nil)
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d, want %d", method, path, resp.StatusCode, want)
	}
	var out []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// doPage decodes a paginated collection envelope ({items,count,nextCursor})
// and returns the items.
func (c *client) doPage(method, path string, want int) []map[string]any {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.srv.URL+path, nil)
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d, want %d", method, path, resp.StatusCode, want)
	}
	var env struct {
		Items      []map[string]any `json:"items"`
		Count      int              `json:"count"`
		NextCursor string           `json:"nextCursor"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return env.Items
}

func TestFullAPILifecycle(t *testing.T) {
	c := newClient(t)

	// Deploy.
	dep := c.do("POST", "/v1/definitions", approvalXML, http.StatusCreated)
	if dep["key"] != "approval" || dep["version"] != 1.0 {
		t.Fatalf("deploy = %v", dep)
	}

	// Bad deployments are rejected with details.
	c.do("POST", "/v1/definitions", "<not-bpmn/>", http.StatusUnprocessableEntity)

	// Start an instance.
	inst := c.do("POST", "/v1/instances", map[string]any{
		"definitionKey": "approval",
		"businessKey":   "ord-9",
		"variables":     map[string]any{"amount": 120},
	}, http.StatusCreated)
	instID := inst["id"].(string)
	if inst["state"] != "active" {
		t.Fatalf("instance = %v", inst)
	}

	// Task queue.
	tasks := c.doPage("GET", "/v1/tasks?assignee=alice", http.StatusOK)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %v", tasks)
	}
	taskID := tasks[0]["id"].(string)

	// Complete via API.
	c.do("POST", "/v1/tasks/"+taskID+"/complete", map[string]any{
		"user":      "alice",
		"variables": map[string]any{"approved": true},
	}, http.StatusOK)
	// Double-complete conflicts.
	c.do("POST", "/v1/tasks/"+taskID+"/complete", map[string]any{"user": "alice"}, http.StatusConflict)

	// External worker.
	fetched := c.doListPost("/v1/external-tasks/fetch", map[string]any{
		"topic": "shipping", "workerId": "w1",
	}, http.StatusOK)
	if len(fetched) != 1 {
		t.Fatalf("fetched = %v", fetched)
	}
	extID := fetched[0]["id"].(string)
	c.do("POST", "/v1/external-tasks/"+extID+"/complete", map[string]any{
		"workerId": "w1", "variables": map[string]any{"trackingId": "TRK"},
	}, http.StatusOK)

	// Instance is done, variables merged, history recorded.
	got := c.do("GET", "/v1/instances/"+instID, nil, http.StatusOK)
	if got["state"] != "completed" {
		t.Fatalf("final = %v", got)
	}
	vars := got["variables"].(map[string]any)
	if vars["approved"] != true || vars["trackingId"] != "TRK" {
		t.Errorf("vars = %v", vars)
	}
	hist := c.doList("GET", "/v1/instances/"+instID+"/history", http.StatusOK)
	if len(hist) < 5 {
		t.Errorf("history too short: %d", len(hist))
	}

	// Metrics endpoints.
	c.do("GET", "/v1/stats", nil, http.StatusOK)
	req, _ := http.NewRequest("GET", c.srv.URL+"/metrics", nil)
	resp, _ := http.DefaultClient.Do(req)
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if !strings.Contains(string(body[:n]), "fliable_instances_completed_total 1") {
		t.Errorf("metrics missing counter:\n%s", body[:n])
	}
}

func (c *client) doListPost(path string, body any, want int) []map[string]any {
	c.t.Helper()
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(body)
	req, _ := http.NewRequest("POST", c.srv.URL+path, &buf)
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		c.t.Fatalf("POST %s = %d, want %d", path, resp.StatusCode, want)
	}
	var out []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func TestAPIKeyAuth(t *testing.T) {
	c := newClient(t, WithAPIKey("sekret"))

	// Health is open.
	c.do("GET", "/healthz", nil, http.StatusOK)
	// Everything else requires the key.
	c.do("GET", "/v1/instances", nil, http.StatusUnauthorized)
	c.key = "sekret"
	c.doPage("GET", "/v1/instances", http.StatusOK)
}

func TestMessagesAndDecisionsAPI(t *testing.T) {
	c := newClient(t)
	c.do("POST", "/v1/definitions", `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" targetNamespace="t">
  <message id="m1" name="go"/>
  <process id="waiter" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="w"/>
    <intermediateCatchEvent id="w"><messageEventDefinition messageRef="m1"/></intermediateCatchEvent>
    <sequenceFlow id="f2" sourceRef="w" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`, http.StatusCreated)

	inst := c.do("POST", "/v1/instances", map[string]any{"definitionKey": "waiter", "businessKey": "k1"}, http.StatusCreated)
	res := c.do("POST", "/v1/messages", map[string]any{"name": "go", "correlationKey": "k1"}, http.StatusOK)
	if res["activated"] != 1.0 {
		t.Fatalf("correlate = %v", res)
	}
	got := c.do("GET", "/v1/instances/"+inst["id"].(string), nil, http.StatusOK)
	if got["state"] != "completed" {
		t.Errorf("state = %v", got["state"])
	}

	// DMN deploy + list.
	c.do("POST", "/v1/decisions", `<?xml version="1.0"?>
<definitions xmlns="https://www.omg.org/spec/DMN/20191111/MODEL/">
  <decision id="d1" name="D">
    <decisionTable hitPolicy="FIRST">
      <input label="x"><inputExpression><text>x</text></inputExpression></input>
      <output name="y"/>
      <rule><inputEntry><text>-</text></inputEntry><outputEntry><text>1</text></outputEntry></rule>
    </decisionTable>
  </decision>
</definitions>`, http.StatusCreated)
	req, _ := http.NewRequest("GET", c.srv.URL+"/v1/decisions", nil)
	resp, _ := http.DefaultClient.Do(req)
	var ids []string
	_ = json.NewDecoder(resp.Body).Decode(&ids)
	resp.Body.Close()
	if len(ids) != 1 || ids[0] != "d1" {
		t.Errorf("decisions = %v", ids)
	}
}

func TestSSEStream(t *testing.T) {
	c := newClient(t)
	c.do("POST", "/v1/definitions", approvalXML, http.StatusCreated)

	req, _ := http.NewRequest("GET", c.srv.URL+"/v1/events?type=task.", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %s", ct)
	}

	// Trigger an event after the stream is open.
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.do("POST", "/v1/instances", map[string]any{"definitionKey": "approval"}, http.StatusCreated)
	}()

	sc := bufio.NewScanner(resp.Body)
	deadline := time.After(5 * time.Second)
	got := make(chan string, 1)
	go func() {
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: task.created") {
				got <- line
				return
			}
		}
	}()
	select {
	case line := <-got:
		if line != "event: task.created" {
			t.Errorf("line = %q", line)
		}
	case <-deadline:
		t.Fatal("no task.created event within 5s")
	}
}

func TestNotFoundAndValidation(t *testing.T) {
	c := newClient(t)
	c.do("GET", "/v1/instances/nope", nil, http.StatusNotFound)
	c.do("POST", "/v1/instances", map[string]any{"definitionKey": "ghost"}, http.StatusNotFound)
	c.do("POST", "/v1/instances", map[string]any{}, http.StatusBadRequest)
	c.do("POST", "/v1/messages", map[string]any{}, http.StatusBadRequest)
	c.do("POST", "/v1/tasks/nope/complete", map[string]any{"user": "x"}, http.StatusNotFound)
}

var _ = fmt.Sprintf
