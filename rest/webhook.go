package rest

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/olbboy/fliable/expr"
	"github.com/olbboy/fliable/store"
)

// webhookBlobKind namespaces webhook channel configs in the store.
const webhookBlobKind = "webhook"

// WebhookChannel maps an inbound HTTP event onto a message or signal —
// the event-registry primitive that turns Fliable into an n8n-style
// automation engine: a webhook starts or resumes a process, no human
// task required.
type WebhookChannel struct {
	// Name is the channel id: events arrive at POST /v1/webhooks/{name}.
	Name string `json:"name"`
	// Kind is "message" (default) or "signal".
	Kind string `json:"kind,omitempty"`
	// Event is the message/signal name to raise.
	Event string `json:"event"`
	// CorrelationExpr extracts the correlation key from the JSON payload
	// (expression over the payload object, e.g. "order.id"). Empty means
	// broadcast / start-only correlation.
	CorrelationExpr string `json:"correlationExpr,omitempty"`
	// DedupeExpr extracts an idempotency key from the payload; a repeated
	// key within the dedupe window is acknowledged but not re-delivered.
	// Empty disables dedup. The X-Idempotency-Key header always wins.
	DedupeExpr string `json:"dedupeExpr,omitempty"`
	// Secret, when set, must match the X-Webhook-Secret header.
	Secret string `json:"secret,omitempty"`
	// Vars maps variable names to expressions over the payload; when
	// empty the whole payload lands in the "payload" variable.
	Vars map[string]string `json:"vars,omitempty"`
	// TenantID scopes started instances (informational; correlation uses
	// engine-level subscriptions).
	TenantID string `json:"tenantId,omitempty"`
}

// dedupeCache is a bounded in-memory idempotency window (per node,
// best-effort — retries across a restart re-deliver, which BPMN
// correlation handles by design).
type dedupeCache struct {
	mu    sync.Mutex
	seen  map[string]time.Time
	limit int
	ttl   time.Duration
}

func newDedupeCache() *dedupeCache {
	return &dedupeCache{seen: map[string]time.Time{}, limit: 8192, ttl: time.Hour}
}

// remember reports whether key was already seen inside the window and
// records it otherwise.
func (d *dedupeCache) remember(key string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if at, ok := d.seen[key]; ok && now.Sub(at) < d.ttl {
		return true
	}
	if len(d.seen) >= d.limit {
		// Drop expired entries; if none expired, reset (bounded memory
		// beats unbounded growth for a best-effort window).
		for k, at := range d.seen {
			if now.Sub(at) >= d.ttl {
				delete(d.seen, k)
			}
		}
		if len(d.seen) >= d.limit {
			d.seen = map[string]time.Time{}
		}
	}
	d.seen[key] = now
	return false
}

func (s *Server) registerWebhookRoutes() {
	s.route("PUT /v1/webhooks/{name}", RoleAdmin, s.handlePutWebhook)
	s.route("GET /v1/webhooks", RoleViewer, s.handleListWebhooks)
	s.route("DELETE /v1/webhooks/{name}", RoleAdmin, s.handleDeleteWebhook)
	// Event ingestion authenticates with the channel secret, not a
	// platform credential, so external systems don't hold API keys.
	s.mux.HandleFunc("POST /v1/webhooks/{name}", s.handleWebhookEvent)
}

func (s *Server) handlePutWebhook(w http.ResponseWriter, r *http.Request) {
	var ch WebhookChannel
	if err := decodeJSON(r, &ch); err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	ch.Name = r.PathValue("name")
	if ch.Event == "" {
		s.error(w, http.StatusUnprocessableEntity, errors.New("webhook channel needs an event name"))
		return
	}
	if ch.Kind == "" {
		ch.Kind = "message"
	}
	if ch.Kind != "message" && ch.Kind != "signal" {
		s.error(w, http.StatusUnprocessableEntity, fmt.Errorf("unknown webhook kind %q", ch.Kind))
		return
	}
	data, _ := json.Marshal(&ch)
	if err := s.e.Store().PutBlob(&store.Blob{Kind: webhookBlobKind, Key: ch.Name, Data: data, UpdatedAt: time.Now().UTC()}); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, &ch)
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	blobs, err := s.e.Store().ListBlobs(webhookBlobKind)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []WebhookChannel{}
	for _, b := range blobs {
		var ch WebhookChannel
		if json.Unmarshal(b.Data, &ch) == nil {
			ch.Secret = "" // never echo secrets
			out = append(out, ch)
		}
	}
	s.json(w, http.StatusOK, out)
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	if err := s.e.Store().DeleteBlob(webhookBlobKind, r.PathValue("name")); err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusNoContent, nil)
}

func (s *Server) handleWebhookEvent(w http.ResponseWriter, r *http.Request) {
	b, err := s.e.Store().GetBlob(webhookBlobKind, r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		s.error(w, http.StatusNotFound, errors.New("unknown webhook channel"))
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	var ch WebhookChannel
	if err := json.Unmarshal(b.Data, &ch); err != nil {
		s.error(w, http.StatusInternalServerError, errors.New("corrupt channel config"))
		return
	}
	if ch.Secret != "" {
		got := r.Header.Get("X-Webhook-Secret")
		if subtle.ConstantTimeCompare([]byte(got), []byte(ch.Secret)) != 1 {
			s.error(w, http.StatusUnauthorized, errors.New("bad webhook secret"))
			return
		}
	}

	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		s.error(w, http.StatusBadRequest, err)
		return
	}
	payload := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			s.error(w, http.StatusBadRequest, fmt.Errorf("webhook payload must be a JSON object: %w", err))
			return
		}
	}

	// Idempotency: explicit header beats the configured expression.
	dedupeKey := r.Header.Get("X-Idempotency-Key")
	if dedupeKey == "" && ch.DedupeExpr != "" {
		if v, err := expr.Eval(ch.DedupeExpr, payload); err == nil {
			dedupeKey = expr.Stringify(v)
		}
	}
	if dedupeKey != "" && s.dedupe.remember(ch.Name+"\x00"+dedupeKey, time.Now()) {
		s.json(w, http.StatusOK, map[string]any{"deduplicated": true, "activated": 0})
		return
	}

	correlation := ""
	if ch.CorrelationExpr != "" {
		v, err := expr.Eval(ch.CorrelationExpr, payload)
		if err != nil {
			s.error(w, http.StatusUnprocessableEntity, fmt.Errorf("correlation expression: %w", err))
			return
		}
		correlation = expr.Stringify(v)
	}

	vars := map[string]any{}
	if len(ch.Vars) == 0 {
		vars["payload"] = payload
	} else {
		for name, src := range ch.Vars {
			v, err := expr.Eval(src, payload)
			if err != nil {
				s.error(w, http.StatusUnprocessableEntity, fmt.Errorf("vars.%s: %w", name, err))
				return
			}
			vars[name] = v
		}
	}

	var n int
	if ch.Kind == "signal" {
		n, err = s.e.BroadcastSignal(ch.Event, vars)
	} else {
		n, err = s.e.CorrelateMessage(ch.Event, correlation, vars)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"activated": n})
}
