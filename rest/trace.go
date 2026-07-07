package rest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"

	"github.com/olbboy/fliable/engine"
)

// W3C Trace Context (https://www.w3.org/TR/trace-context/): every request
// carries or is assigned a `traceparent`, echoed as X-Trace-Id and
// propagated to workers so a single business transaction is traceable
// across the engine and every external/AI worker — OpenTelemetry-
// compatible on the wire, no SDK dependency.

var traceparentRE = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)

type traceKey struct{}

type traceInfo struct {
	TraceID     string
	Traceparent string
}

// ensureTrace returns the request's trace context, generating a new one
// when the incoming traceparent is missing or malformed.
func ensureTrace(r *http.Request) traceInfo {
	tp := r.Header.Get("traceparent")
	if traceparentRE.MatchString(tp) {
		return traceInfo{TraceID: tp[3:35], Traceparent: tp}
	}
	traceID := randHex(16)
	span := randHex(8)
	return traceInfo{TraceID: traceID, Traceparent: "00-" + traceID + "-" + span + "-01"}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		for i := range b {
			b[i] = 0
		}
	}
	return hex.EncodeToString(b)
}

func traceFrom(ctx context.Context) traceInfo {
	t, _ := ctx.Value(traceKey{}).(traceInfo)
	return t
}

// statusWriter captures the response status for request logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush and the http.Flusher interface must survive the wrapper so SSE
// keeps working.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// traceparentVar is the reserved instance variable that carries the trace
// context to workers (external tasks and agent jobs snapshot instance
// variables, so it rides along automatically).
const traceparentVar = engine.TraceparentVar
