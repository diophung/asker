package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/asker/asker/platform/telemetry"
)

const readyzProbeTimeout = 2 * time.Second

// dbPinger is the slice of *pgxpool.Pool the readiness probe needs.
type dbPinger interface {
	Ping(ctx context.Context) error
}

// newHealthHandler serves the plain HTTP health endpoints: /healthz is
// process liveness, /readyz additionally pings Postgres (the service is
// useless without it).
func newHealthHandler(db dbPinger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", getOnly(handleHealthz))
	mux.HandleFunc("/readyz", getOnly(handleReadyz(db)))
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

func handleReadyz(db dbPinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyzProbeTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
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
