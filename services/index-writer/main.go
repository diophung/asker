// Command index-writer consumes enriched documents from docs.enriched and
// feeds them into Vespa: upserts via document/v1 POST (full-document put) into
// the tenant's streaming group, tombstones via document/v1 DELETE. Delivery is
// at-least-once (kafkautil); Vespa upserts keyed by doc id make replays
// no-ops (ADR-004).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/asker/asker/platform/kafkautil"
	"github.com/asker/asker/platform/telemetry"
)

const (
	serviceName   = "index-writer"
	consumerGroup = "index-writer"
	// topicPartitions matches the dev partition count from ADR-004 §6.
	topicPartitions = 4
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit 0/1 (container healthcheck)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", serviceName)
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if *healthcheck {
		os.Exit(runHealthcheck(cfg.HealthAddr))
	}

	if err := run(context.Background(), cfg, logger); err != nil {
		logger.Error("index-writer exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg indexWriterConfig, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName:  serviceName,
		OTLPEndpoint: cfg.OTLPEndpoint,
	})
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	defer func() {
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(shCtx); err != nil {
			logger.Warn("telemetry shutdown", "error", err)
		}
	}()

	// Idempotent topic creation at every consumer start (build contract).
	if err := kafkautil.EnsureTopics(ctx, cfg.Kafka, topicPartitions,
		kafkautil.TopicDocsRaw, kafkautil.TopicDocsChunked,
		kafkautil.TopicDocsEnriched, kafkautil.TopicDocsDeadletter); err != nil {
		return fmt.Errorf("ensure topics: %w", err)
	}

	w, err := newWriter(cfg.VespaURL, cfg.EmbeddingDim, cfg.CLIPDim, logger)
	if err != nil {
		return err
	}

	consumer, err := kafkautil.NewConsumer(cfg.Kafka, consumerGroup, kafkautil.TopicDocsEnriched)
	if err != nil {
		return fmt.Errorf("new consumer: %w", err)
	}
	defer consumer.Close()

	ln, err := net.Listen("tcp", cfg.HealthAddr)
	if err != nil {
		return fmt.Errorf("listen health %s: %w", cfg.HealthAddr, err)
	}
	healthSrv := &http.Server{
		Handler:           newHealthHandler(cfg.VespaURL),
		ReadHeaderTimeout: 5 * time.Second,
	}
	healthErr := make(chan error, 1)
	go func() {
		if err := healthSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			healthErr <- err
			return
		}
		healthErr <- nil
	}()

	pm := newPipelineMetrics()
	// The index-writer consumes docs.enriched and feeds Vespa; the metric topic
	// label is the consumed topic (TopicDocsEnriched).
	handle := pm.instrument(serviceName, kafkautil.TopicDocsEnriched, w.Handle)

	consumeErr := make(chan error, 1)
	go func() { consumeErr <- consumer.Run(ctx, handle) }()

	logger.Info("index-writer consuming",
		"topic", kafkautil.TopicDocsEnriched, "group", consumerGroup,
		"vespa_url", cfg.VespaURL, "embedding_dim", cfg.EmbeddingDim,
		"clip_dim", cfg.CLIPDim, "health_addr", ln.Addr().String())

	shutdownHealth := func() error {
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := healthSrv.Shutdown(shCtx); err != nil {
			return fmt.Errorf("health server shutdown: %w", err)
		}
		return <-healthErr
	}

	select {
	case <-ctx.Done():
		// Graceful drain: cancellation makes Run finish the in-flight record
		// (commit-after-success) and return nil; only then stop the rest.
		logger.Info("shutdown signal received; draining consumer")
		if err := <-consumeErr; err != nil {
			logger.Warn("consumer stopped with error during drain", "error", err)
		}
		consumer.Close()
		return shutdownHealth()
	case err := <-consumeErr:
		// The consumer never stops on its own without an unrecoverable error.
		herr := shutdownHealth()
		if err != nil {
			return fmt.Errorf("consumer: %w", err)
		}
		return herr
	case err := <-healthErr:
		return fmt.Errorf("health server: %w", err)
	}
}

// newHealthHandler serves liveness (/healthz, always 200 while the process
// runs) and readiness (/readyz, 200 only when Vespa's /state/v1/health
// answers 2xx — there is no point being ready when the sink is down).
func newHealthHandler(vespaURL string) http.Handler {
	client := &http.Client{Timeout: 2 * time.Second}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, "ok")
	})
	mux.HandleFunc("GET /readyz", func(rw http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, vespaURL+"/state/v1/health", nil)
		if err != nil {
			http.Error(rw, "vespa health request: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(rw, "vespa unreachable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			http.Error(rw, fmt.Sprintf("vespa health status %d", resp.StatusCode), http.StatusServiceUnavailable)
			return
		}
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, "ready")
	})
	// Prometheus scrape endpoint on the existing health server; works without an
	// OTLP collector. Exposes the pipeline records/stage-duration metrics and
	// the asker_index_doc_age_seconds freshness histogram.
	mux.Handle("GET /metrics", telemetry.MetricsHandler())
	// Wrap in the RED middleware (like gateway/query/ingest) so this service also
	// emits http_server_requests_total{job="index-writer"} — without it the
	// AskerService5xxRateHigh alert silently never covers index-writer (M5 review).
	return telemetry.HTTPMiddleware(serviceName)(mux)
}

// runHealthcheck probes the local /healthz endpoint and returns a process
// exit code. It is the container healthcheck for the distroless image, which
// has no shell or curl.
func runHealthcheck(addr string) int {
	port := "9701"
	if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
		port = p
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck failed:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck failed: status", resp.StatusCode)
		return 1
	}
	return 0
}
