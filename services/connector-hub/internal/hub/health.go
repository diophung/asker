package hub

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/telemetry"
)

const readyzProbeTimeout = 2 * time.Second

// readiness bundles the dependency probes behind /readyz: a live
// control-plane RPC and the Kafka producer. The producer offers no ping —
// EnsureTopics succeeding at startup is the broker reachability proof, so
// kafkaReady reports that the producer was initialized behind it.
type readiness struct {
	controlPlane func(ctx context.Context) error
	kafkaReady   func() bool
}

// controlPlaneProbe returns a readiness probe that exercises the scheduler's
// cross-tenant listing — the exact dependency the hub cannot run without.
func controlPlaneProbe(sched controlplanev1.SchedulerServiceClient) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := sched.ListAllInstances(ctx, &controlplanev1.ListAllInstancesRequest{})
		return err
	}
}

// newHealthHandler serves /healthz (process liveness) and /readyz
// (control-plane + Kafka dependencies).
func newHealthHandler(rdy readiness) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", getOnly(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	mux.HandleFunc("/readyz", getOnly(func(w http.ResponseWriter, r *http.Request) {
		if !rdy.kafkaReady() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "kafka producer not ready"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readyzProbeTimeout)
		defer cancel()
		if err := rdy.controlPlane(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "control plane unreachable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
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

// RunHealthcheck probes the local /healthz endpoint and returns a process
// exit code: the container healthcheck for the distroless image, which has
// no shell or curl (same pattern as gateway and control-plane).
func RunHealthcheck(addr string) int {
	port := "9301"
	if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
		port = p
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck failed:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck failed: status", resp.StatusCode)
		return 1
	}
	return 0
}
