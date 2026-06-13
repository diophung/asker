package main

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// instrumentationName scopes the query service's OTel instruments (mirrors the
// platform/telemetry HTTP middleware convention).
const instrumentationName = "github.com/asker/asker/services/query"

// Instrument names below are the OTel instrument names. The platform/telemetry
// Prometheus exporter renders them to the scrape names the M5 dashboards wire,
// using OTel's standard suffixing (left as default so it matches the existing
// http.server.requests -> http_server_requests_total convention):
//
//	instrument "asker_query_search_duration"   unit "ms"        -> asker_query_search_duration_milliseconds   (histogram)
//	instrument "asker_query_cache_requests"     unit "{request}" -> asker_query_cache_requests_total            (counter)
//	instrument "asker_query_degradation_events" unit "{event}"   -> asker_query_degradation_events_total         (counter)
//
// The histogram is in MILLISECONDS (the exporter appends _milliseconds), which
// keeps the unit explicit in the scrape name and matches TookMs already on the
// wire. Buckets straddle the P90<=5s SLO (5000ms boundary present) so a
// dashboard reads P90 and an alert reads "fraction over 5s" directly.
const (
	instSearchDuration    = "asker_query_search_duration"
	instCacheRequests     = "asker_query_cache_requests"
	instDegradationEvents = "asker_query_degradation_events"
)

// queryMetrics holds the query-path instruments. They are created from the
// global meter, so when telemetry was initialized without an endpoint they are
// no-op-safe (recording does nothing) and when initialized they feed both the
// Prometheus scrape endpoint and any OTLP collector.
//
// Cardinality: every label here is low-cardinality and bounded by a closed set
// (search mode, the degradation marker, cache result, rung). NO tenant/doc/
// query labels — those would explode Prometheus series across ~10M tenants.
type queryMetrics struct {
	searchDuration    metric.Float64Histogram
	cacheRequests     metric.Int64Counter
	degradationEvents metric.Int64Counter
}

// searchDurationBucketsMs are explicit latency buckets (milliseconds) chosen to
// straddle the P90<=5s SLO: dense below it for percentile resolution, with the
// 5000ms boundary present so an alert rule can read "fraction over 5s" exactly.
var searchDurationBucketsMs = []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

func newQueryMetrics() *queryMetrics {
	meter := otel.Meter(instrumentationName)

	searchDuration, err := meter.Float64Histogram(
		instSearchDuration,
		metric.WithDescription("End-to-end query Search latency."),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(searchDurationBucketsMs...),
	)
	if err != nil {
		otel.Handle(err)
	}

	cacheRequests, err := meter.Int64Counter(
		instCacheRequests,
		metric.WithDescription("Query result-cache lookups by outcome."),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		otel.Handle(err)
	}

	degradationEvents, err := meter.Int64Counter(
		instDegradationEvents,
		metric.WithDescription("Query degradation-ladder rung activations."),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		otel.Handle(err)
	}

	return &queryMetrics{
		searchDuration:    searchDuration,
		cacheRequests:     cacheRequests,
		degradationEvents: degradationEvents,
	}
}

// recordSearch records the Search latency labeled by mode, degraded marker,
// cache result, and outcome (ok|error). ms is the measured duration in ms.
// CRITICAL: failed searches MUST be recorded too — a Vespa/embed brownout that
// takes 4.9s and then returns codes.Unavailable still consumes the latency
// budget, and the HARD P90<=5s SLO alert reads only this histogram (M5 review).
// outcome="error" lets a dashboard split healthy vs failed latency while the
// SLO alert (sum by le, no label filter) sees both.
func (m *queryMetrics) recordSearch(ctx context.Context, ms float64, mode, degraded, cache, outcome string) {
	if m == nil || m.searchDuration == nil {
		return
	}
	if degraded == "" {
		degraded = "none"
	}
	m.searchDuration.Record(ctx, ms, metric.WithAttributes(
		attribute.String("mode", mode),
		attribute.String("degraded", degraded),
		attribute.String("cache", cache),
		attribute.String("outcome", outcome),
	))
}

// recordCache counts one cache lookup outcome ("hit" or "miss").
func (m *queryMetrics) recordCache(ctx context.Context, result string) {
	if m == nil || m.cacheRequests == nil {
		return
	}
	m.cacheRequests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("result", result),
	))
}

// recordDegradation counts one degradation rung activation
// ("keyword-only" or "clip-unavailable").
func (m *queryMetrics) recordDegradation(ctx context.Context, rung string) {
	if m == nil || m.degradationEvents == nil {
		return
	}
	m.degradationEvents.Add(ctx, 1, metric.WithAttributes(
		attribute.String("rung", rung),
	))
}
