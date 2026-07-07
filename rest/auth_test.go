package rest

import (
	"net/http"
	"testing"
	"time"

	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
)

func TestTokenMintParse(t *testing.T) {
	key := []byte("super-secret-signing-key")
	p := Principal{Subject: "u1", Tenant: "acme", Roles: []Role{RoleOperator}}
	tok, err := MintToken(key, p, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseToken(key, tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "u1" || got.Tenant != "acme" || !got.has(RoleOperator) {
		t.Fatalf("parsed = %+v", got)
	}
	// Tampered signature rejected.
	if _, err := ParseToken([]byte("wrong-key"), tok); err == nil {
		t.Error("token verified under wrong key")
	}
	// Expired token rejected.
	exp, _ := MintToken(key, p, -time.Second)
	if _, err := ParseToken(key, exp); err == nil {
		t.Error("expired token accepted")
	}
}

func TestRBACRoles(t *testing.T) {
	key := []byte("k")
	e := engine.New(store.NewMemory())
	s := New(e, WithSigningKey(key))
	srv := newTestServer(t, s)
	c := &client{t: t, srv: srv}

	viewer, _ := MintToken(key, Principal{Subject: "v", Roles: []Role{RoleViewer}}, time.Hour)
	operator, _ := MintToken(key, Principal{Subject: "o", Roles: []Role{RoleOperator}}, time.Hour)
	admin, _ := MintToken(key, Principal{Subject: "a", Roles: []Role{RoleAdmin}}, time.Hour)

	// No token → 401.
	c.bearer = ""
	c.doAuth("GET", "/v1/instances", http.StatusUnauthorized)
	// Viewer can read, cannot deploy (admin) or start (operator).
	c.bearer = viewer
	c.doAuth("GET", "/v1/instances", http.StatusOK)
	c.doAuth("POST", "/v1/instances", http.StatusForbidden)
	c.doAuthBody("POST", "/v1/definitions", approvalXML, http.StatusForbidden)
	// Operator can start but not deploy.
	c.bearer = operator
	c.doAuthBody("POST", "/v1/definitions", approvalXML, http.StatusForbidden)
	// Admin can deploy.
	c.bearer = admin
	c.doAuthBody("POST", "/v1/definitions", approvalXML, http.StatusCreated)
	// Operator can now start an instance of it.
	c.bearer = operator
	c.doAuthBody("POST", "/v1/instances", `{"definitionKey":"approval"}`, http.StatusCreated)
}

func TestTenantConfinement(t *testing.T) {
	key := []byte("k")
	e := engine.New(store.NewMemory())
	// Seed: deploy + start instances in two tenants directly on the engine.
	_, _ = e.DeployTenant("acme", []byte(approvalXML), "")
	_, _ = e.DeployTenant("globex", []byte(approvalXML), "")
	_, _ = e.StartInstanceTenant("acme", "approval", "a1", nil)
	_, _ = e.StartInstanceTenant("globex", "approval", "g1", nil)

	s := New(e, WithSigningKey(key))
	srv := newTestServer(t, s)
	c := &client{t: t, srv: srv}

	// A token confined to acme sees only acme, even if it asks for globex.
	acme, _ := MintToken(key, Principal{Subject: "u", Tenant: "acme", Roles: []Role{RoleViewer}}, time.Hour)
	c.bearer = acme
	items := c.doAuthPage("GET", "/v1/instances?tenantId=globex", http.StatusOK)
	if len(items) != 1 || items[0]["businessKey"] != "a1" {
		t.Fatalf("tenant confinement broken: %+v", items)
	}
}

func TestCORSPreflight(t *testing.T) {
	e := engine.New(store.NewMemory())
	s := New(e, WithCORS(DefaultCORS()))
	srv := newTestServer(t, s)

	req, _ := http.NewRequest("OPTIONS", srv.URL+"/v1/instances", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("allow-origin = %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}
	if !contains(resp.Header.Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("allow-methods = %q", resp.Header.Get("Access-Control-Allow-Methods"))
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
