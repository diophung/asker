package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	"github.com/asker/asker/platform/telemetry"
)

// TestQueryMetricsExportedOnScrape drives a real Search (cache miss, then a hit
// on the identical request) and a degraded search through the gRPC harness,
// then scrapes telemetry.MetricsHandler() and asserts the query instruments
// appear with the documented scrape names and low-cardinality labels.
//
// telemetry.Init must run BEFORE the server is built: newQueryMetrics() binds
// to the global meter at construction time, so the SDK MeterProvider (with the
// Prometheus reader) has to be installed first. With no OTLP endpoint this is
// the no-collector path the services run in by default.
func TestQueryMetricsExportedOnScrape(t *testing.T) {
	saveOtelGlobals(t)

	shutdown, err := telemetry.Init(context.Background(), telemetry.Config{ServiceName: "query-metrics-test"})
	if err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// TEI down so the HYBRID query degrades to keyword-only: exercises both the
	// degradation counter and the degraded="keyword-only" duration label.
	env := newQueryEnv(t, withTEIDown())
	ctx := tenantCtx(t, "tenant-metrics")

	req := &queryv1.SearchRequest{Query: "metrics probe"}

	// First call: cache miss, TEI-down -> keyword-only degraded result (not
	// cached, since degraded results are never cached).
	if _, err := env.client.Search(ctx, req); err != nil {
		t.Fatalf("first Search: %v", err)
	}

	// A clean (non-degraded) request that WILL be cached, then repeated for a
	// cache hit. A filter-only query needs no embedder, so it does not degrade.
	clean := &queryv1.SearchRequest{Query: "type:email"}
	if _, err := env.client.Search(ctx, clean); err != nil {
		t.Fatalf("clean Search (miss): %v", err)
	}
	if _, err := env.client.Search(ctx, clean); err != nil {
		t.Fatalf("clean Search (hit): %v", err)
	}

	out := scrape(t)

	// Histogram: documented scrape name asker_query_search_duration_milliseconds,
	// with the SLO boundary bucket present.
	for _, want := range []string{
		"asker_query_search_duration_milliseconds_bucket",
		`le="5000"`,
		"asker_query_search_duration_milliseconds_count",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape missing %q in search-duration histogram\n%s", want, out)
		}
	}

	// Cache counter: both outcomes recorded.
	if !strings.Contains(out, `asker_query_cache_requests_total{result="hit"}`) {
		t.Errorf("scrape missing cache hit counter\n%s", out)
	}
	if !strings.Contains(out, `asker_query_cache_requests_total{result="miss"}`) {
		t.Errorf("scrape missing cache miss counter\n%s", out)
	}

	// Degradation counter: keyword-only rung fired.
	if !strings.Contains(out, `asker_query_degradation_events_total{rung="keyword-only"}`) {
		t.Errorf("scrape missing keyword-only degradation counter\n%s", out)
	}

	// Cardinality guard: the forbidden high-cardinality labels must never appear.
	for _, forbidden := range []string{"tenant", "doc_id", "query="} {
		if strings.Contains(out, forbidden) {
			t.Errorf("scrape contains forbidden high-cardinality label %q\n%s", forbidden, out)
		}
	}
}

func scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	telemetry.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

// saveOtelGlobals restores the global OTel providers after the test so the
// installed SDK MeterProvider does not leak into other tests in the package.
func saveOtelGlobals(t *testing.T) {
	t.Helper()
	tp := otel.GetTracerProvider()
	mp := otel.GetMeterProvider()
	prop := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(tp)
		otel.SetMeterProvider(mp)
		otel.SetTextMapPropagator(prop)
	})
}
