package replicate

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
)

const failoverXML = `<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="t">
  <process id="order" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="approve"/>
    <userTask id="approve" name="Approve order"/>
    <sequenceFlow id="f2" sourceRef="approve" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`

// waitFor polls until cond succeeds (replication is async).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestLeaderFollowerFailover(t *testing.T) {
	// Follower: journal + receiver on a private listener.
	follower, err := store.OpenJournal(t.TempDir(), store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rcv := httptest.NewServer(NewReceiver(follower, "repl-secret"))
	defer rcv.Close()

	// Leader: journal + engine + sender.
	leader, err := store.OpenJournal(t.TempDir(), store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	snd := NewSender(leader, rcv.URL, "repl-secret", SenderOptions{FlushInterval: 20 * time.Millisecond})

	eng := engine.New(leader)
	if _, err := eng.Deploy([]byte(failoverXML), ""); err != nil {
		t.Fatal(err)
	}
	inst, err := eng.StartInstance("order", "ord-9", map[string]any{"amount": 250.0})
	if err != nil {
		t.Fatal(err)
	}
	if err := leader.PutBlob(&store.Blob{Kind: "secret", Key: "k", Data: []byte("sealed")}); err != nil {
		t.Fatal(err)
	}

	// The follower converges on the leader's state.
	waitFor(t, "instance replication", func() bool {
		got, err := follower.GetInstance(inst.ID)
		return err == nil && got.BusinessKey == "ord-9"
	})
	waitFor(t, "task replication", func() bool {
		tasks, _ := follower.ListTasks(store.TaskFilter{InstanceID: inst.ID, State: store.TaskCreated})
		return len(tasks) == 1
	})
	waitFor(t, "blob replication", func() bool {
		_, err := follower.GetBlob("secret", "k")
		return err == nil
	})
	// History replicated without duplicates.
	leaderHist, _ := leader.ListHistory(inst.ID, store.HistoryFilter{})
	waitFor(t, "history replication", func() bool {
		fh, _ := follower.ListHistory(inst.ID, store.HistoryFilter{})
		return len(fh) == len(leaderHist)
	})

	// LEADER DIES. Stop the sender, promote the follower: just start an
	// engine on its store.
	snd.Close()
	leader.Close()

	promoted := engine.New(follower)
	promoted.Start()
	defer promoted.Stop()

	tasks, err := promoted.ListTasks(store.TaskFilter{InstanceID: inst.ID, State: store.TaskCreated})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("promoted tasks: %v %d", err, len(tasks))
	}
	if err := promoted.CompleteTask(tasks[0].ID, map[string]any{"approved": true}, "alice"); err != nil {
		t.Fatalf("complete on promoted engine: %v", err)
	}
	got, err := promoted.GetInstance(inst.ID)
	if err != nil || got.State != store.InstanceCompleted {
		t.Fatalf("promoted completion: %v %s", err, got.State)
	}
}

func TestResyncAfterFollowerRestart(t *testing.T) {
	followerDir := t.TempDir()
	follower, err := store.OpenJournal(followerDir, store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rcv := httptest.NewServer(NewReceiver(follower, ""))

	leader, err := store.OpenJournal(t.TempDir(), store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	snd := NewSender(leader, rcv.URL, "", SenderOptions{FlushInterval: 20 * time.Millisecond})
	defer snd.Close()

	eng := engine.New(leader)
	if _, err := eng.Deploy([]byte(failoverXML), ""); err != nil {
		t.Fatal(err)
	}
	first, _ := eng.StartInstance("order", "one", nil)
	waitFor(t, "first instance", func() bool {
		_, err := follower.GetInstance(first.ID)
		return err == nil
	})

	// Follower goes down; the leader keeps working (buffer absorbs or
	// resyncs), then the follower comes back.
	rcv.Close()
	follower.Close()
	second, _ := eng.StartInstance("order", "two", nil)

	follower2, err := store.OpenJournal(followerDir, store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer follower2.Close()
	rcv2 := httptest.NewServer(NewReceiver(follower2, ""))
	defer rcv2.Close()

	// Point a fresh sender at the revived follower (a real deployment
	// reuses the same URL; httptest allocates a new port).
	snd2 := NewSender(leader, rcv2.URL, "", SenderOptions{FlushInterval: 20 * time.Millisecond})
	defer snd2.Close()

	waitFor(t, "resync", func() bool {
		_, e1 := follower2.GetInstance(first.ID)
		_, e2 := follower2.GetInstance(second.ID)
		return e1 == nil && e2 == nil
	})
	// No duplicated history after the overlap of snapshot + stream.
	lh, _ := leader.ListHistory(first.ID, store.HistoryFilter{})
	fh, _ := follower2.ListHistory(first.ID, store.HistoryFilter{})
	if len(lh) != len(fh) {
		t.Fatalf("history diverged: leader %d follower %d", len(lh), len(fh))
	}
}

func TestReceiverRejectsBadSecret(t *testing.T) {
	follower, err := store.OpenJournal(t.TempDir(), store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close()
	rcv := httptest.NewServer(NewReceiver(follower, "right"))
	defer rcv.Close()

	leader, err := store.OpenJournal(t.TempDir(), store.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	snd := NewSender(leader, rcv.URL, "wrong", SenderOptions{FlushInterval: 10 * time.Millisecond})
	defer snd.Close()

	eng := engine.New(leader)
	if _, err := eng.Deploy([]byte(failoverXML), ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	defs, _ := follower.ListDefinitions(false)
	if len(defs) != 0 {
		t.Fatal("replication with a wrong secret succeeded")
	}
}
