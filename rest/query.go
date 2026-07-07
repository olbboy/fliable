package rest

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/olbboy/fliable/store"
)

// page is the stable list envelope returned by every collection endpoint.
// nextCursor is empty on the last page; pass it back as ?cursor= to fetch
// the following page (keyset pagination — stable under concurrent writes).
type page struct {
	Items      any    `json:"items"`
	Count      int    `json:"count"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// writePage writes a collection with pagination metadata. lastID must
// return the ID of the final element for the next cursor; it is only
// consulted when the page is full (len == limit).
func (s *Server) writePage(w http.ResponseWriter, items any, count, limit int, lastID string) {
	p := page{Items: items, Count: count}
	if limit > 0 && count == limit {
		p.NextCursor = lastID
	}
	s.json(w, http.StatusOK, p)
}

// parseVarMatches reads repeated ?var=name:op:value query parameters into
// variable predicates. Shorthand ?var=name:value means equality. Values
// parse as number, bool, or string.
func parseVarMatches(r *http.Request) []store.VarMatch {
	raw := r.URL.Query()["var"]
	if len(raw) == 0 {
		return nil
	}
	out := make([]store.VarMatch, 0, len(raw))
	for _, spec := range raw {
		parts := strings.SplitN(spec, ":", 3)
		switch len(parts) {
		case 2:
			// name:value → equality (or name:exists)
			if parts[1] == "exists" {
				out = append(out, store.VarMatch{Name: parts[0], Op: "exists"})
			} else {
				out = append(out, store.VarMatch{Name: parts[0], Op: "eq", Value: parseScalar(parts[1])})
			}
		case 3:
			out = append(out, store.VarMatch{Name: parts[0], Op: parts[1], Value: parseScalar(parts[2])})
		}
	}
	return out
}

// parseScalar coerces a query string into the JSON value space.
func parseScalar(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

func queryTime(r *http.Request, name string) time.Time {
	v := r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func queryBool(r *http.Request, name string) bool {
	return r.URL.Query().Get(name) == "true"
}

// pageLimit reads ?limit, defaulting to 100 and capping at 1000 so a
// single request can never sweep an unbounded result set.
func pageLimit(r *http.Request) int {
	n := queryInt(r, "limit", 100)
	if n <= 0 {
		n = 100
	}
	if n > 1000 {
		n = 1000
	}
	return n
}

// tenantOf returns the effective tenant for a request: the X-Tenant-Id
// header, falling back to the ?tenantId query parameter.
func tenantOf(r *http.Request) string {
	if t := r.Header.Get("X-Tenant-Id"); t != "" {
		return t
	}
	return r.URL.Query().Get("tenantId")
}
