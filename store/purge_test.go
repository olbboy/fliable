package store

import (
	"testing"
	"time"
)

func testPurge(t *testing.T, s Store) {
	t.Helper()
	now := time.Now()
	_ = s.PutInstance(&Instance{ID: "i1", DefinitionKey: "p", State: InstanceCompleted,
		Variables: map[string]any{}, Tokens: map[string]*Token{}, StartedAt: now, EndedAt: now})
	_ = s.PutTask(&Task{ID: "t1", InstanceID: "i1", State: TaskCompleted, CreatedAt: now})
	_ = s.PutJob(&Job{ID: "j1", InstanceID: "i1", DueAt: now})
	_ = s.PutSubscription(&Subscription{ID: "s1", InstanceID: "i1", Kind: SubMessage, Name: "m", CreatedAt: now})
	_ = s.PutIncident(&Incident{ID: "inc1", InstanceID: "i1", CreatedAt: now})
	_ = s.AppendHistory(&HistoryEvent{InstanceID: "i1", Type: "x", Time: now})
	// Keep a second instance to prove scoping.
	_ = s.PutInstance(&Instance{ID: "i2", DefinitionKey: "p", State: InstanceActive,
		Variables: map[string]any{}, Tokens: map[string]*Token{}, StartedAt: now})
	_ = s.PutTask(&Task{ID: "t2", InstanceID: "i2", State: TaskCreated, CreatedAt: now})

	if err := s.PurgeInstance("i1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetInstance("i1"); err != ErrNotFound {
		t.Error("instance not purged")
	}
	if evs, _ := s.ListHistory("i1", HistoryFilter{}); len(evs) != 0 {
		t.Error("history not purged")
	}
	if ts, _ := s.ListTasks(TaskFilter{InstanceID: "i1"}); len(ts) != 0 {
		t.Error("tasks not purged")
	}
	if js, _ := s.ListJobs("i1"); len(js) != 0 {
		t.Error("jobs not purged")
	}
	f := false
	if incs, _ := s.ListIncidents(IncidentFilter{InstanceID: "i1", Resolved: &f}); len(incs) != 0 {
		t.Error("incidents not purged")
	}
	// The other instance is untouched.
	if _, err := s.GetInstance("i2"); err != nil {
		t.Error("purge removed the wrong instance")
	}
	if ts, _ := s.ListTasks(TaskFilter{InstanceID: "i2"}); len(ts) != 1 {
		t.Error("purge removed the wrong tasks")
	}
}

func TestPurgeMemory(t *testing.T) { testPurge(t, NewMemory()) }

func TestPurgeJournalRecovery(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	testPurge(t, j)
	j.Close()
	// Reopen: the purge must survive replay.
	j2, err := OpenJournal(dir, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if _, err := j2.GetInstance("i1"); err != ErrNotFound {
		t.Error("purge did not survive journal replay")
	}
	if _, err := j2.GetInstance("i2"); err != nil {
		t.Error("surviving instance lost after replay")
	}
}
