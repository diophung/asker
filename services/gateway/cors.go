package main

import (
	"net/http"
	"strings"
)

// corsPolicy implements exact-origin-match CORS. The allowed origin is echoed
// back verbatim — never "*" — so adding credentials later cannot silently
// open the API to every origin.
type corsPolicy struct {
	origins map[string]bool
}

// newCORSPolicy parses the comma-separated CORS_ALLOWED_ORIGINS value.
// A literal "*" entry is dropped: wildcards are forbidden by contract.
func newCORSPolicy(csv string) *corsPolicy {
	origins := make(map[string]bool)
	for _, o := range strings.Split(csv, ",") {
		o = strings.TrimSpace(o)
		if o == "" || o == "*" {
			continue
		}
		origins[strings.TrimRight(o, "/")] = true
	}
	return &corsPolicy{origins: origins}
}

// middleware is the outermost handler layer: preflights are answered before
// auth (browsers never attach Authorization to OPTIONS), and Vary: Origin is
// always set so caches never serve one origin's CORS response to another.
func (c *corsPolicy) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		allowed := origin != "" && c.origins[origin]
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			// Preflight. For a denied origin respond 204 with no allow
			// headers: the browser blocks the actual request.
			if allowed {
				w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
