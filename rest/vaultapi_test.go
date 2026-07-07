package rest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
	"github.com/olbboy/fliable/vault"
)

func TestSecretsAPI(t *testing.T) {
	st := store.NewMemory()
	v, err := vault.New(st, []byte("master"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(engine.New(st), WithVault(v), WithAPIKey("admin-key"))

	do := func(method, path, body string, admin bool) *httptest.ResponseRecorder {
		var rd *strings.Reader
		if body != "" {
			rd = strings.NewReader(body)
		} else {
			rd = strings.NewReader("")
		}
		req := httptest.NewRequest(method, path, rd)
		if admin {
			req.Header.Set("X-Api-Key", "admin-key")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// Unauthenticated access is rejected.
	if rec := do("GET", "/v1/secrets", "", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon list: %d", rec.Code)
	}

	if rec := do("PUT", "/v1/secrets/slack", `{"value":"xoxb-1"}`, true); rec.Code != http.StatusNoContent {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	rec := do("GET", "/v1/secrets", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"slack"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "xoxb-1") {
		t.Fatal("list leaked a secret value")
	}
	rec = do("GET", "/v1/secrets/slack/value", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "xoxb-1") {
		t.Fatalf("value: %d %s", rec.Code, rec.Body)
	}
	if rec := do("DELETE", "/v1/secrets/slack", "", true); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := do("GET", "/v1/secrets/slack/value", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted value: %d", rec.Code)
	}
}

func TestHandlerReadsSecret(t *testing.T) {
	st := store.NewMemory()
	v, _ := vault.New(st, []byte("master"))
	_ = v.Set("apiToken", "tok-42")

	e := engine.New(st, engine.WithSecrets(v))
	var seen string
	e.RegisterHandler("callAPI", func(ctx engine.Context) (map[string]any, error) {
		s, err := ctx.Secret("apiToken")
		if err != nil {
			return nil, err
		}
		seen = s
		return map[string]any{"ok": true}, nil
	})
	if _, err := e.Deploy([]byte(`<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="t">
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="svc"/>
    <serviceTask id="svc" fliable:type="callAPI"/>
    <sequenceFlow id="f2" sourceRef="svc" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`), ""); err != nil {
		t.Fatal(err)
	}
	inst, err := e.StartInstance("p", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != store.InstanceCompleted {
		t.Fatalf("instance %s", inst.State)
	}
	if seen != "tok-42" {
		t.Fatalf("handler saw %q", seen)
	}
}
