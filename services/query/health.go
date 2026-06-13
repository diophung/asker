package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/asker/asker/platform/telemetry"
)

const readyzProbeTimeout = 2 * time.Second

// newHealthHandler serves the plain HTTP health endpoints: /healthz is
// process liveness, /readyz additionally pings Vespa's health endpoint — the
// query service is useless without its index.
func newHealthHandler(vespaURL string) http.Handler {
	healthURL := strings.TrimRight(vespaURL, "/") + "/state/v1/health"
	probe := &http.Client{Timeout: readyzProbeTimeout}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyzProbeTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		resp, err := probe.Do(req)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	// Prometheus scrape endpoint. Served on the existing health HTTP server so
	// the gRPC port stays RPC-only; works without an OTLP collector.
	mux.Handle("GET /metrics", telemetry.MetricsHandler())
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	return telemetry.HTTPMiddleware(serviceName)(mux)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encoding a map[string]string cannot fail; a write error means the
	// client went away and there is nothing useful left to do.
	_ = json.NewEncoder(w).Encode(body)
}
