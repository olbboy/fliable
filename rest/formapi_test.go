package rest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/form"
	"github.com/olbboy/fliable/store"
)

const formProcessXML = `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="t">
  <process id="expense" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="approve"/>
    <userTask id="approve" name="Approve" fliable:formKey="approveExpense"/>
    <sequenceFlow id="f2" sourceRef="approve" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`

func TestFormBoundTaskEndToEnd(t *testing.T) {
	st := store.NewMemory()
	forms := form.NewRegistry(st)
	e := engine.New(st, engine.WithFormValidator(forms))
	srv := New(e, WithForms(forms))

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// Deploy the form and the process.
	rec := do("POST", "/v1/forms", `{"key":"approveExpense","fields":[
		{"id":"approved","type":"boolean","required":true},
		{"id":"amount","type":"number","min":0}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deploy form: %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/v1/definitions", formProcessXML); rec.Code != http.StatusCreated {
		t.Fatalf("deploy process: %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/v1/instances", `{"definitionKey":"expense"}`); rec.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}

	tasks, err := e.ListTasks(store.TaskFilter{State: store.TaskCreated})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks: %v %d", err, len(tasks))
	}
	taskID := tasks[0].ID

	// The task exposes its bound form for rendering.
	rec = do("GET", "/v1/tasks/"+taskID+"/form", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"approveExpense"`) {
		t.Fatalf("task form: %d %s", rec.Code, rec.Body)
	}

	// Invalid submission → 422, task stays open.
	rec = do("POST", "/v1/tasks/"+taskID+"/complete", `{"variables":{"amount":-1}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid submit: %d %s", rec.Code, rec.Body)
	}
	if tk, _ := e.Store().GetTask(taskID); tk.State != store.TaskCreated {
		t.Fatalf("task advanced on invalid submit: %s", tk.State)
	}

	// Valid submission completes the instance.
	rec = do("POST", "/v1/tasks/"+taskID+"/complete", `{"variables":{"approved":true,"amount":12}}`)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("valid submit: %d %s", rec.Code, rec.Body)
	}
	insts, _ := e.ListInstances(store.InstanceFilter{State: store.InstanceCompleted})
	if len(insts) != 1 {
		t.Fatalf("instance not completed")
	}
}
