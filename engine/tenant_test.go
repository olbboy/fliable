package engine

import (
	"testing"

	"github.com/olbboy/fliable/store"
)

// TestTenantIsolation deploys the same process key in two tenants and
// verifies versions, instances, tasks and queries stay isolated.
func TestTenantIsolation(t *testing.T) {
	h := newHarness(t)
	xml := procXML(`
  <process id="approve" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="u"/>
    <userTask id="u" fliable:assignee="${who}"/>
    <sequenceFlow id="f2" sourceRef="u" targetRef="e"/>
    <endEvent id="e"/>
  </process>`)

	// Same key, two tenants — independent version streams.
	a1, err := h.e.DeployTenant("acme", []byte(xml), "")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := h.e.DeployTenant("acme", []byte(xml), "")
	b1, _ := h.e.DeployTenant("globex", []byte(xml), "")
	if a1.Version != 1 || a2.Version != 2 || b1.Version != 1 {
		t.Fatalf("versions: acme %d,%d globex %d", a1.Version, a2.Version, b1.Version)
	}
	if a1.TenantID != "acme" || b1.TenantID != "globex" {
		t.Fatal("tenant not recorded on definition")
	}

	ia, err := h.e.StartInstanceTenant("acme", "approve", "o-1", map[string]any{"who": "alice"})
	if err != nil {
		t.Fatal(err)
	}
	ib, _ := h.e.StartInstanceTenant("globex", "approve", "o-2", map[string]any{"who": "bob"})
	if ia.TenantID != "acme" || ib.TenantID != "globex" {
		t.Fatalf("instance tenants: %s %s", ia.TenantID, ib.TenantID)
	}
	// acme runs on version 2 (latest for that tenant).
	if h.instance(ia.ID).DefinitionID != a2.ID {
		t.Error("acme instance not on its latest version")
	}

	// Tenant-scoped queries never cross tenants.
	acme, _ := h.e.ListInstances(store.InstanceFilter{TenantID: "acme"})
	if len(acme) != 1 || acme[0].ID != ia.ID {
		t.Fatalf("acme instance query = %+v", acme)
	}
	acmeTasks, _ := h.e.ListTasks(store.TaskFilter{TenantID: "acme", State: store.TaskCreated})
	if len(acmeTasks) != 1 || acmeTasks[0].TenantID != "acme" || acmeTasks[0].Assignee != "alice" {
		t.Fatalf("acme tasks = %+v", acmeTasks)
	}
	globexTasks, _ := h.e.ListTasks(store.TaskFilter{TenantID: "globex", State: store.TaskCreated})
	if len(globexTasks) != 1 || globexTasks[0].Assignee != "bob" {
		t.Fatalf("globex tasks = %+v", globexTasks)
	}

	// Unknown tenant/key fails cleanly.
	if _, err := h.e.StartInstanceTenant("nope", "approve", "", nil); err == nil {
		t.Error("start in unknown tenant should fail")
	}
}

func TestSuspendResume(t *testing.T) {
	h := newHarness(t)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="w"/>
    <intermediateCatchEvent id="w">
      <timerEventDefinition><timeDuration>PT10M</timeDuration></timerEventDefinition>
    </intermediateCatchEvent>
    <sequenceFlow id="f2" sourceRef="w" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", nil)
	if err := h.e.SuspendInstance(inst.ID); err != nil {
		t.Fatal(err)
	}
	if !h.instance(inst.ID).Suspended {
		t.Fatal("instance not marked suspended")
	}
	// Timer fires while suspended: recorded but the token does not advance.
	h.advance(15 * 60 * 1e9)
	if h.instance(inst.ID).State != store.InstanceActive {
		t.Fatal("suspended instance advanced past the timer")
	}
	// Resume drains the accumulated work to completion.
	if err := h.e.ResumeInstance(inst.ID); err != nil {
		t.Fatal(err)
	}
	h.requireState(inst.ID, store.InstanceCompleted)
}
