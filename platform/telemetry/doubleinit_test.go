package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
)

// TestDoubleInitThenScrape guards against the shared-registry hazard: a second
// Init registers a second Prometheus collector into the same process registry.
// If both collectors emit overlapping series, promhttp's HandlerFor would
// return a 500 on scrape. This asserts the scrape still succeeds (the registry
// must not be left in a state that breaks /metrics).
func TestDoubleInitThenScrape(t *testing.T) {
	saveGlobals(t)

	sd1, err := Init(context.Background(), Config{ServiceName: "double-a"})
	if err != nil {
		t.Fatalf("Init 1: %v", err)
	}
	t.Cleanup(func() { _ = sd1(context.Background()) })

	sd2, err := Init(context.Background(), Config{ServiceName: "double-b"})
	if err != nil {
		t.Fatalf("Init 2: %v", err)
	}
	t.Cleanup(func() { _ = sd2(context.Background()) })

	counter, err := otel.Meter("double-test").Int64Counter("asker_double_probe_total")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(context.Background(), 1)

	rec := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status after double Init = %d, want 200; body:\n%s", rec.Code, body)
	}
}
