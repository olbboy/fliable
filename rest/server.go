// Package rest serves the Fliable engine over HTTP: deployment, instance
// lifecycle, task management, messages/signals, external worker tasks,
// incidents, history, live server-sent events, health and Prometheus
// metrics. Zero dependencies — pure net/http.
package rest

import (
	"context"
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
	"github.com/olbboy/fliable/form"
	"github.com/olbboy/fliable/store"
	"github.com/olbboy/fliable/vault"
)

// Option configures the server.
type Option func(*Server)

// WithAPIKey requires the X-Api-Key header, granting the admin role across
// all tenants. Sugar over WithAuth(APIKeyAuth(...)).
func WithAPIKey(key string) Option {
	return func(s *Server) { s.authenticators = append(s.authenticators, APIKeyAuth(key, RoleAdmin)) }
}

// WithSigningKey verifies HMAC bearer tokens minted by MintToken with the
// same key (roles + tenant travel inside the token).
func WithSigningKey(key []byte) Option {
	return func(s *Server) { s.authenticators = append(s.authenticators, SignedTokenAuth(key)) }
}

// WithAuth adds a custom authenticator to the chain (first non-nil
// principal wins).
func WithAuth(a Authenticator) Option {
	return func(s *Server) { s.authenticators = append(s.authenticators, a) }
}

// WithCORS enables cross-origin requests from browser SPAs.
func WithCORS(c *CORSConfig) Option {
	return func(s *Server) { s.cors = c }
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
	vault     *vault.Vault
	forms     *form.Registry
	mux       *http.ServeMux
	log       *slog.Logger

	authenticators []Authenticator
	auth           Authenticator
	cors           *CORSConfig

	sseMu   sync.Mutex
	sseSubs map[chan *store.HistoryEvent]struct{}
	started time.Time
	dedupe  *dedupeCache
}

// New builds the HTTP handler for an engine.
func New(e *engine.Engine, opts ...Option) *Server {
	s := &Server{
		e:       e,
		mux:     http.NewServeMux(),
		log:     slog.Default(),
		sseSubs: map[chan *store.HistoryEvent]struct{}{},
		started: time.Now(),
		dedupe:  newDedupeCache(),
	}
	for _, o := range opts {
		o(s)
	}
	if len(s.authenticators) > 0 {
		s.auth = ChainAuth(s.authenticators...)
	}
	s.routes()
	e.OnEvent(s.fanout)
	return s
}

// authRequired reports whether an authenticator is configured (otherwise
// the API is open — the embedded/dev default).
func (s *Server) authRequired() bool { return s.auth != nil }

// publicPath endpoints skip authentication entirely.
func publicPath(p string) bool {
	return p == "/healthz" || p == "/openapi.json" || p == "/docs"
}

// publicRequest additionally admits webhook event ingestion, which
// authenticates with the channel's own secret instead of a platform
// credential (external systems shouldn't hold API keys). Channel
// management (GET/PUT/DELETE) stays behind normal auth.
func publicRequest(r *http.Request) bool {
	if publicPath(r.URL.Path) {
		return true
	}
	if r.Method == http.MethodPost {
		if name, ok := strings.CutPrefix(r.URL.Path, "/v1/webhooks/"); ok && name != "" && !strings.Contains(name, "/") {
			return true
		}
	}
	return false
}

// ServeHTTP implements http.Handler: trace context, CORS, panic recovery,
// request logging, authentication and tenant confinement, then routing
// (per-route RBAC lives in guard).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tr := ensureTrace(r)
	w.Header().Set("X-Trace-Id", tr.TraceID)
	r = r.WithContext(context.WithValue(r.Context(), traceKey{}, tr))
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	w = sw
	begin := time.Now()
	defer func() {
		s.log.Info("request",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"dur", time.Since(begin).String(), "trace", tr.TraceID)
	}()

	if s.cors != nil {
		if s.cors.apply(w, r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("panic in handler", "path", r.URL.Path, "panic", rec, "trace", tr.TraceID)
			s.error(w, http.StatusInternalServerError, fmt.Errorf("internal error"))
		}
	}()

	if s.authRequired() && !publicRequest(r) {
		p, err := s.auth.Authenticate(r)
		if err != nil {
			s.error(w, http.StatusUnauthorized, err)
			return
		}
		if p == nil {
			s.error(w, http.StatusUnauthorized, errors.New("authentication required"))
			return
		}
		// Confine a tenant-scoped principal: its tenant overrides any
		// client-supplied X-Tenant-Id so it can never read another tenant.
		if p.Tenant != "" {
			r.Header.Set("X-Tenant-Id", p.Tenant)
		}
		r = r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
	}
	s.mux.ServeHTTP(w, r)
}

