package rest

import (
	"errors"
	"net/http"
	"time"

	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
)

// registerAgentRoutes wires the AI agent worker endpoints: an external AI
// assistant polls for agent jobs, runs the model + tools with its own
// provider, and reports the structured result back. This is how an AI
// agent integrates with Fliable in production without the engine ever
// vendoring a model provider.
func (s *Server) registerAgentRoutes() {
	s.route("POST /v1/agent-jobs/fetch", RoleOperator, s.handleFetchAgentJobs)
	s.route("POST /v1/agent-jobs/{id}/complete", RoleOperator, s.handleCompleteAgentJob)
	s.route("POST /v1/agent-jobs/{id}/fail", RoleOperator, s.handleFailAgentJob)
	s.route("GET /v1/agent-jobs", RoleViewer, s.handleListAgentJobs)
}

type fetchAgentReq struct {
	Topic        string `json:"topic"`
	WorkerID     string `json:"workerId"`
	MaxJobs      int    `json:"maxJobs"`
	LockDuration string `json:"lockDuration"`
}

func (s *Server) handleFetchAgentJobs(w http.ResponseWriter, r *http.Request) {
	var req fetchAgentReq
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
			s.error(w, http.StatusBadRequest, err)
			return
		}
		lock = d
	}
	if req.MaxJobs <= 0 {
		req.MaxJobs = 5
	}
	jobs, err := s.e.FetchAgentJobs(req.Topic, req.WorkerID, lock, req.MaxJobs)
	if err != nil {
		s.fail(w, err)
		return
	}
	if jobs == nil {
		jobs = []*store.AgentJob{}
	}
	s.json(w, http.StatusOK, jobs)
}

type completeAgentReq struct {
	WorkerID  string                 `json:"workerId"`
	Output    map[string]any         `json:"output"`
	Text      string                 `json:"text"`
	ToolCalls []engine.AgentToolCall `json:"toolCalls"`
	Usage     engine.AgentUsage      `json:"usage"`
}

func (s *Server) handleCompleteAgentJob(w http.ResponseWriter, r *http.Request) {
	var req completeAgentReq
	if err := decodeJSON(r, &req); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	if err := s.e.CompleteAgentJob(r.PathValue("id"), req.WorkerID, req.Output, req.Text, req.ToolCalls, req.Usage); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]string{"status": "completed"})
}

type failAgentReq struct {
	WorkerID  string `json:"workerId"`
	Message   string `json:"message"`
	ErrorCode string `json:"errorCode"`
}

func (s *Server) handleFailAgentJob(w http.ResponseWriter, r *http.Request) {
	var req failAgentReq
	if err := decodeJSON(r, &req); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	if err := s.e.FailAgentJob(r.PathValue("id"), req.WorkerID, req.Message, req.ErrorCode); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]string{"status": "failed"})
}

func (s *Server) handleListAgentJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.e.Store().ListAgentJobs(r.URL.Query().Get("instanceId"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if jobs == nil {
		jobs = []*store.AgentJob{}
	}
	s.json(w, http.StatusOK, jobs)
}
