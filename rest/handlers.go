package rest

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"time"

	"github.com/olbboy/fliable/store"
)

// ---- health & metrics --------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.json(w, http.StatusOK, map[string]any{
		"status": "ok",
		"uptime": time.Since(s.started).Round(time.Second).String(),
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	s.json(w, http.StatusOK, map[string]any{
		"engine":     s.e.Metrics(),
		"goroutines": runtime.NumGoroutine(),
		"heapBytes":  mem.HeapAlloc,
		"uptime":     time.Since(s.started).Round(time.Second).String(),
	})
}

// handleMetrics emits Prometheus text exposition format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m := s.e.Metrics()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	counters := []struct {
		name, help string
		value      int64
	}{
		{"fliable_definitions_deployed_total", "Process definitions deployed", m.DefinitionsDeployed},
		{"fliable_instances_started_total", "Process instances started", m.InstancesStarted},
		{"fliable_instances_completed_total", "Process instances completed", m.InstancesCompleted},
		{"fliable_instances_terminated_total", "Process instances terminated", m.InstancesTerminated},
		{"fliable_tasks_created_total", "User tasks created", m.TasksCreated},
		{"fliable_tasks_completed_total", "User tasks completed", m.TasksCompleted},
		{"fliable_service_tasks_total", "Service task handler executions", m.ServiceTasksRun},
		{"fliable_external_tasks_created_total", "External worker tasks created", m.ExternalCreated},
		{"fliable_external_tasks_completed_total", "External worker tasks completed", m.ExternalCompleted},
		{"fliable_timers_fired_total", "Timer events fired", m.TimersFired},
		{"fliable_jobs_executed_total", "Background jobs executed", m.JobsExecuted},
		{"fliable_messages_correlated_total", "Messages correlated", m.MessagesCorrelated},
		{"fliable_decisions_evaluated_total", "DMN decisions evaluated", m.DecisionsEvaluated},
		{"fliable_incidents_total", "Incidents created", m.IncidentsCreated},
	}
	for _, c := range counters {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", c.name, c.help, c.name, c.name, c.value)
	}
}

// ---- definitions ---------------------------------------------------------------

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	xml, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	r.Body.Close()
	if err != nil || len(xml) == 0 {
		s.error(w, http.StatusBadRequest, errors.New("request body must be BPMN XML"))
		return
	}
	def, err := s.e.DeployTenant(tenantOf(r), xml, r.URL.Query().Get("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusCreated, defView(def))
}

func (s *Server) handleListDefinitions(w http.ResponseWriter, r *http.Request) {
	latest := r.URL.Query().Get("latest") != "false"
	defs, err := s.e.Store().ListDefinitions(latest)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]any, len(defs))
	for i, d := range defs {
		out[i] = defView(d)
	}
	s.json(w, http.StatusOK, out)
}

func (s *Server) handleGetDefinition(w http.ResponseWriter, r *http.Request) {
	def, err := s.e.Store().GetDefinition(r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, defView(def))
}