// route registers a handler requiring at least the given role (enforced
// only when authentication is configured).
func (s *Server) route(pattern string, min Role, h http.HandlerFunc) {
	s.mux.HandleFunc(pattern, s.guard(min, h))
}

// guard enforces the minimum role for a route using the request principal.
func (s *Server) guard(min Role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authRequired() {
			p := principalFrom(r.Context())
			if p == nil {
				s.error(w, http.StatusUnauthorized, errors.New("authentication required"))
				return
			}
			if !p.has(min) {
				s.error(w, http.StatusForbidden, fmt.Errorf("role %q required", min))
				return
			}
		}
		h(w, r)
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)
	s.mux.HandleFunc("GET /docs", s.handleDocs)
	s.route("GET /metrics", RoleViewer, s.handleMetrics)
	s.route("GET /v1/stats", RoleViewer, s.handleStats)

	s.route("POST /v1/definitions", RoleAdmin, s.handleDeploy)
	s.route("GET /v1/definitions", RoleViewer, s.handleListDefinitions)
	s.route("GET /v1/definitions/{id}", RoleViewer, s.handleGetDefinition)
	s.route("GET /v1/definitions/{id}/xml", RoleViewer, s.handleGetDefinitionXML)

	s.route("POST /v1/instances", RoleOperator, s.handleStartInstance)
	s.route("GET /v1/instances", RoleViewer, s.handleListInstances)
	s.route("GET /v1/instances/{id}", RoleViewer, s.handleGetInstance)
	s.route("DELETE /v1/instances/{id}", RoleOperator, s.handleCancelInstance)
	s.route("PUT /v1/instances/{id}/variables", RoleOperator, s.handleSetVariables)
	s.route("POST /v1/instances/{id}/suspend", RoleOperator, s.handleSuspendInstance)
	s.route("POST /v1/instances/{id}/resume", RoleOperator, s.handleResumeInstance)
	s.route("POST /v1/instances/{id}/migrate", RoleAdmin, s.handleMigrateInstance)
	s.route("GET /v1/instances/{id}/history", RoleViewer, s.handleHistory)
	s.route("GET /v1/instances/{id}/incidents", RoleViewer, s.handleInstanceIncidents)

	s.route("GET /v1/tasks", RoleViewer, s.handleListTasks)
	s.route("GET /v1/tasks/{id}", RoleViewer, s.handleGetTask)
	s.route("POST /v1/tasks/{id}/claim", RoleOperator, s.handleClaimTask)
	s.route("POST /v1/tasks/{id}/complete", RoleOperator, s.handleCompleteTask)

	s.route("POST /v1/messages", RoleOperator, s.handleMessage)
	s.route("POST /v1/signals", RoleOperator, s.handleSignal)

	s.route("POST /v1/external-tasks/fetch", RoleOperator, s.handleFetchExternal)
	s.route("POST /v1/external-tasks/{id}/complete", RoleOperator, s.handleCompleteExternal)
	s.route("POST /v1/external-tasks/{id}/fail", RoleOperator, s.handleFailExternal)

	s.route("GET /v1/incidents", RoleViewer, s.handleListIncidents)
	s.route("POST /v1/incidents/{id}/resolve", RoleOperator, s.handleResolveIncident)

	s.route("POST /v1/batch/{op}", RoleOperator, s.handleBatch)

	s.route("GET /v1/events", RoleViewer, s.handleSSE)

	s.route("POST /v1/decisions", RoleAdmin, s.handleDeployDecision)
	s.route("GET /v1/decisions", RoleViewer, s.handleListDecisions)

	s.registerAgentRoutes()
	s.registerVaultRoutes()
	s.registerFormRoutes()
	s.registerWebhookRoutes()
	s.registerAnalyticsRoutes()
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
	case strings.Contains(err.Error(), "bpmn:"), strings.Contains(err.Error(), "dmn:"),
		strings.Contains(err.Error(), "expr:"), strings.Contains(err.Error(), "form:"):
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
