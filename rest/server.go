// Package rest serves the Fliable engine over HTTP: deployment, instance
// lifecycle, task management, messages/signals, external worker tasks,
// incidents, history, live server-sent events, health and Prometheus
// metrics. Zero dependencies — pure net/http.
package rest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/olbboy/fliable/dmn"
	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
)

// Option configures the server.
type Option func(*Server)

// WithAPIKey requires the X-Api-Key header on every request except
// /healthz.
func WithAPIKey(key string) Option {
	return func(s *Server) { s.apiKey = key }
}

// WithLogger sets the request logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) { s.log = l }
}

// WithDecisions exposes DMN deployment endpoints backed by the registry.
func WithDecisions(r *dmn.Registry) Option {
	return func(s *Server) { s.decisions = r }
}

// Server is the HTTP front end of a Fliable engine.
type Server struct {
	e         *engine.Engine
	decisions *dmn.Registry
	mux       *http.ServeMux
	log       *slog.Logger
	apiKey    string

	sseMu   sync.Mutex
	sseSubs map[chan *store.HistoryEvent]struct{}
	started time.Time
}

// New builds the HTTP handler for an engine.
func New(e *engine.Engine, opts ...Option) *Server {
	s := &Server{
		e:       e,
		mux:     http.NewServeMux(),
		log:     slog.Default(),
		sseSubs: map[chan *store.HistoryEvent]struct{}{},
		started: time.Now(),
	}
	for _, o := range opts {
		o(s)
	}
	s.routes()
	e.OnEvent(s.fanout)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.apiKey != "" && r.URL.Path != "/healthz" {
		if r.Header.Get("X-Api-Key") != s.apiKey {
			s.error(w, http.StatusUnauthorized, errors.New("missing or invalid X-Api-Key"))
			return
		}
	}
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("panic in handler", "path", r.URL.Path, "panic", rec)
			s.error(w, http.StatusInternalServerError, fmt.Errorf("internal error"))
		}
	}()
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /v1/stats", s.handleStats)

	s.mux.HandleFunc("POST /v1/definitions", s.handleDeploy)
	s.mux.HandleFunc("GET /v1/definitions", s.handleListDefinitions)
	s.mux.HandleFunc("GET /v1/definitions/{id}", s.handleGetDefinition)
	s.mux.HandleFunc("GET /v1/definitions/{id}/xml", s.handleGetDefinitionXML)

	s.mux.HandleFunc("POST /v1/instances", s.handleStartInstance)
	s.mux.HandleFunc("GET /v1/instances", s.handleListInstances)
	s.mux.HandleFunc("GET /v1/instances/{id}", s.handleGetInstance)
	s.mux.HandleFunc("DELETE /v1/instances/{id}", s.handleCancelInstance)
	s.mux.HandleFunc("PUT /v1/instances/{id}/variables", s.handleSetVariables)
	s.mux.HandleFunc("GET /v1/instances/{id}/history", s.handleHistory)
	s.mux.HandleFunc("GET /v1/instances/{id}/incidents", s.handleInstanceIncidents)

	s.mux.HandleFunc("GET /v1/tasks", s.handleListTasks)
	s.mux.HandleFunc("GET /v1/tasks/{id}", s.handleGetTask)
	s.mux.HandleFunc("POST /v1/tasks/{id}/claim", s.handleClaimTask)
	s.mux.HandleFunc("POST /v1/tasks/{id}/complete", s.handleCompleteTask)

	s.mux.HandleFunc("POST /v1/messages", s.handleMessage)
	s.mux.HandleFunc("POST /v1/signals", s.handleSignal)

	s.mux.HandleFunc("POST /v1/external-tasks/fetch", s.handleFetchExternal)
	s.mux.HandleFunc("POST /v1/external-tasks/{id}/complete", s.handleCompleteExternal)
	s.mux.HandleFunc("POST /v1/external-tasks/{id}/fail", s.handleFailExternal)

	s.mux.HandleFunc("GET /v1/incidents", s.handleListIncidents)
	s.mux.HandleFunc("POST /v1/incidents/{id}/resolve", s.handleResolveIncident)

	s.mux.HandleFunc("GET /v1/events", s.handleSSE)

	s.mux.HandleFunc("POST /v1/decisions", s.handleDeployDecision)
	s.mux.HandleFunc("GET /v1/decisions", s.handleListDecisions)
}

// ---- plumbing --------------------------------------------------------------

func (s *Server) json(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

type apiError struct {
	Error string `json:"error"`
}

func (s *Server) error(w http.ResponseWriter, code int, err error) {
	s.json(w, code, apiError{Error: err.Error()})
}

// fail maps engine/store errors onto HTTP status codes.
func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.error(w, http.StatusNotFound, err)
	case strings.Contains(err.Error(), "bpmn:"), strings.Contains(err.Error(), "dmn:"), strings.Contains(err.Error(), "expr:"):
		s.error(w, http.StatusUnprocessableEntity, err)
	case strings.Contains(err.Error(), "is completed"), strings.Contains(err.Error(), "is terminated"),
		strings.Contains(err.Error(), "already"), strings.Contains(err.Error(), "locked by"),
		strings.Contains(err.Error(), "no longer active"):
		s.error(w, http.StatusConflict, err)
	default:
		s.error(w, http.StatusBadRequest, err)
	}
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func queryInt(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
