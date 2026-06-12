// Package telemetry initializes OpenTelemetry tracing and metrics for Asker
// services. With an empty OTLP endpoint it installs no-op providers so
// services behave identically with telemetry off.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// shutdownTimeout caps the final flush so process exit is never held hostage
// by an unreachable collector.
const shutdownTimeout = 5 * time.Second

// Config controls telemetry initialization.
type Config struct {
	// ServiceName is reported as service.name on all telemetry.
	ServiceName string
	// OTLPEndpoint is the OTLP gRPC collector endpoint, either host:port or a
	// URL (e.g. http://otel-collector:4317). Empty disables telemetry export.
	OTLPEndpoint string
}

// Init configures the global TracerProvider, MeterProvider and propagators.
// It returns a shutdown function that flushes pending telemetry; the shutdown
// function is never nil when err is nil. Exporters connect lazily, so Init
// does not block on an unreachable endpoint.
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	if cfg.OTLPEndpoint == "" {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
		return func(context.Context) error { return nil }, nil
	}

	// Schemaless so the merge never conflicts with the schema URL of
	// resource.Default(), which tracks the SDK's semconv version.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", cfg.ServiceName),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
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
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	shutdown := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}
	return shutdown, nil
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
