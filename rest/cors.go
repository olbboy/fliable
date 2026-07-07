package rest

import (
	"net/http"
	"strconv"
	"strings"
)

// CORSConfig enables browser SPAs (React/shadcn/Base UI, Vue, Svelte, ...)
// hosted on other origins to call the API directly. Without it, a browser
// blocks cross-origin XHR/fetch before the request reaches a handler.
type CORSConfig struct {
	// AllowOrigins lists permitted origins, or ["*"] for any. With
	// AllowCredentials, "*" is narrowed to the caller's origin (the spec
	// forbids "*" + credentials).
	AllowOrigins     []string
	AllowMethods     []string
	AllowHeaders     []string
	ExposeHeaders    []string
	AllowCredentials bool
	MaxAgeSeconds    int
}

// DefaultCORS allows any origin with the headers the API uses — a sane
// default for public/dev APIs. Lock AllowOrigins down in production.
func DefaultCORS() *CORSConfig {
	return &CORSConfig{
		AllowOrigins:  []string{"*"},
		AllowMethods:  []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:  []string{"Content-Type", "Authorization", "X-Api-Key", "X-Tenant-Id"},
		ExposeHeaders: []string{"X-Trace-Id"},
		MaxAgeSeconds: 600,
	}
}

// apply writes CORS response headers and reports whether the request was a
// preflight that the caller should answer with 204 and no body.
func (c *CORSConfig) apply(w http.ResponseWriter, r *http.Request) (preflight bool) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	allow := c.originFor(origin)
	if allow == "" {
		return false // origin not allowed: emit no CORS headers, browser blocks
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", allow)
	if allow != "*" {
		h.Add("Vary", "Origin")
	}
	if c.AllowCredentials {
		h.Set("Access-Control-Allow-Credentials", "true")
	}
	if len(c.ExposeHeaders) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(c.ExposeHeaders, ", "))
	}
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		h.Set("Access-Control-Allow-Methods", strings.Join(c.AllowMethods, ", "))
		h.Set("Access-Control-Allow-Headers", strings.Join(c.AllowHeaders, ", "))
		if c.MaxAgeSeconds > 0 {
			h.Set("Access-Control-Max-Age", strconv.Itoa(c.MaxAgeSeconds))
		}
		return true
	}
	return false
}

func (c *CORSConfig) originFor(origin string) string {
	for _, o := range c.AllowOrigins {
		if o == "*" {
			if c.AllowCredentials {
				return origin // reflect: "*" + credentials is invalid
			}
			return "*"
		}
		if strings.EqualFold(o, origin) {
			return origin
		}
	}
	return ""
}
