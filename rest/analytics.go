package rest

import (
	"net/http"
	"sort"
	"time"

	"github.com/olbboy/fliable/store"
)

// ProcessStats is one process definition's operational profile, computed
// from the event-sourced instance records — cycle times, throughput and
// incident pressure without an external analytics stack.
type ProcessStats struct {
	DefinitionKey string `json:"definitionKey"`
	Active        int    `json:"active"`
	Completed     int    `json:"completed"`
	Terminated    int    `json:"terminated"`
	Suspended     int    `json:"suspended"`
	OpenIncidents int    `json:"openIncidents"`
	OpenTasks     int    `json:"openTasks"`

	// Cycle times over completed instances, in milliseconds.
	AvgDurationMs float64 `json:"avgDurationMs,omitempty"`
	P50DurationMs float64 `json:"p50DurationMs,omitempty"`
	P95DurationMs float64 `json:"p95DurationMs,omitempty"`
	// CompletedLast24h is a simple throughput signal.
	CompletedLast24h int `json:"completedLast24h"`
}

func (s *Server) registerAnalyticsRoutes() {
	s.route("GET /v1/analytics/processes", RoleViewer, s.handleProcessAnalytics)
}

func (s *Server) handleProcessAnalytics(w http.ResponseWriter, r *http.Request) {
	tenant := tenantOf(r)
	insts, err := s.e.ListInstances(store.InstanceFilter{TenantID: tenant})
	if err != nil {
		s.fail(w, err)
		return
	}
	byKey := map[string]*ProcessStats{}
	durs := map[string][]float64{}
	dayAgo := time.Now().Add(-24 * time.Hour)
	instKey := map[string]string{}

	for _, in := range insts {
		st := byKey[in.DefinitionKey]
		if st == nil {
			st = &ProcessStats{DefinitionKey: in.DefinitionKey}
			byKey[in.DefinitionKey] = st
		}
		instKey[in.ID] = in.DefinitionKey
		switch in.State {
		case store.InstanceActive:
			st.Active++
			if in.Suspended {
				st.Suspended++
			}
		case store.InstanceCompleted:
			st.Completed++
			d := in.EndedAt.Sub(in.StartedAt)
			durs[in.DefinitionKey] = append(durs[in.DefinitionKey], float64(d.Milliseconds()))
			if in.EndedAt.After(dayAgo) {
				st.CompletedLast24h++
			}
		case store.InstanceTerminated:
			st.Terminated++
		}
	}

	unresolved := false
	if incs, err := s.e.Store().ListIncidents(store.IncidentFilter{Resolved: &unresolved}); err == nil {
		for _, inc := range incs {
			if key, ok := instKey[inc.InstanceID]; ok {
				byKey[key].OpenIncidents++
			}
		}
	}
	if tasks, err := s.e.ListTasks(store.TaskFilter{TenantID: tenant, State: store.TaskCreated}); err == nil {
		for _, t := range tasks {
			if st, ok := byKey[t.DefinitionKey]; ok {
				st.OpenTasks++
			}
		}
	}

	out := make([]*ProcessStats, 0, len(byKey))
	for key, st := range byKey {
		if ds := durs[key]; len(ds) > 0 {
			sort.Float64s(ds)
			var sum float64
			for _, d := range ds {
				sum += d
			}
			st.AvgDurationMs = sum / float64(len(ds))
			st.P50DurationMs = percentile(ds, 0.50)
			st.P95DurationMs = percentile(ds, 0.95)
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DefinitionKey < out[j].DefinitionKey })
	s.json(w, http.StatusOK, out)
}

// percentile over a sorted slice (nearest-rank).
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p*float64(len(sorted))+0.5) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
