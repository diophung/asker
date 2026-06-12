package main

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/asker/asker/platform/telemetry"
	"github.com/asker/asker/platform/tenancy"
)

const readyzProbeTimeout = 2 * time.Second

func newHandler(cfg gatewayConfig, auth *authenticator) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", getOnly(handleHealthz))
	mux.HandleFunc("/readyz", getOnly(handleReadyz(cfg.OIDCJWKSURL)))
	mux.Handle("/v1/me", auth.middleware(getOnly(handleMe)))
	mux.HandleFunc("/", handleNotFound)
	return telemetry.HTTPMiddleware("gateway")(mux)
}

// getOnly rejects non-GET methods with a JSON 405 (ServeMux method patterns
// would answer with a plain-text body).
func getOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		h(w, r)
	}
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports ready only when the JWKS endpoint is fetchable, since
// the gateway cannot authenticate anything without it.
func handleReadyz(jwksURL string) http.HandlerFunc {
	client := &http.Client{Timeout: readyzProbeTimeout}
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, jwksURL, nil)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	tc, err := tenancy.FromContext(r.Context())
	if err != nil {
		// Unreachable behind the auth middleware; fail closed regardless.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	email := ""
	if claims := claimsFromContext(r.Context()); claims != nil {
		if e, ok := claims["email"].(string); ok {
			email = e
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"tenant_id": string(tc.TenantID()),
		"subject":   tc.Subject(),
		"email":     email,
	})
}

func handleNotFound(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encoding a map[string]string cannot fail; a write error here means the
	// client went away and there is nothing useful left to do.
	_ = json.NewEncoder(w).Encode(body)
}
