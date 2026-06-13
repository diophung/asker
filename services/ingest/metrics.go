package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// instrumentationName scopes the ingest stage's OTel instruments.
const instrumentationName = "github.com/asker/asker/services/ingest"

// Pipeline instrument names. The platform/telemetry Prometheus exporter renders
// them with OTel's default suffixing:
//
//	instrument "asker_pipeline_records"        unit "{record}" -> asker_pipeline_records_total            (counter)
//	instrument "asker_pipeline_stage_duration" unit "ms"       -> asker_pipeline_stage_duration_milliseconds (histogram)
//
// Labels are low-cardinality only: stage (the service), topic (the produced-to
// Kafka topic), result (ok|error). NO tenant/doc_id labels — they would explode
// Prometheus cardinality across ~10M tenants.
const (
	instPipelineRecords = "asker_pipeline_records"
	instStageDuration   = "asker_pipeline_stage_duration"
)

// stageDurationBucketsMs are explicit per-record handler-latency buckets
// (milliseconds): an ingest record is parse/dedupe/chunk + a Kafka produce, so
// the range spans sub-millisecond to multi-second tails.
var stageDurationBucketsMs = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000}

// pipelineMetrics holds the ingest-stage instruments, created from the global
// meter (no-op-safe when telemetry was initialized without an endpoint).
type pipelineMetrics struct {
	records       metric.Int64Counter
	stageDuration metric.Float64Histogram
}

func newPipelineMetrics() *pipelineMetrics {
	meter := otel.Meter(instrumentationName)

	records, err := meter.Int64Counter(
		instPipelineRecords,
		metric.WithDescription("Pipeline records processed per stage by outcome."),
		metric.WithUnit("{record}"),
	)
	if err != nil {
		otel.Handle(err)
	}
	stageDuration, err := meter.Float64Histogram(
		instStageDuration,
		metric.WithDescription("Per-record pipeline stage processing latency."),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(stageDurationBucketsMs...),
	)
	if err != nil {
		otel.Handle(err)
	}
	return &pipelineMetrics{records: records, stageDuration: stageDuration}
}

// instrument wraps a document handler so each invocation records the stage
// duration and a records-total{result} count. topic is the stage's produced-to
// topic (the label value); stage is the service name. The wrapper is called
// once per handler attempt by the kafkautil consumer (so a record retried N
// times records N outcomes); true dead-letter accounting lives in kafkautil and
// is a separate follow-up.
func (m *pipelineMetrics) instrument(stage, topic string, h func(context.Context, *askerv1.Document) error) func(context.Context, *askerv1.Document) error {
	if m == nil {
		return h
	}
	return func(ctx context.Context, doc *askerv1.Document) error {
		start := time.Now()
		err := h(ctx, doc)
		result := "ok"
		if err != nil {
			result = "error"
		}
		attrs := metric.WithAttributes(
			attribute.String("stage", stage),
			attribute.String("topic", topic),
		)
		if m.stageDuration != nil {
			m.stageDuration.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
		}
		if m.records != nil {
			m.records.Add(ctx, 1, metric.WithAttributes(
				attribute.String("stage", stage),
				attribute.String("topic", topic),
				attribute.String("result", result),
			))
		}
		return err
	}
}
