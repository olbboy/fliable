package store

import (
	"errors"
	"testing"
	"time"

	"github.com/olbboy/fliable/store/sqltest"
)

// openFakeSQL returns a fresh SQL store over the in-memory test driver.
func openFakeSQL(t *testing.T) *SQL {
	t.Helper()
	db, err := sqltest.OpenDB()
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewSQL(db, SQLOptions{Dialect: DialectSQLite})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSQLDefinitionsAndVersions(t *testing.T) {
	st := openFakeSQL(t)
	for v := 1; v <= 3; v++ {
		if err := st.PutDefinition(&Definition{ID: idN("def", v), Key: "order", Version: v}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutDefinition(&Definition{ID: "def_t1", TenantID: "acme", Key: "order", Version: 1}); err != nil {
		t.Fatal(err)
	}

	d, err := st.LatestDefinition("order")
	if err != nil || d.Version != 3 {
		t.Fatalf("latest: %+v %v", d, err)
	}
	d, err = st.LatestDefinitionForTenant("acme", "order")
	if err != nil || d.Version != 1 || d.TenantID != "acme" {
		t.Fatalf("tenant latest: %+v %v", d, err)
	}
	latest, _ := st.ListDefinitions(true)
	if len(latest) != 1 { // untenanted + tenant share the key; latestOnly by key
		t.Fatalf("latestOnly: %d", len(latest))
	}
	all, _ := st.ListDefinitions(false)
	if len(all) != 4 {
		t.Fatalf("all: %d", len(all))
	}
	if _, err := st.GetDefinition("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func idN(prefix string, n int) string { return prefix + string(rune('a'+n)) }

func TestSQLInstanceFiltering(t *testing.T) {
	st := openFakeSQL(t)
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	put := func(id, key, tenant string, state InstanceState, amount float64, started time.Time) {
		if err := st.PutInstance(&Instance{
			ID: id, DefinitionKey: key, TenantID: tenant, State: state,
			Variables: map[string]any{"amount": amount}, Tokens: map[string]*Token{},
			StartedAt: started,
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("inst_a", "order", "", InstanceActive, 100, base)
	put("inst_b", "order", "", InstanceCompleted, 900, base.Add(time.Hour))
	put("inst_c", "ship", "acme", InstanceActive, 50, base.Add(2*time.Hour))

	got, _ := st.ListInstances(InstanceFilter{DefinitionKey: "order"})
	if len(got) != 2 {
		t.Fatalf("by key: %d", len(got))
	}
	got, _ = st.ListInstances(InstanceFilter{State: InstanceActive})
	if len(got) != 2 {
		t.Fatalf("by state: %d", len(got))
	}
	got, _ = st.ListInstances(InstanceFilter{TenantID: "acme"})
	if len(got) != 1 || got[0].ID != "inst_c" {
		t.Fatalf("by tenant: %+v", got)
	}
	got, _ = st.ListInstances(InstanceFilter{Vars: []VarMatch{{Name: "amount", Op: "gte", Value: 500.0}}})
	if len(got) != 1 || got[0].ID != "inst_b" {
		t.Fatalf("by var: %+v", got)
	}
	got, _ = st.ListInstances(InstanceFilter{StartedAfter: base.Add(30 * time.Minute)})
	if len(got) != 2 {
		t.Fatalf("by time: %d", len(got))
	}
	// Keyset pagination ascending.
	got, _ = st.ListInstances(InstanceFilter{Limit: 1})
	if len(got) != 1 || got[0].ID != "inst_a" {
		t.Fatalf("page 1: %+v", got)
	}
	got, _ = st.ListInstances(InstanceFilter{Limit: 2, Cursor: got[0].ID})
	if len(got) != 2 || got[0].ID != "inst_b" {
		t.Fatalf("page 2: %+v", got)
	}
	// Rev bumps like Memory.
	in, _ := st.GetInstance("inst_a")
	if in.Rev != 1 {
		t.Fatalf("rev: %d", in.Rev)
	}
}

func TestSQLTaskVarsJoin(t *testing.T) {
	st := openFakeSQL(t)
	if err := st.PutInstance(&Instance{ID: "i1", DefinitionKey: "p", State: InstanceActive,
		Variables: map[string]any{"amount": 900.0}, Tokens: map[string]*Token{}}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTask(&Task{ID: "t1", InstanceID: "i1", State: TaskCreated, CandidateGroups: []string{"fraud"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ListTasks(TaskFilter{CandidateGroup: "fraud", Vars: []VarMatch{{Name: "amount", Op: "gt", Value: 500.0}}})
	if len(got) != 1 {
		t.Fatalf("vars join: %d", len(got))
	}
	got, _ = st.ListTasks(TaskFilter{Vars: []VarMatch{{Name: "amount", Op: "lt", Value: 500.0}}})
	if len(got) != 0 {
		t.Fatalf("vars join negative: %d", len(got))
	}
}

func TestSQLDueJobsClaimOnce(t *testing.T) {
	st := openFakeSQL(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	for i, due := range []time.Time{now.Add(-time.Minute), now.Add(-time.Second), now.Add(time.Hour)} {
		if err := st.PutJob(&Job{ID: idN("job", i), Kind: JobTimer, DueAt: due, InstanceID: "i1"}); err != nil {
			t.Fatal(err)
		}
	}
	due, err := st.DueJobs(now, 10)
	if err != nil || len(due) != 2 {
		t.Fatalf("due: %d %v", len(due), err)
	}
	// Claimed jobs are gone; the future one remains.
	again, _ := st.DueJobs(now, 10)
	if len(again) != 0 {
		t.Fatalf("double claim: %d", len(again))
	}
	left, _ := st.ListJobs("i1")
	if len(left) != 1 {
		t.Fatalf("remaining: %d", len(left))
	}
}

func TestSQLFetchAndLock(t *testing.T) {
	st := openFakeSQL(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := st.PutExternalTask(&ExternalTask{
			ID: idN("ext", i), Topic: "ship", State: ExternalPending, InstanceID: "i1", Retries: 3,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.FetchAndLockExternalTasks("ship", "w1", now.Add(time.Minute), now, 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("fetch: %d %v", len(got), err)
	}
	// A second worker only gets the remaining one while locks hold.
	got2, _ := st.FetchAndLockExternalTasks("ship", "w2", now.Add(time.Minute), now, 10)
	if len(got2) != 1 {
		t.Fatalf("second worker: %d", len(got2))
	}
	// After lock expiry they are reclaimable.
	later := now.Add(2 * time.Minute)
	got3, _ := st.FetchAndLockExternalTasks("ship", "w3", later.Add(time.Minute), later, 10)
	if len(got3) != 3 {
		t.Fatalf("after expiry: %d", len(got3))
	}

	// Agent jobs share the same claim discipline.
	if err := st.PutAgentJob(&AgentJob{ID: "aj1", Topic: "llm", State: AgentPending, InstanceID: "i1", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.FetchAndLockAgentJobs("llm", "ai-1", now.Add(time.Minute), now, 5)
	if err != nil || len(jobs) != 1 || jobs[0].LockedBy != "ai-1" {
		t.Fatalf("agent fetch: %+v %v", jobs, err)
	}
	if again, _ := st.FetchAndLockAgentJobs("llm", "ai-2", now.Add(time.Minute), now, 5); len(again) != 0 {
		t.Fatalf("agent double claim: %d", len(again))
	}
}

func TestSQLHistorySeqAndPurge(t *testing.T) {
	st := openFakeSQL(t)
	if err := st.PutInstance(&Instance{ID: "i1", State: InstanceActive, Tokens: map[string]*Token{}, Variables: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := st.AppendHistory(&HistoryEvent{InstanceID: "i1", Type: "element.activated"}); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := st.ListHistory("i1", HistoryFilter{})
	if len(evs) != 3 || evs[0].Seq != 1 || evs[2].Seq != 3 {
		t.Fatalf("seq: %+v", evs)
	}
	// Seq survives a fresh store handle (MAX(seq) reload).
	st2 := &SQL{db: st.db, d: st.d, histSeq: map[string]int64{}}
	if err := st2.AppendHistory(&HistoryEvent{InstanceID: "i1", Type: "element.completed"}); err != nil {
		t.Fatal(err)
	}
	evs, _ = st.ListHistory("i1", HistoryFilter{})
	if len(evs) != 4 || evs[3].Seq != 4 {
		t.Fatalf("seq after reload: %+v", evs)
	}
	// Filters.
	evs, _ = st.ListHistory("i1", HistoryFilter{Type: "element.completed"})
	if len(evs) != 1 {
		t.Fatalf("type filter: %d", len(evs))
	}
	evs, _ = st.ListHistory("i1", HistoryFilter{AfterSeq: 2})
	if len(evs) != 2 {
		t.Fatalf("afterSeq: %d", len(evs))
	}

	// Purge removes the instance and every dependent record.
	if err := st.PutTask(&Task{ID: "t1", InstanceID: "i1", State: TaskCreated}); err != nil {
		t.Fatal(err)
	}
	if err := st.PurgeInstance("i1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetInstance("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("instance survived purge: %v", err)
	}
	if evs, _ := st.ListHistory("i1", HistoryFilter{}); len(evs) != 0 {
		t.Fatalf("history survived purge: %d", len(evs))
	}
	if tasks, _ := st.ListTasks(TaskFilter{InstanceID: "i1"}); len(tasks) != 0 {
		t.Fatalf("tasks survived purge: %d", len(tasks))
	}
}

func TestSQLBlobsAndSubscriptions(t *testing.T) {
	st := openFakeSQL(t)
	testBlobCRUD(t, st)

	if err := st.PutSubscription(&Subscription{ID: "s1", Kind: SubMessage, Name: "paid", InstanceID: "i1"}); err != nil {
		t.Fatal(err)
	}
	isStart := false
	subs, _ := st.ListSubscriptions(SubscriptionFilter{Kind: SubMessage, Name: "paid", IsStart: &isStart})
	if len(subs) != 1 {
		t.Fatalf("subs: %d", len(subs))
	}
	if err := st.DeleteSubscription("s1"); err != nil {
		t.Fatal(err)
	}
	subs, _ = st.ListSubscriptions(SubscriptionFilter{})
	if len(subs) != 0 {
		t.Fatalf("after delete: %d", len(subs))
	}

	unresolved := false
	if err := st.PutIncident(&Incident{ID: "inc1", InstanceID: "i1"}); err != nil {
		t.Fatal(err)
	}
	incs, _ := st.ListIncidents(IncidentFilter{Resolved: &unresolved})
	if len(incs) != 1 {
		t.Fatalf("incidents: %d", len(incs))
	}
}
