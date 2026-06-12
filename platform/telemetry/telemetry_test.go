package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

// saveGlobals restores the global providers/propagator after a test so tests
// do not leak state into each other.
func saveGlobals(t *testing.T) {
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

func TestInitEmptyEndpointIsNoOp(t *testing.T) {
	saveGlobals(t)

	shutdown, err := Init(context.Background(), Config{ServiceName: "test-svc"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned nil shutdown")
	}

	// Spans from the global tracer must be non-recording no-ops.
	_, span := otel.Tracer("test").Start(context.Background(), "op")
	if span.IsRecording() {
		t.Error("span is recording, want no-op tracer provider")
	}
	span.End()

	// Metric instruments must be creatable and usable.
	counter, err := otel.Meter("test").Int64Counter("test.counter")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(context.Background(), 1)

	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestInitWithUnreachableEndpoint(t *testing.T) {
	saveGlobals(t)

	// Port 1 is essentially guaranteed closed; exporters connect lazily so
	// Init must return immediately without error.
	start := time.Now()
	shutdown, err := Init(context.Background(), Config{
		ServiceName:  "test-svc",
		OTLPEndpoint: "127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Init blocked for %v, want lazy connection", elapsed)
	}
	if shutdown == nil {
		t.Fatal("Init returned nil shutdown")
	}

	// The SDK providers should be live (recording) even if export fails.
	_, span := otel.Tracer("test").Start(context.Background(), "op")
	if !span.IsRecording() {
		t.Error("span is not recording, want SDK tracer provider")
	}
	span.End()

	// Shutdown must complete within its timeout even though the collector
	// endpoint never accepts connections. The flush will fail; completing
	// promptly is what matters. The parent deadline keeps the test fast.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := time.Now()
	_ = shutdown(ctx)
	if elapsed := time.Since(done); elapsed > 8*time.Second {
		t.Fatalf("shutdown took %v, want completion within timeout", elapsed)
	}
}

func TestInitWithEndpointURL(t *testing.T) {
	saveGlobals(t)

	shutdown, err := Init(context.Background(), Config{
		ServiceName:  "test-svc",
		OTLPEndpoint: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("Init with URL endpoint: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}
