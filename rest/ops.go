package rest

import (
	"errors"
	"net/http"

	"github.com/olbboy/fliable/engine"
)

func (s *Server) handleSuspendInstance(w http.ResponseWriter, r *http.Request) {
	if err := s.e.SuspendInstance(r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]string{"status": "suspended"})
}

func (s *Server) handleResumeInstance(w http.ResponseWriter, r *http.Request) {
	if err := s.e.ResumeInstance(r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]string{"status": "resumed"})
}

// handleMigrateInstance validates (and unless dryRun, applies) a live
// migration of one instance onto another definition version.
func (s *Server) handleMigrateInstance(w http.ResponseWriter, r *http.Request) {
	var plan engine.MigrationPlan
	if err := decodeJSON(r, &plan); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	report, err := s.e.MigrateInstance(r.PathValue("id"), plan)
	if err != nil {
		s.fail(w, err)
		return
	}
	code := http.StatusOK
	if !report.Applied && !plan.DryRun {
		code = http.StatusUnprocessableEntity // validation issues
	}
	s.json(w, code, report)
}

// batchReq is the body for POST /v1/batch/{op}.
type batchReq struct {
	IDs    []string `json:"ids"`
	Reason string   `json:"reason"`
}

type batchResult struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// handleBatch applies one operation to many IDs, returning a per-ID
// outcome so a partial failure never loses the successes. Operations:
// cancel/suspend/resume (instance IDs), resolve-incidents (incident IDs).
func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	op := r.PathValue("op")
	var req batchReq
	if err := decodeJSON(r, &req); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	if len(req.IDs) == 0 {
		s.error(w, http.StatusBadRequest, errors.New("ids required"))
		return
	}
	if len(req.IDs) > 1000 {
		s.error(w, http.StatusBadRequest, errors.New("batch limited to 1000 ids"))
		return
	}

	var apply func(id string) error
	switch op {
	case "cancel":
		reason := req.Reason
		if reason == "" {
			reason = "batch cancel"
		}
		apply = func(id string) error { return s.e.CancelInstance(id, reason) }
	case "suspend":
		apply = s.e.SuspendInstance
	case "resume":
		apply = s.e.ResumeInstance
	case "resolve-incidents":
		apply = s.e.ResolveIncident
	default:
		s.error(w, http.StatusBadRequest, errors.New("unknown batch op: "+op))
		return
	}

	results := make([]batchResult, 0, len(req.IDs))
	ok, failed := 0, 0
	for _, id := range req.IDs {
		if err := apply(id); err != nil {
			results = append(results, batchResult{ID: id, OK: false, Error: err.Error()})
			failed++
			continue
		}
		results = append(results, batchResult{ID: id, OK: true})
		ok++
	}
	s.json(w, http.StatusOK, map[string]any{
		"op": op, "succeeded": ok, "failed": failed, "results": results,
	})
}
