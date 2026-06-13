package main

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"github.com/asker/asker/platform/telemetry"
)

// newHealthHandler serves the plain HTTP health endpoints: /healthz is
// process liveness, /readyz reports whether the consumer loop is running
// (flips false during shutdown drain so orchestrators stop routing early).
func newHealthHandler(ready *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", getOnly(handleHealthz))
	mux.HandleFunc("/readyz", getOnly(handleReadyz(ready)))
	// Prometheus scrape endpoint on the existing health server; works without
	// an OTLP collector. Exposes the pipeline records/stage-duration metrics.
	mux.Handle("GET /metrics", telemetry.MetricsHandler())
	mux.HandleFunc("/", handleNotFound)
	return telemetry.HTTPMiddleware(serviceName)(mux)
}

// getOnly rejects non-GET methods with a JSON 405.
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

func handleReadyz(ready *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

func handleNotFound(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encoding a map[string]string cannot fail; a write error means the
	// client went away and there is nothing useful left to do.
	_ = json.NewEncoder(w).Encode(body)
}
