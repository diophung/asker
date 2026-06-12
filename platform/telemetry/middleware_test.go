package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestHTTPMiddlewareServesAndPropagates(t *testing.T) {
	saveGlobals(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
	})

	innerCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, r *http.Request) {
		innerCalled = true
		if id := r.PathValue("id"); id != "42" {
			t.Errorf("PathValue(id) = %q, want %q", id, "42")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})

	handler := HTTPMiddleware("test-svc")(mux)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/things/42", nil))

	if !innerCalled {
		t.Fatal("inner handler was not called")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if got, want := spans[0].Name(), "GET /things/{id}"; got != want {
		t.Errorf("span name = %q, want %q", got, want)
	}
}

func TestHTTPMiddlewareSpanNameWithoutPattern(t *testing.T) {
	saveGlobals(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
	})

	handler := HTTPMiddleware("test-svc")(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/raw/path", nil))

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if got, want := spans[0].Name(), "POST /raw/path"; got != want {
		t.Errorf("span name = %q, want %q", got, want)
	}
}

// TestHTTPMiddlewareUninitialized verifies the middleware is usable when
// Init was never called: the global providers default to no-ops.
func TestHTTPMiddlewareUninitialized(t *testing.T) {
	handler := HTTPMiddleware("test-svc")(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		},
	))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anything", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
}

func TestStatusClass(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{100, "1xx"},
		{200, "2xx"},
		{301, "3xx"},
		{404, "4xx"},
		{503, "5xx"},
		{599, "5xx"},
		{0, "unknown"},
		{999, "unknown"},
	}
	for _, tc := range cases {
		if got := statusClass(tc.status); got != tc.want {
			t.Errorf("statusClass(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}
