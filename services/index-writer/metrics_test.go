package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/types/known/timestamppb"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/telemetry"
)

// TestPipelineMetricsExportedOnScrape feeds a fresh document through the
// metric-instrumented writer handler, then scrapes telemetry.MetricsHandler()
// and asserts the pipeline + freshness instruments render with the documented
// scrape names and low-cardinality labels.
//
// telemetry.Init runs BEFORE newPipelineMetrics so the SDK MeterProvider (with
// the Prometheus reader) is installed; with no OTLP endpoint this is the
// no-collector path the service runs in by default.
func TestPipelineMetricsExportedOnScrape(t *testing.T) {
	saveOtelGlobals(t)

	shutdown, err := telemetry.Init(context.Background(), telemetry.Config{ServiceName: "index-writer-metrics-test"})
	if err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	stub := newVespaStub(t, nil)
	w := newTestWriter(t, stub.srv.URL, 4)
	pm := newPipelineMetrics()
	handle := pm.instrument(serviceName, "docs.enriched", w.Handle)

	// A fresh document: created just now, so its index-time age lands in a
	// small bucket. richDoc is dim-4 and feeds cleanly against the stub.
	doc := richDoc()
	doc.Ts = &askerv1.Timestamps{Created: timestamppb.New(time.Now().Add(-2 * time.Second))}
	if err := handle(tenantCtx(t, "tenant-a"), doc); err != nil {
		t.Fatalf("instrumented Handle: %v", err)
	}

	out := scrape(t)

	for _, want := range []string{
		// records counter, labeled stage/topic/result/doc_type (all low-card).
		`asker_pipeline_records_total{`,
		`result="ok"`,
		`stage="index-writer"`,
		`topic="docs.enriched"`,
		// stage-duration histogram.
		"asker_pipeline_stage_duration_milliseconds_bucket",
		// doc-age freshness histogram with the 30-min (1800s) SLA boundary.
		"asker_index_doc_age_seconds_bucket",
		`le="1800"`,
		"asker_index_doc_age_seconds_count",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape missing %q\n%s", want, out)
		}
	}

	// Cardinality guard: no tenant/doc_id labels.
	for _, forbidden := range []string{"tenant_id=", "tenant=", "doc_id="} {
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
