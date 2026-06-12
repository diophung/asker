package telemetry

import (
	"net/http"
	"strconv"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const instrumentationName = "github.com/asker/asker/platform/telemetry"

// HTTPMiddleware returns middleware that traces requests via otelhttp (span
// name "METHOD route") and records RED metrics. Request duration comes from
// otelhttp's built-in semconv http.server.request.duration histogram; this
// middleware adds only an http.server.requests counter labeled by method and
// status code class (recording a second duration histogram here would
// double-count every request). It uses the global tracer/meter providers, so
// it is safe (and a no-op) when telemetry was never initialized or was
// initialized without an endpoint.
func HTTPMiddleware(serviceName string) func(http.Handler) http.Handler {
	meter := otel.Meter(instrumentationName)
	requests, err := meter.Int64Counter("http.server.requests",
		metric.WithDescription("Number of HTTP requests handled."),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		otel.Handle(err)
	}

	return func(next http.Handler) http.Handler {
		measured := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			if requests != nil {
				requests.Add(r.Context(), 1, metric.WithAttributes(
					attribute.String("http.request.method", normalizeMethod(r.Method)),
					attribute.String("http.response.status_class", statusClass(rec.status)),
				))
			}
		})
		return otelhttp.NewHandler(measured, serviceName,
			otelhttp.WithSpanNameFormatter(spanName),
		)
	}
}

// normalizeMethod bounds metric label cardinality: anything outside the nine
// well-known HTTP methods is recorded as "_OTHER" (per OTel semconv), since
// unauthenticated clients control the raw method string.
func normalizeMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return m
	}
	return "_OTHER"
}

// spanName builds "METHOD route", preferring the ServeMux pattern when the
// request was matched by one (otelhttp re-invokes the formatter after the
// handler runs once r.Pattern is known), falling back to the raw path.
func spanName(_ string, r *http.Request) string {
	if p := r.Pattern; p != "" {
		// Patterns registered as "GET /path" already include the method.
		if strings.Contains(p, " ") {
			return p
		}
		return r.Method + " " + p
	}
	return r.Method + " " + r.URL.Path
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "unknown"
	}
	return strconv.Itoa(status/100) + "xx"
}

// statusRecorder captures the response status code for metric labels.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap supports http.ResponseController pass-through (Flush, deadlines).
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
