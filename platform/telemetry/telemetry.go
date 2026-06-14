// Package telemetry initializes OpenTelemetry tracing and metrics for Asker
// services. With an empty OTLP endpoint it installs no-op tracing and skips
// OTLP metric export, but it still installs a Prometheus MeterProvider reader
// so the /metrics scrape endpoint (telemetry.MetricsHandler) works WITHOUT a
// collector — a service running with no telemetry backend behaves identically
// at the request path and additionally serves an empty/registered /metrics.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// shutdownTimeout caps the final flush so process exit is never held hostage
// by an unreachable collector.
const shutdownTimeout = 5 * time.Second

// promState holds the process-wide Prometheus registry that the OTel
// Prometheus exporter registers metrics into and that MetricsHandler serves.
// It is populated by Init exactly once; MetricsHandler reads it. A dedicated
// (non-default) registry keeps the scrape output limited to instruments
// recorded through the OTel MeterProvider plus the Go/process collectors we
// opt into, with no global-registry surprises.
var promState struct {
	once     sync.Once
	registry *prometheus.Registry
}

// registry returns the process Prometheus registry, creating it (with the
// standard Go runtime + process collectors) on first use. It is safe to call
// before Init: MetricsHandler then serves only the runtime/process metrics,
// and a later Init wires the OTel exporter into the same registry.
func registry() *prometheus.Registry {
	promState.once.Do(func() {
		reg := prometheus.NewRegistry()
		// Best-effort runtime/process metrics. Registration cannot fail on a
		// fresh registry; ignore the error rather than panic a service at boot
		// over scrape-only telemetry.
		_ = reg.Register(collectors.NewGoCollector())
		_ = reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		promState.registry = reg
	})
	return promState.registry
}

// Config controls telemetry initialization.
type Config struct {
	// ServiceName is reported as service.name on all telemetry.
	ServiceName string
	// OTLPEndpoint is the OTLP gRPC collector endpoint, either host:port or a
	// URL (e.g. http://otel-collector:4317). Empty disables OTLP export (and
	// installs no-op tracing); the Prometheus metric reader is installed
	// regardless so /metrics serves without a collector.
	OTLPEndpoint string
}

// Init configures the global TracerProvider, MeterProvider and propagators.
// It returns a shutdown function that flushes pending telemetry; the shutdown
// function is never nil when err is nil. Exporters connect lazily, so Init
// does not block on an unreachable endpoint.
//
// The MeterProvider always has a Prometheus reader (so telemetry.MetricsHandler
// serves the registered instruments without any collector); when OTLPEndpoint
// is set it additionally gets an OTLP periodic reader and the tracer exports
// over OTLP. When OTLPEndpoint is empty, tracing is a no-op and the only metric
// reader is Prometheus.
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	// Schemaless so the merge never conflicts with the schema URL of
	// resource.Default(), which tracks the SDK's semconv version.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", cfg.ServiceName),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	// Prometheus reader: always installed so /metrics works without a
	// collector. The exporter registers into the process registry that
	// MetricsHandler serves; WithoutScopeInfo drops the otel_scope_* labels
	// (instrumentation scope is not a useful dashboard dimension here).
	promExp, err := otelprom.New(
		otelprom.WithRegisterer(registry()),
		otelprom.WithoutScopeInfo(),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create prometheus exporter: %w", err)
	}
	meterReaders := []sdkmetric.Option{
		sdkmetric.WithReader(promExp),
		sdkmetric.WithResource(res),
	}

	// No OTLP endpoint: no-op tracing, Prometheus-only metrics.
	if cfg.OTLPEndpoint == "" {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		mp := sdkmetric.NewMeterProvider(meterReaders...)
		otel.SetMeterProvider(mp)
		shutdown := func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
			defer cancel()
			return mp.Shutdown(ctx)
		}
		return shutdown, nil
	}

	traceExp, err := otlptracegrpc.New(ctx, traceExporterOptions(cfg.OTLPEndpoint)...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create trace exporter: %w", err)
	}
	metricExp, err := otlpmetricgrpc.New(ctx, metricExporterOptions(cfg.OTLPEndpoint)...)
	if err != nil {
		// Best-effort cleanup; the trace exporter has no live connection yet.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = traceExp.Shutdown(shutdownCtx)
		return nil, fmt.Errorf("telemetry: create metric exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	// Both readers coexist on one MeterProvider: instruments are recorded once
	// and fanned out to Prometheus (scrape) and OTLP (push).
	mp := sdkmetric.NewMeterProvider(append(meterReaders,
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
	)...)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	shutdown := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}
	return shutdown, nil
}

// MetricsHandler returns an http.Handler that serves the registered metrics in
// the Prometheus text exposition format. Services mount it at GET /metrics on
// their existing health HTTP server. It is safe to call before Init (it serves
// only the Go runtime/process collectors until Init wires in the OTel
// exporter) and never requires a collector to be reachable.
func MetricsHandler() http.Handler {
	return promhttp.HandlerFor(registry(), promhttp.HandlerOpts{})
}

// traceExporterOptions maps an endpoint to exporter options. URL-form
// endpoints carry their own scheme (http => insecure); bare host:port is
// treated as insecure, which is what dev compose uses.
func traceExporterOptions(endpoint string) []otlptracegrpc.Option {
	if strings.Contains(endpoint, "://") {
		return []otlptracegrpc.Option{otlptracegrpc.WithEndpointURL(endpoint)}
	}
	return []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	}
}

func metricExporterOptions(endpoint string) []otlpmetricgrpc.Option {
	if strings.Contains(endpoint, "://") {
		return []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpointURL(endpoint)}
	}
	return []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
	}
}
