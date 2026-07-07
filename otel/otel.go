// Package otel exports Fliable execution as OpenTelemetry traces over
// OTLP/HTTP (JSON encoding) — zero dependencies, pure stdlib. It derives
// spans from the engine's event-sourced history stream: one root span
// per process instance and one child span per executed element, parented
// under the caller's W3C traceparent when the instance was started with
// one. Point it at any OTLP collector (otel-collector, Jaeger, Grafana
// Tempo, Datadog agent) on the standard :4318 HTTP port.
package otel

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
)

// Config tunes the exporter.
type Config struct {
	// Endpoint is the OTLP/HTTP base URL, e.g. "http://localhost:4318";
	// traces POST to <Endpoint>/v1/traces.
	Endpoint string
	// ServiceName sets resource attribute service.name (default "fliable").
	ServiceName string
	// Headers are added to every export request (auth tokens etc.).
	Headers map[string]string
	// BatchSize triggers an export when this many spans are buffered
	// (default 512). FlushInterval exports whatever is buffered on a
	// timer (default 5s).
	BatchSize     int
	FlushInterval time.Duration
	// Client overrides the HTTP client (default 10s timeout).
	Client *http.Client
	// Logger receives export failures (default slog.Default).
	Logger *slog.Logger
}

// Exporter converts engine history events to OTLP spans and ships them.
type Exporter struct {
	cfg Config

	mu    sync.Mutex
	insts map[string]*instTrace
	buf   []span

	spanSeq  atomic.Uint64
	spanBase [4]byte

	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

// instTrace tracks the open trace of one running instance.
type instTrace struct {
	traceID    string
	rootSpanID string
	rootParent string
	rootName   string
	started    time.Time
	// open element spans: elementID -> start-time stack (multi-instance
	// activations of the same element nest LIFO).
	open map[string][]time.Time
}

type span struct {
	TraceID  string
	SpanID   string
	ParentID string
	Name     string
	Start    time.Time
	End      time.Time
	Attrs    map[string]string
}

// New creates an exporter. Call Attach to wire it to an engine, Close on
// shutdown.
func New(cfg Config) *Exporter {
	if cfg.ServiceName == "" {
		cfg.ServiceName = "fliable"
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 512
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	x := &Exporter{
		cfg:     cfg,
		insts:   map[string]*instTrace{},
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	_, _ = rand.Read(x.spanBase[:])
	go x.loop()
	return x
}

// Attach subscribes the exporter to an engine's event stream.
func (x *Exporter) Attach(e *engine.Engine) { e.OnEvent(x.consume) }

// consume runs synchronously inside the engine (under the instance lock):
// it only mutates in-memory state and never blocks on I/O.
func (x *Exporter) consume(ev *store.HistoryEvent) {
	x.mu.Lock()
	defer x.mu.Unlock()
	switch ev.Type {
	case store.HistInstanceStarted:
		it := &instTrace{
			rootSpanID: x.nextSpanID(),
			rootName:   "process",
			started:    ev.Time,
			open:       map[string][]time.Time{},
		}
		if key, _ := ev.Detail["definitionKey"].(string); key != "" {
			it.rootName = "process " + key
		}
		if tp, _ := ev.Detail["traceparent"].(string); len(tp) == 55 {
			it.traceID = tp[3:35]
			it.rootParent = tp[36:52]
		} else {
			sum := sha256.Sum256([]byte(ev.InstanceID))
			it.traceID = hex.EncodeToString(sum[:16])
		}
		x.insts[ev.InstanceID] = it

	case store.HistElementActivated:
		if it := x.insts[ev.InstanceID]; it != nil && ev.ElementID != "" {
			it.open[ev.ElementID] = append(it.open[ev.ElementID], ev.Time)
		}

	case store.HistElementCompleted, store.HistTaskCompleted, store.HistTaskCanceled, store.HistAgentCompleted:
		x.closeElement(ev, "")

	case store.HistIncidentCreated:
		x.closeElement(ev, "incident")

	case store.HistInstanceCompleted, store.HistInstanceTerminated:
		it := x.insts[ev.InstanceID]
		if it == nil {
			return
		}
		// Dangling opens (cancelled waits, interrupted scopes) close with
		// the instance so no span is lost.
		for el, starts := range it.open {
			for _, st := range starts {
				x.bufferLocked(span{
					TraceID: it.traceID, SpanID: x.nextSpanID(), ParentID: it.rootSpanID,
					Name: el, Start: st, End: ev.Time,
					Attrs: map[string]string{"fliable.instance": ev.InstanceID, "fliable.unfinished": "true"},
				})
			}
		}
		status := "completed"
		if ev.Type == store.HistInstanceTerminated {
			status = "terminated"
		}
		x.bufferLocked(span{
			TraceID: it.traceID, SpanID: it.rootSpanID, ParentID: it.rootParent,
			Name: it.rootName, Start: it.started, End: ev.Time,
			Attrs: map[string]string{"fliable.instance": ev.InstanceID, "fliable.outcome": status},
		})
		delete(x.insts, ev.InstanceID)
	}
}

// closeElement pops the element's open-span stack and buffers the span.
func (x *Exporter) closeElement(ev *store.HistoryEvent, note string) {
	it := x.insts[ev.InstanceID]
	if it == nil || ev.ElementID == "" {
		return
	}
	starts := it.open[ev.ElementID]
	if len(starts) == 0 {
		return
	}
	start := starts[len(starts)-1]
	if len(starts) == 1 {
		delete(it.open, ev.ElementID)
	} else {
		it.open[ev.ElementID] = starts[:len(starts)-1]
	}
	attrs := map[string]string{"fliable.instance": ev.InstanceID}
	if note != "" {
		attrs["fliable.note"] = note
	}
	x.bufferLocked(span{
		TraceID: it.traceID, SpanID: x.nextSpanID(), ParentID: it.rootSpanID,
		Name: ev.ElementID, Start: start, End: ev.Time, Attrs: attrs,
	})
}

func (x *Exporter) bufferLocked(s span) {
	x.buf = append(x.buf, s)
	// Bounded buffer: drop the oldest half if the collector is
	// unreachable for long — observability must never OOM the engine.
	if len(x.buf) > 16*x.cfg.BatchSize {
		x.cfg.Logger.Warn("otel: span buffer overflow, dropping oldest", "dropped", len(x.buf)/2)
		x.buf = append([]span(nil), x.buf[len(x.buf)/2:]...)
	}
}

func (x *Exporter) nextSpanID() string {
	var b [8]byte
	copy(b[:4], x.spanBase[:])
	seq := x.spanSeq.Add(1)
	b[4] = byte(seq >> 24)
	b[5] = byte(seq >> 16)
	b[6] = byte(seq >> 8)
	b[7] = byte(seq)
	return hex.EncodeToString(b[:])
}

func (x *Exporter) loop() {
	t := time.NewTicker(x.cfg.FlushInterval)
	defer t.Stop()
	defer close(x.stopped)
	for {
		select {
		case <-t.C:
			if err := x.Flush(); err != nil {
				x.cfg.Logger.Warn("otel: export failed", "error", err)
			}
		case <-x.stop:
			return
		}
	}
}

// Flush exports every buffered span now. On failure spans return to the
// buffer for the next attempt.
func (x *Exporter) Flush() error {
	x.mu.Lock()
	batch := x.buf
	x.buf = nil
	x.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	if err := x.export(batch); err != nil {
		x.mu.Lock()
		x.buf = append(batch, x.buf...)
		x.mu.Unlock()
		return err
	}
	return nil
}

// Close stops the flush loop and drains the buffer.
func (x *Exporter) Close() error {
	x.once.Do(func() { close(x.stop) })
	<-x.stopped
	return x.Flush()
}

// export ships one OTLP/HTTP JSON request.
func (x *Exporter) export(batch []span) error {
	otlpSpans := make([]map[string]any, len(batch))
	for i, s := range batch {
		attrs := make([]map[string]any, 0, len(s.Attrs))
		for k, v := range s.Attrs {
			attrs = append(attrs, map[string]any{"key": k, "value": map[string]any{"stringValue": v}})
		}
		sp := map[string]any{
			"traceId":           s.TraceID,
			"spanId":            s.SpanID,
			"name":              s.Name,
			"kind":              1, // SPAN_KIND_INTERNAL
			"startTimeUnixNano": strconv.FormatInt(s.Start.UnixNano(), 10),
			"endTimeUnixNano":   strconv.FormatInt(s.End.UnixNano(), 10),
			"attributes":        attrs,
		}
		if s.ParentID != "" {
			sp["parentSpanId"] = s.ParentID
		}
		otlpSpans[i] = sp
	}
	body, err := json.Marshal(map[string]any{
		"resourceSpans": []map[string]any{{
			"resource": map[string]any{
				"attributes": []map[string]any{{
					"key": "service.name", "value": map[string]any{"stringValue": x.cfg.ServiceName},
				}},
			},
			"scopeSpans": []map[string]any{{
				"scope": map[string]any{"name": "fliable"},
				"spans": otlpSpans,
			}},
		}},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, x.cfg.Endpoint+"/v1/traces", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range x.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := x.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("otel: collector returned HTTP %d", resp.StatusCode)
	}
	return nil
}
