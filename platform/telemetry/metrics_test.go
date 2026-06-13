package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// TestMetricsHandlerServesPrometheusWithoutCollector verifies the core M5
// requirement: with NO OTLP endpoint configured (no collector), Init still
// installs the Prometheus reader, an instrument recorded through the global
// MeterProvider appears on /metrics, and the response is the Prometheus text
// exposition format with a 200.
func TestMetricsHandlerServesPrometheusWithoutCollector(t *testing.T) {
	saveGlobals(t)

	shutdown, err := Init(context.Background(), Config{ServiceName: "metrics-test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// Record through the global meter (what services use). The metric name is
	// chosen to be unmistakable in the scrape output.
	counter, err := otel.Meter("telemetry-test").Int64Counter("asker_telemetry_probe_total")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(context.Background(), 7, metric.WithAttributes())

	rec := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want Prometheus text format (text/plain...)", ct)
	}

	body, _ := io.ReadAll(rec.Body)
	out := string(body)
	// The OTel Prometheus exporter renders an Int64Counter "x_total" as the
	// metric "x_total" with value 7. Assert both the series and its value show
	// up so we know the reader is actually wired to the registry.
	if !strings.Contains(out, "asker_telemetry_probe_total") {
		t.Errorf("scrape output missing recorded instrument; got:\n%s", out)
	}
	if !strings.Contains(out, "asker_telemetry_probe_total 7") {
		t.Errorf("scrape output missing recorded value 7; got:\n%s", out)
	}
}

// TestMetricsHandlerBeforeInit verifies /metrics serves a 200 even when Init
// was never called (the runtime/process collectors are present, no instruments
// required) — a service's health server must never depend on Init order.
func TestMetricsHandlerBeforeInit(t *testing.T) {
	rec := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status before Init = %d, want 200", rec.Code)
	}
}
