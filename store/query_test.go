package store

import (
	"testing"
	"time"
)

func TestVarMatch(t *testing.T) {
	vars := map[string]any{"amount": 1500.0, "region": "EU", "vip": true, "note": "urgent order"}
	cases := []struct {
		m    VarMatch
		want bool
	}{
		{VarMatch{"amount", "gte", 1000.0}, true},
		{VarMatch{"amount", "lt", 1000.0}, false},
		{VarMatch{"amount", "eq", 1500.0}, true},
		{VarMatch{"region", "eq", "EU"}, true},
		{VarMatch{"region", "ne", "US"}, true},
		{VarMatch{"note", "contains", "urgent"}, true},
		{VarMatch{"note", "contains", "cancel"}, false},
		{VarMatch{"vip", "eq", true}, true},
		{VarMatch{"missing", "exists", nil}, false},
		{VarMatch{"amount", "exists", nil}, true},
	}
	for _, tc := range cases {
		if got := MatchVars(vars, []VarMatch{tc.m}); got != tc.want {
			t.Errorf("MatchVars(%+v) = %v, want %v", tc.m, got, tc.want)
		}
	}
	// AND across multiple predicates.
	if !MatchVars(vars, []VarMatch{{"amount", "gte", 1000.0}, {"region", "eq", "EU"}}) {
		t.Error("combined predicate should match")
	}
	if MatchVars(vars, []VarMatch{{"amount", "gte", 1000.0}, {"region", "eq", "US"}}) {
		t.Error("combined predicate should fail on region")
	}
}

func TestListInstancesQueryAndPagination(t *testing.T) {
	m := NewMemory()
	base := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		region := "EU"
		if i%2 == 0 {
			region = "US"
		}
		id := "inst_" + string(rune('a'+i/10)) + string(rune('0'+i%10))
		_ = m.PutInstance(&Instance{
			ID: id, TenantID: "acme", DefinitionKey: "order", State: InstanceActive,
			Variables: map[string]any{"amount": float64(i * 100), "region": region},
			Tokens:    map[string]*Token{}, StartedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	// Other tenant must be invisible.
	_ = m.PutInstance(&Instance{ID: "inst_z9", TenantID: "other", DefinitionKey: "order",
		State: InstanceActive, Variables: map[string]any{"amount": 999999.0}, Tokens: map[string]*Token{}})

	// Variable range query, tenant-scoped.
	got, _ := m.ListInstances(InstanceFilter{TenantID: "acme", Vars: []VarMatch{{"amount", "gte", 2000.0}}})
	for _, in := range got {
		if in.TenantID != "acme" {
			t.Fatalf("tenant leak: %s", in.ID)
		}
		if in.Variables["amount"].(float64) < 2000 {
			t.Fatalf("amount filter leaked: %v", in.Variables["amount"])
		}
	}
	if len(got) != 5 { // amount=i*100 >= 2000 ⇒ i in 20..24
		t.Fatalf("amount>=2000 matched %d, want 5", len(got))
	}

	eu, _ := m.ListInstances(InstanceFilter{TenantID: "acme", Vars: []VarMatch{{"region", "eq", "EU"}}})
	if len(eu) == 0 {
		t.Fatal("region filter returned nothing")
	}
	for _, in := range eu {
		if in.Variables["region"] != "EU" {
			t.Fatalf("region leak: %v", in.Variables["region"])
		}
	}

	// Keyset pagination walks the whole tenant set with no dupes/gaps.
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		pg, _ := m.ListInstances(InstanceFilter{TenantID: "acme", Cursor: cursor, Limit: 7})
		if len(pg) == 0 {
			break
		}
		for _, in := range pg {
			if seen[in.ID] {
				t.Fatalf("duplicate across pages: %s", in.ID)
			}
			seen[in.ID] = true
		}
		cursor = pg[len(pg)-1].ID
		pages++
		if len(pg) < 7 {
			break
		}
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 25 {
		t.Fatalf("paginated %d instances, want 25", len(seen))
	}

	// Descending order.
	desc, _ := m.ListInstances(InstanceFilter{TenantID: "acme", Desc: true, Limit: 3})
	if len(desc) != 3 || !(desc[0].ID > desc[1].ID && desc[1].ID > desc[2].ID) {
		t.Fatalf("descending order broken: %v", []string{desc[0].ID, desc[1].ID, desc[2].ID})
	}

	// Time window.
	win, _ := m.ListInstances(InstanceFilter{TenantID: "acme",
		StartedAfter: base.Add(5 * time.Minute), StartedBefore: base.Add(9 * time.Minute)})
	if len(win) != 5 { // minutes 5,6,7,8,9 inclusive
		t.Fatalf("time window = %d, want 5", len(win))
	}
}

func TestListTasksVarFilterJoinsInstance(t *testing.T) {
	m := NewMemory()
	_ = m.PutInstance(&Instance{ID: "i1", TenantID: "t", DefinitionKey: "p", State: InstanceActive,
		Variables: map[string]any{"amount": 5000.0}, Tokens: map[string]*Token{}})
	_ = m.PutInstance(&Instance{ID: "i2", TenantID: "t", DefinitionKey: "p", State: InstanceActive,
		Variables: map[string]any{"amount": 10.0}, Tokens: map[string]*Token{}})
	_ = m.PutTask(&Task{ID: "tk1", TenantID: "t", InstanceID: "i1", State: TaskCreated, CreatedAt: time.Now()})
	_ = m.PutTask(&Task{ID: "tk2", TenantID: "t", InstanceID: "i2", State: TaskCreated, CreatedAt: time.Now()})

	got, _ := m.ListTasks(TaskFilter{TenantID: "t", Vars: []VarMatch{{"amount", "gte", 1000.0}}})
	if len(got) != 1 || got[0].ID != "tk1" {
		t.Fatalf("task var-join filter = %+v", got)
	}
}