func (s *Server) handleGetDefinitionXML(w http.ResponseWriter, r *http.Request) {
	def, err := s.e.Store().GetDefinition(r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write(def.XML)
}

func defView(d *store.Definition) map[string]any {
	return map[string]any{
		"id": d.ID, "key": d.Key, "version": d.Version,
		"name": d.Name, "deployedAt": d.DeployedAt,
	}
}

// ---- instances ------------------------------------------------------------------

type startInstanceReq struct {
	DefinitionKey string         `json:"definitionKey"`
	DefinitionID  string         `json:"definitionId"`
	BusinessKey   string         `json:"businessKey"`
	Variables     map[string]any `json:"variables"`
}

func (s *Server) handleStartInstance(w http.ResponseWriter, r *http.Request) {
	var req startInstanceReq
	if err := decodeJSON(r, &req); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	// Ride the trace context into the instance so workers can continue it.
	if tr := traceFrom(r.Context()); tr.Traceparent != "" {
		if req.Variables == nil {
			req.Variables = map[string]any{}
		}
		if _, set := req.Variables[traceparentVar]; !set {
			req.Variables[traceparentVar] = tr.Traceparent
		}
	}

	var inst *store.Instance
	var err error
	switch {
	case req.DefinitionID != "":
		inst, err = s.e.StartInstanceByDefinition(req.DefinitionID, req.BusinessKey, req.Variables)
	case req.DefinitionKey != "":
		inst, err = s.e.StartInstanceTenant(tenantOf(r), req.DefinitionKey, req.BusinessKey, req.Variables)
	default:
		s.error(w, http.StatusBadRequest, errors.New("definitionKey or definitionId required"))
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusCreated, inst)
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := pageLimit(r)
	insts, err := s.e.ListInstances(store.InstanceFilter{
		TenantID:      tenantOf(r),
		DefinitionKey: q.Get("definitionKey"),
		DefinitionID:  q.Get("definitionId"),
		BusinessKey:   q.Get("businessKey"),
		State:         store.InstanceState(q.Get("state")),
		ParentID:      q.Get("parentId"),
		Vars:          parseVarMatches(r),
		StartedAfter:  queryTime(r, "startedAfter"),
		StartedBefore: queryTime(r, "startedBefore"),
		EndedAfter:    queryTime(r, "endedAfter"),
		EndedBefore:   queryTime(r, "endedBefore"),
		Cursor:        q.Get("cursor"),
		Desc:          queryBool(r, "desc"),
		Limit:         limit,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	last := ""
	if n := len(insts); n > 0 {
		last = insts[n-1].ID
	}
	s.writePage(w, insts, len(insts), limit, last)
}

func (s *Server) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	inst, err := s.e.GetInstance(r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, inst)
}

func (s *Server) handleCancelInstance(w http.ResponseWriter, r *http.Request) {
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "cancelled via API"
	}
	if err := s.e.CancelInstance(r.PathValue("id"), reason); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]string{"status": "terminated"})
}

func (s *Server) handleSetVariables(w http.ResponseWriter, r *http.Request) {
	var vars map[string]any
	if err := decodeJSON(r, &vars); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	if err := s.e.SetVariables(r.PathValue("id"), vars); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	evs, err := s.e.History(r.PathValue("id"), store.HistoryFilter{
		Type:     r.URL.Query().Get("type"),
		AfterSeq: int64(queryInt(r, "afterSeq", 0)),
		Limit:    queryInt(r, "limit", 1000),
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, evs)
}

func (s *Server) handleInstanceIncidents(w http.ResponseWriter, r *http.Request) {
	incs, err := s.e.Store().ListIncidents(store.IncidentFilter{InstanceID: r.PathValue("id")})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, incs)
}

// ---- tasks -------------------------------------------------------------------------

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := store.TaskState(q.Get("state"))
	if q.Get("state") == "" {
		state = store.TaskCreated
	}
	limit := pageLimit(r)
	tasks, err := s.e.ListTasks(store.TaskFilter{
		TenantID:       tenantOf(r),
		InstanceID:     q.Get("instanceId"),
		Assignee:       q.Get("assignee"),
		Unassigned:     queryBool(r, "unassigned"),
		CandidateUser:  q.Get("candidateUser"),
		CandidateGroup: q.Get("candidateGroup"),
		DefinitionKey:  q.Get("definitionKey"),
		ElementID:      q.Get("elementId"),
		State:          state,
		Vars:           parseVarMatches(r),
		CreatedAfter:   queryTime(r, "createdAfter"),
		CreatedBefore:  queryTime(r, "createdBefore"),
		DueBefore:      queryTime(r, "dueBefore"),
		Cursor:         q.Get("cursor"),
		Desc:           queryBool(r, "desc"),
		Limit:          limit,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	last := ""
	if n := len(tasks); n > 0 {
		last = tasks[n-1].ID
	}
	s.writePage(w, tasks, len(tasks), limit, last)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.e.Store().GetTask(r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, t)
}

type taskActionReq struct {
	User      string         `json:"user"`
	Variables map[string]any `json:"variables"`
}

func (s *Server) handleClaimTask(w http.ResponseWriter, r *http.Request) {
	var req taskActionReq
	if err := decodeJSON(r, &req); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	if err := s.e.ClaimTask(r.PathValue("id"), req.User); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]string{"status": "claimed"})
}

func (s *Server) handleCompleteTask(w http.ResponseWriter, r *http.Request) {
	var req taskActionReq
	if err := decodeJSON(r, &req); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	if err := s.e.CompleteTask(r.PathValue("id"), req.Variables, req.User); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]string{"status": "completed"})
}

// ---- messages & signals ----------------------------------------------------------------

