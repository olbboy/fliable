package rest

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/olbboy/fliable/store"
)

// fanout forwards engine history events to all connected SSE clients.
// Slow clients drop events rather than block the engine.
func (s *Server) fanout(ev *store.HistoryEvent) {
	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	for ch := range s.sseSubs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// handleSSE streams the live engine event feed as server-sent events.
// Filter with ?instanceId= and/or ?type= prefix (e.g. type=task.).
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.error(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	instanceID := r.URL.Query().Get("instanceId")
	typePrefix := r.URL.Query().Get("type")

	ch := make(chan *store.HistoryEvent, 256)
	s.sseMu.Lock()
	s.sseSubs[ch] = struct{}{}
	s.sseMu.Unlock()
	defer func() {
		s.sseMu.Lock()
		delete(s.sseSubs, ch)
		s.sseMu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if instanceID != "" && ev.InstanceID != instanceID {
				continue
			}
			if typePrefix != "" && !hasPrefix(ev.Type, typePrefix) {
				continue
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		}
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
