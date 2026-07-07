package store

import (
	"sort"
	"strings"
	"time"
)

// sortByID orders a slice by a string key (typically the time-sortable ID)
// ascending, or descending when desc is set.
func sortByID[T any](s []T, id func(T) string, desc bool) {
	sort.Slice(s, func(i, j int) bool {
		if desc {
			return id(s[i]) > id(s[j])
		}
		return id(s[i]) < id(s[j])
	})
}

// VarMatch is a predicate over one process/instance variable, used by
// ListInstances and ListTasks to query by business data (e.g. "amount >=
// 1000", "region = 'EU'", "approved exists"). Stored numbers are float64
// and JSON query values decode to float64, so numeric comparisons line up
// without coercion surprises.
type VarMatch struct {
	Name  string `json:"name"`
	Op    string `json:"op"`    // eq, ne, lt, lte, gt, gte, contains, exists
	Value any    `json:"value"` // ignored for exists
}

// MatchVars reports whether vars satisfies every predicate (AND).
func MatchVars(vars map[string]any, matches []VarMatch) bool {
	for _, m := range matches {
		if !matchOne(vars[m.Name], m) {
			return false
		}
	}
	return true
}

func matchOne(v any, m VarMatch) bool {
	switch m.Op {
	case "exists", "":
		if m.Op == "" {
			return looseEqual(v, m.Value)
		}
		return v != nil
	case "eq":
		return looseEqual(v, m.Value)
	case "ne":
		return !looseEqual(v, m.Value)
	case "contains":
		s, ok := v.(string)
		sub, ok2 := m.Value.(string)
		return ok && ok2 && strings.Contains(s, sub)
	case "lt", "lte", "gt", "gte":
		return compareOrder(m.Op, v, m.Value)
	}
	return false
}

func looseEqual(a, b any) bool {
	if af, ok := toF(a); ok {
		if bf, ok := toF(b); ok {
			return af == bf
		}
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	}
	return false
}

func compareOrder(op string, a, b any) bool {
	if af, ok := toF(a); ok {
		if bf, ok := toF(b); ok {
			return orderResult(op, af > bf, af == bf)
		}
	}
	if as, ok := a.(string); ok {
		if bs, ok := b.(string); ok {
			return orderResult(op, as > bs, as == bs)
		}
	}
	return false
}

func orderResult(op string, gt, eq bool) bool {
	switch op {
	case "lt":
		return !gt && !eq
	case "lte":
		return !gt
	case "gt":
		return gt
	case "gte":
		return gt || eq
	}
	return false
}

func toF(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	}
	return 0, false
}

// matchInstance reports whether an instance satisfies every set field of
// the filter (cursor and limit are handled by the caller's pagination).
func matchInstance(f InstanceFilter, inst *Instance) bool {
	if f.TenantID != "" && inst.TenantID != f.TenantID {
		return false
	}
	if f.DefinitionKey != "" && inst.DefinitionKey != f.DefinitionKey {
		return false
	}
	if f.DefinitionID != "" && inst.DefinitionID != f.DefinitionID {
		return false
	}
	if f.BusinessKey != "" && inst.BusinessKey != f.BusinessKey {
		return false
	}
	if f.State != "" && inst.State != f.State {
		return false
	}
	if f.ParentID != "" && inst.ParentID != f.ParentID {
		return false
	}
	if !inTimeWindow(inst.StartedAt, f.StartedAfter, f.StartedBefore) {
		return false
	}
	if (!f.EndedAfter.IsZero() || !f.EndedBefore.IsZero()) && !inTimeWindow(inst.EndedAt, f.EndedAfter, f.EndedBefore) {
		return false
	}
	if len(f.Vars) > 0 && !MatchVars(inst.Variables, f.Vars) {
		return false
	}
	return afterCursor(inst.ID, f.Cursor, f.Desc)
}

// matchTask reports whether a task satisfies every set field of the filter.
// instVars are the task's instance variables, needed only when f.Vars is
// set (pass nil otherwise); a nil map with Vars set never matches.
func matchTask(f TaskFilter, t *Task, instVars func() map[string]any) bool {
	if f.TenantID != "" && t.TenantID != f.TenantID {
		return false
	}
	if f.InstanceID != "" && t.InstanceID != f.InstanceID {
		return false
	}
	if f.Assignee != "" && t.Assignee != f.Assignee {
		return false
	}
	if f.Unassigned && t.Assignee != "" {
		return false
	}
	if f.CandidateUser != "" && !containsStr(t.CandidateUsers, f.CandidateUser) {
		return false
	}
	if f.CandidateGroup != "" && !containsStr(t.CandidateGroups, f.CandidateGroup) {
		return false
	}
	if f.State != "" && t.State != f.State {
		return false
	}
	if f.DefinitionKey != "" && t.DefinitionKey != f.DefinitionKey {
		return false
	}
	if f.ElementID != "" && t.ElementID != f.ElementID {
		return false
	}
	if !inTimeWindow(t.CreatedAt, f.CreatedAfter, f.CreatedBefore) {
		return false
	}
	if !f.DueBefore.IsZero() && (t.DueAt.IsZero() || t.DueAt.After(f.DueBefore)) {
		return false
	}
	if len(f.Vars) > 0 {
		vars := instVars()
		if vars == nil || !MatchVars(vars, f.Vars) {
			return false
		}
	}
	return afterCursor(t.ID, f.Cursor, f.Desc)
}

// afterCursor reports whether id sorts after the cursor for the requested
// direction. An empty cursor accepts everything (page one).
func afterCursor(id, cursor string, desc bool) bool {
	if cursor == "" {
		return true
	}
	if desc {
		return id < cursor
	}
	return id > cursor
}

// inTimeWindow reports whether t falls within [after, before]. Zero bounds
// are open.
func inTimeWindow(t, after, before time.Time) bool {
	if !after.IsZero() && t.Before(after) {
		return false
	}
	if !before.IsZero() && t.After(before) {
		return false
	}
	return true
}