type messageReq struct {
	Name           string         `json:"name"`
	CorrelationKey string         `json:"correlationKey"`
	Variables      map[string]any `json:"variables"`
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	var req messageReq
	if err := decodeJSON(r, &req); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		s.error(w, http.StatusBadRequest, errors.New("name required"))
		return
	}
	n, err := s.e.CorrelateMessage(req.Name, req.CorrelationKey, req.Variables)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"activated": n})
}

func (s *Server) handleSignal(w http.ResponseWriter, r *http.Request) {
	var req messageReq
	if err := decodeJSON(r, &req); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		s.error(w, http.StatusBadRequest, errors.New("name required"))
		return
	}
	n, err := s.e.BroadcastSignal(req.Name, req.Variables)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"activated": n})
}

// ---- external tasks ------------------------------------------------------------------------

type fetchExternalReq struct {
	Topic        string `json:"topic"`
	WorkerID     string `json:"workerId"`
	MaxTasks     int    `json:"maxTasks"`
	LockDuration string `json:"lockDuration"` // Go duration, default 5m
}

func (s *Server) handleFetchExternal(w http.ResponseWriter, r *http.Request) {
	var req fetchExternalReq
	if err := decodeJSON(r, &req); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	if req.Topic == "" || req.WorkerID == "" {
		s.error(w, http.StatusBadRequest, errors.New("topic and workerId required"))
		return
	}
	lock := 5 * time.Minute
	if req.LockDuration != "" {
		d, err := time.ParseDuration(req.LockDuration)
		if err != nil {
			s.error(w, http.StatusBadRequest, fmt.Errorf("bad lockDuration: %w", err))
			return
		}
		lock = d
	}
	if req.MaxTasks <= 0 {
		req.MaxTasks = 10
	}
	tasks, err := s.e.FetchExternalTasks(req.Topic, req.WorkerID, lock, req.MaxTasks)
	if err != nil {
		s.fail(w, err)
		return
	}
	if tasks == nil {
		tasks = []*store.ExternalTask{}
	}
	s.json(w, http.StatusOK, tasks)
}

type externalActionReq struct {
	WorkerID  string         `json:"workerId"`
	Variables map[string]any `json:"variables"`
	Message   string         `json:"message"`
	ErrorCode string         `json:"errorCode"`
}

func (s *Server) handleCompleteExternal(w http.ResponseWriter, r *http.Request) {
	var req externalActionReq
	if err := decodeJSON(r, &req); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	if err := s.e.CompleteExternalTask(r.PathValue("id"), req.WorkerID, req.Variables); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]string{"status": "completed"})
}

func (s *Server) handleFailExternal(w http.ResponseWriter, r *http.Request) {
	var req externalActionReq
	if err := decodeJSON(r, &req); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	if err := s.e.FailExternalTask(r.PathValue("id"), req.WorkerID, req.Message, req.ErrorCode); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]string{"status": "failed"})
}

// ---- incidents --------------------------------------------------------------------------------

func (s *Server) handleListIncidents(w http.ResponseWriter, r *http.Request) {
	f := store.IncidentFilter{Limit: queryInt(r, "limit", 200)}
	if v := r.URL.Query().Get("resolved"); v != "" {
		b := v == "true"
		f.Resolved = &b
	}
	incs, err := s.e.Store().ListIncidents(f)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, incs)
}

func (s *Server) handleResolveIncident(w http.ResponseWriter, r *http.Request) {
	if err := s.e.ResolveIncident(r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]string{"status": "resolved"})
}

// ---- decisions -----------------------------------------------------------------------------------

func (s *Server) handleDeployDecision(w http.ResponseWriter, r *http.Request) {
	if s.decisions == nil {
		s.error(w, http.StatusNotImplemented, errors.New("decision registry not configured"))
		return
	}
	xml, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	r.Body.Close()
	if err != nil || len(xml) == 0 {
		s.error(w, http.StatusBadRequest, errors.New("request body must be DMN XML"))
		return
	}
	ds, err := s.decisions.RegisterXML(xml)
	if err != nil {
		s.fail(w, err)
		return
	}
	ids := make([]string, len(ds))
	for i, d := range ds {
		ids[i] = d.ID
	}
	s.json(w, http.StatusCreated, map[string]any{"decisions": ids})
}

func (s *Server) handleListDecisions(w http.ResponseWriter, r *http.Request) {
	if s.decisions == nil {
		s.json(w, http.StatusOK, []string{})
		return
	}
	s.json(w, http.StatusOK, s.decisions.List())
}
