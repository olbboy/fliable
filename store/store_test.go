package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
}

func testStoreBasics(t *testing.T, s Store) {
	t.Helper()
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	// Definitions & versioning.
	if err := s.PutDefinition(&Definition{ID: "d1", Key: "order", Version: 1, DeployedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDefinition(&Definition{ID: "d2", Key: "order", Version: 2, DeployedAt: now}); err != nil {
		t.Fatal(err)
	}
	latest, err := s.LatestDefinition("order")
	if err != nil || latest.ID != "d2" {
		t.Fatalf("LatestDefinition = %v, %v", latest, err)
	}
	if _, err := s.GetDefinition("nope"); err != ErrNotFound {
		t.Errorf("GetDefinition(nope) err = %v", err)
	}

	// Instances round-trip with nested state.
	inst := &Instance{
		ID: "i1", DefinitionID: "d2", DefinitionKey: "order", State: InstanceActive,
		Variables: map[string]any{"amount": 250.0, "items": []any{"a", "b"}},
		Tokens: map[string]*Token{
			"t1": {ID: "t1", ElementID: "task1", State: TokenWaitTask, ScopePath: []string{"sub1"}},
		},
		Multi:     map[string]*MultiInstanceState{"t9": {TokenID: "t9", Total: 3, Completed: 1, Items: []any{1.0, 2.0, 3.0}}},
		StartedAt: now,
	}
	if err := s.PutInstance(inst); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInstance("i1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Variables["amount"] != 250.0 || got.Tokens["t1"].State != TokenWaitTask {
		t.Errorf("instance round-trip = %+v", got)
	}
	if got.Multi["t9"].Total != 3 {
		t.Errorf("multi state = %+v", got.Multi)
	}
	// Mutating the returned copy must not affect the store.
	got.Variables["amount"] = 999.0
	got2, _ := s.GetInstance("i1")
	if got2.Variables["amount"] != 250.0 {
		t.Error("store returned aliased instance state")
	}

	// Instance filters.
	insts, _ := s.ListInstances(InstanceFilter{DefinitionKey: "order", State: InstanceActive})
	if len(insts) != 1 {
		t.Errorf("ListInstances = %d", len(insts))
	}

	// Tasks.
	if err := s.PutTask(&Task{ID: "tk1", InstanceID: "i1", State: TaskCreated, Assignee: "alice", CandidateGroups: []string{"ops"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	tasks, _ := s.ListTasks(TaskFilter{Assignee: "alice"})
	if len(tasks) != 1 {
		t.Errorf("tasks by assignee = %d", len(tasks))
	}
	tasks, _ = s.ListTasks(TaskFilter{CandidateGroup: "ops"})
	if len(tasks) != 1 {
		t.Errorf("tasks by group = %d", len(tasks))
	}

	// Jobs & due claiming.
	if err := s.PutJob(&Job{ID: "j1", Kind: JobTimer, InstanceID: "i1", DueAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutJob(&Job{ID: "j2", Kind: JobTimer, InstanceID: "i1", DueAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	due, _ := s.DueJobs(now.Add(2*time.Minute), 10)
	if len(due) != 1 || due[0].ID != "j1" {
		t.Fatalf("DueJobs = %v", due)
	}
	// Claimed jobs are gone.
	due, _ = s.DueJobs(now.Add(2*time.Minute), 10)
	if len(due) != 0 {
		t.Errorf("DueJobs should be empty after claim, got %v", due)
	}
	remaining, _ := s.ListJobs("i1")
	if len(remaining) != 1 || remaining[0].ID != "j2" {
		t.Errorf("remaining jobs = %v", remaining)
	}

	// External tasks: lock semantics.
	if err := s.PutExternalTask(&ExternalTask{ID: "e1", Topic: "ship", State: ExternalPending, InstanceID: "i1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	locked, _ := s.FetchAndLockExternalTasks("ship", "w1", now.Add(time.Minute), now, 5)
	if len(locked) != 1 || locked[0].LockedBy != "w1" {
		t.Fatalf("FetchAndLock = %v", locked)
	}
	// Second worker can't steal an active lock.
	locked2, _ := s.FetchAndLockExternalTasks("ship", "w2", now.Add(time.Minute), now.Add(30*time.Second), 5)
	if len(locked2) != 0 {
		t.Errorf("lock stolen: %v", locked2)
	}
	// After expiry the lock is reclaimable.
	locked3, _ := s.FetchAndLockExternalTasks("ship", "w2", now.Add(3*time.Minute), now.Add(2*time.Minute), 5)
	if len(locked3) != 1 || locked3[0].LockedBy != "w2" {
		t.Errorf("expired lock not reclaimed: %v", locked3)
	}

	// Subscriptions.
	if err := s.PutSubscription(&Subscription{ID: "s1", Kind: SubMessage, Name: "pay", InstanceID: "i1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	subs, _ := s.ListSubscriptions(SubscriptionFilter{Kind: SubMessage, Name: "pay"})
	if len(subs) != 1 {
		t.Errorf("subs = %v", subs)
	}
	if err := s.DeleteSubscription("s1"); err != nil {
		t.Fatal(err)
	}
	subs, _ = s.ListSubscriptions(SubscriptionFilter{Kind: SubMessage, Name: "pay"})
	if len(subs) != 0 {
		t.Errorf("subs after delete = %v", subs)
	}

	// Incidents.
	if err := s.PutIncident(&Incident{ID: "inc1", InstanceID: "i1", Message: "boom", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	f := false
	incs, _ := s.ListIncidents(IncidentFilter{InstanceID: "i1", Resolved: &f})
	if len(incs) != 1 {
		t.Errorf("incidents = %v", incs)
	}

	// History: sequencing per instance.
	for _, typ := range []string{HistInstanceStarted, HistElementActivated, HistElementCompleted} {
		if err := s.AppendHistory(&HistoryEvent{InstanceID: "i1", Time: now, Type: typ}); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := s.ListHistory("i1", HistoryFilter{})
	if len(evs) != 3 || evs[0].Seq != 1 || evs[2].Seq != 3 {
		t.Fatalf("history = %+v", evs)
	}
	evs, _ = s.ListHistory("i1", HistoryFilter{AfterSeq: 1, Type: "element."})
	if len(evs) != 2 {
		t.Errorf("filtered history = %+v", evs)
	}
}

func TestMemoryStore(t *testing.T) {
	testStoreBasics(t, NewMemory())
}

func TestJournalStore(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	testStoreBasics(t, j)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJournalRecovery(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	j, err := OpenJournal(dir, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.PutDefinition(&Definition{ID: "d1", Key: "p", Version: 1}); err != nil {
		t.Fatal(err)
	}
	inst := &Instance{ID: "i1", DefinitionID: "d1", DefinitionKey: "p", State: InstanceActive,
		Variables: map[string]any{"x": 1.0},
		Tokens:    map[string]*Token{"t1": {ID: "t1", ElementID: "e1", State: TokenActive}},
		StartedAt: now}
	if err := j.PutInstance(inst); err != nil {
		t.Fatal(err)
	}
	if err := j.PutJob(&Job{ID: "j1", Kind: JobTimer, InstanceID: "i1", DueAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := j.DeleteJob("j1"); err != nil {
		t.Fatal(err)
	}
	if err := j.AppendHistory(&HistoryEvent{InstanceID: "i1", Type: HistInstanceStarted, Time: now}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: state must be identical.
	j2, err := OpenJournal(dir, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	got, err := j2.GetInstance("i1")
	if err != nil || got.Variables["x"] != 1.0 || got.Tokens["t1"].ElementID != "e1" {
		t.Fatalf("recovered instance = %+v, %v", got, err)
	}
	jobs, _ := j2.ListJobs("i1")
	if len(jobs) != 0 {
		t.Errorf("deleted job resurrected: %v", jobs)
	}
	evs, _ := j2.ListHistory("i1", HistoryFilter{})
	if len(evs) != 1 || evs[0].Type != HistInstanceStarted {
		t.Errorf("recovered history = %+v", evs)
	}
}

func TestJournalCompaction(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir, JournalOptions{CompactEvery: 10})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		inst := &Instance{ID: "i1", DefinitionKey: "p", State: InstanceActive,
			Variables: map[string]any{"n": float64(i)},
			Tokens:    map[string]*Token{}}
		if err := j.PutInstance(inst); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	// Snapshot must exist and journal must be small.
	if _, err := filepath.Glob(filepath.Join(dir, "snapshot.json")); err != nil {
		t.Fatal(err)
	}
	j2, err := OpenJournal(dir, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	got, err := j2.GetInstance("i1")
	if err != nil || got.Variables["n"] != 24.0 {
		t.Fatalf("after compaction = %+v, %v", got, err)
	}
}

func TestJournalSurvivesTornWrite(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.PutDefinition(&Definition{ID: "d1", Key: "p", Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-append: garbage partial line at the end.
	f, err := openAppend(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"op":"def","d":{"id":"d2","ke`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	j2, err := OpenJournal(dir, JournalOptions{})
	if err != nil {
		t.Fatalf("torn write should not prevent recovery: %v", err)
	}
	defer j2.Close()
	if _, err := j2.GetDefinition("d1"); err != nil {
		t.Errorf("d1 lost: %v", err)
	}
	if _, err := j2.GetDefinition("d2"); err != ErrNotFound {
		t.Errorf("torn d2 should not exist, err = %v", err)
	}
}
