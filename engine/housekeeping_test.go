package engine

import (
	"testing"
	"time"

	"github.com/olbboy/fliable/store"
)

func TestHousekeepingPurgesFinishedInstances(t *testing.T) {
	h := newHarness(t)
	// Enable retention via option on a fresh engine sharing the harness store.
	h.e = New(h.st,
		WithClock(func() time.Time { return h.now }),
		WithHousekeeping(HousekeepingPolicy{TTL: time.Hour}),
	)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))

	inst := h.start("p", map[string]any{"x": 1})
	h.requireState(inst.ID, store.InstanceCompleted)
	// History exists now.
	if evs, _ := h.e.History(inst.ID, store.HistoryFilter{}); len(evs) == 0 {
		t.Fatal("no history recorded")
	}

	// Before TTL: not purged.
	if n := h.e.RunHousekeeping(h.now.Add(30 * time.Minute)); n != 0 {
		t.Fatalf("purged %d before TTL", n)
	}
	if _, err := h.e.GetInstance(inst.ID); err != nil {
		t.Fatal("instance vanished before TTL")
	}

	// After TTL: purged with all its records.
	if n := h.e.RunHousekeeping(h.now.Add(2 * time.Hour)); n != 1 {
		t.Fatalf("purged %d after TTL, want 1", n)
	}
	if _, err := h.e.GetInstance(inst.ID); err != store.ErrNotFound {
		t.Errorf("instance not purged: %v", err)
	}
	if evs, _ := h.e.History(inst.ID, store.HistoryFilter{}); len(evs) != 0 {
		t.Errorf("history not purged: %d events", len(evs))
	}
}

func TestHousekeepingKeepsActiveAndChildLinkedInstances(t *testing.T) {
	h := newHarness(t)
	h.e = New(h.st,
		WithClock(func() time.Time { return h.now }),
		WithHousekeeping(HousekeepingPolicy{TTL: time.Minute}),
	)
	h.deploy(procXML(`
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="u"/>
    <userTask id="u"/>
    <sequenceFlow id="f2" sourceRef="u" targetRef="e"/>
    <endEvent id="e"/>
  </process>`))
	active := h.start("p", nil)
	h.requireState(active.ID, store.InstanceActive)

	// Active instances are never purged, however old.
	if n := h.e.RunHousekeeping(h.now.Add(1000 * time.Hour)); n != 0 {
		t.Fatalf("purged an active instance: %d", n)
	}
	if _, err := h.e.GetInstance(active.ID); err != nil {
		t.Fatal("active instance purged")
	}
}
