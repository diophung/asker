package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// instrumentationName scopes the index-writer stage's OTel instruments.
const instrumentationName = "github.com/asker/asker/services/index-writer"

// Pipeline + freshness instrument names. The platform/telemetry Prometheus
// exporter renders them with OTel's default suffixing:
//
//	instrument "asker_pipeline_records"        unit "{record}"  -> asker_pipeline_records_total                (counter)
//	instrument "asker_pipeline_stage_duration" unit "ms"        -> asker_pipeline_stage_duration_milliseconds   (histogram)
//	instrument "asker_index_doc_age"           unit "s"         -> asker_index_doc_age_seconds                  (histogram)
//
// asker_index_doc_age_seconds = (index time) - Document.created_at, the best
// in-cluster proxy for end-to-end ingest freshness (source create -> indexed),
// against the 30-min freshness SLA. Labels are low-cardinality only: stage,
// topic, result, doc_type. NO tenant/doc_id labels.
const (
	instPipelineRecords = "asker_pipeline_records"
	instStageDuration   = "asker_pipeline_stage_duration"
	instDocAge          = "asker_index_doc_age"
)

// stageDurationBucketsMs are per-record handler-latency buckets (milliseconds):
// the index-writer's work is a Vespa feed HTTP round trip.
var stageDurationBucketsMs = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// docAgeBucketsSec straddle the 30-minute (1800s) freshness SLA so a dashboard
// reads the freshness distribution and an alert reads "fraction over 30 min"
// directly: seconds, minutes, the 30-min boundary, then stale tails.
var docAgeBucketsSec = []float64{1, 5, 10, 30, 60, 300, 600, 1800, 3600, 21600, 86400}

// pipelineMetrics holds the index-writer instruments (no-op-safe when telemetry
// was initialized without an endpoint).
type pipelineMetrics struct {
	records       metric.Int64Counter
	stageDuration metric.Float64Histogram
	docAge        metric.Float64Histogram
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
	docAge, err := meter.Float64Histogram(
		instDocAge,
		metric.WithDescription("Age of a document at index time (now - created_at); end-to-end freshness proxy."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(docAgeBucketsSec...),
	)
	if err != nil {
		otel.Handle(err)
	}
	return &pipelineMetrics{records: records, stageDuration: stageDuration, docAge: docAge}
}

// instrument wraps the writer's document handler so each invocation records the
// stage duration, a records-total{result} count, and (for non-tombstone docs
// with a created timestamp) the indexed-doc-age freshness histogram. topic is
// the consumed topic; stage is the service name. Called once per handler
// attempt by the kafkautil consumer; true dead-letter accounting lives in
// kafkautil and is a separate follow-up.
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
		docType := doc.GetType().String()

		if m.stageDuration != nil {
			m.stageDuration.Record(ctx, float64(time.Since(start).Milliseconds()),
				metric.WithAttributes(
					attribute.String("stage", stage),
					attribute.String("topic", topic),
				))
		}
		if m.records != nil {
			m.records.Add(ctx, 1, metric.WithAttributes(
				attribute.String("stage", stage),
				attribute.String("topic", topic),
				attribute.String("result", result),
				attribute.String("doc_type", docType),
			))
		}
		// Freshness: only meaningful for an upsert that actually reached the
		// index, and only when the source created timestamp is present.
		if m.docAge != nil && err == nil && !doc.GetTombstone().GetDeleted() {
			if created := doc.GetTs().GetCreated(); created != nil {
				age := time.Since(created.AsTime()).Seconds()
				if age < 0 {
					age = 0 // clock skew: never record a negative age
				}
				m.docAge.Record(ctx, age, metric.WithAttributes(
					attribute.String("stage", stage),
					attribute.String("doc_type", docType),
				))
			}
		}
		return err
	}
}
