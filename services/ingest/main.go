// Command ingest is the parse/dedupe/chunk stage of the M1 pipeline: it
// consumes canonical Documents from docs.raw (consumer group "ingest"),
// passes tombstones through untouched, normalizes everything else (whitespace
// trim + derived version_etag), suppresses replays via a Redis SETNX dedupe
// key (failing OPEN when Redis is down — downstream upserts are idempotent),
// cuts the body into retrieval chunks per the pinned chunking policy, and
// produces the result to docs.chunked.
//
// Tenancy: the kafkautil consumer reconstructs the tenancy.Context from the
// record's tenant_id header (failing closed to docs.deadletter), and the
// kafkautil producer re-validates it against doc.TenantId on the way out.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/asker/asker/platform/kafkautil"
	"github.com/asker/asker/platform/telemetry"
)

const (
	serviceName = "ingest"
	// consumerGroup is the pinned consumer-group id on docs.raw.
	consumerGroup = "ingest"
	// topicPartitions is the dev partition count (ADR-004: 4 in dev).
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
		logger.Error("ingest exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg ingestConfig, logger *slog.Logger) error {
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

	// Idempotent topic creation at every producer/consumer service start.
	if err := kafkautil.EnsureTopics(ctx, cfg.Kafka, topicPartitions,
		kafkautil.TopicDocsRaw, kafkautil.TopicDocsChunked,
		kafkautil.TopicDocsEnriched, kafkautil.TopicDocsDeadletter); err != nil {
		return fmt.Errorf("ensure topics: %w", err)
	}

	producer, err := kafkautil.NewProducer(cfg.Kafka)
	if err != nil {
		return fmt.Errorf("create producer: %w", err)
	}
	defer producer.Close()

	seen := newRedisSeen(cfg.RedisAddr)
	defer func() { _ = seen.Close() }()

	consumer, err := kafkautil.NewConsumer(cfg.Kafka, consumerGroup, kafkautil.TopicDocsRaw)
	if err != nil {
		return fmt.Errorf("create consumer: %w", err)
	}
	defer consumer.Close()

	h := newHandler(producer, seen, logger)
	pm := newPipelineMetrics()
	// The ingest stage consumes docs.raw and produces docs.chunked; the metric
	// topic label is the stage's output topic (TopicDocsChunked).
	handle := pm.instrument(serviceName, kafkautil.TopicDocsChunked, h.Handle)

	var ready atomic.Bool
	healthSrv := &http.Server{
		Addr:              cfg.HealthAddr,
		Handler:           newHealthHandler(&ready),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("health server listening", "addr", cfg.HealthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("health serve: %w", err)
			return
		}
		errCh <- nil
	}()
	go func() {
		logger.Info("consuming", "topic", kafkautil.TopicDocsRaw, "group", consumerGroup, "redis", cfg.RedisAddr)
		ready.Store(true)
		if err := consumer.Run(ctx, handle); err != nil {
			errCh <- fmt.Errorf("consumer run: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		// One side failed; tear the other down and report the first error.
		ready.Store(false)
		consumer.Close() // Run returns nil once the client is closed
		_ = healthSrv.Close()
		<-errCh
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received; draining")
		ready.Store(false)
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := healthSrv.Shutdown(shCtx); err != nil {
			logger.Warn("health server shutdown", "error", err)
		}
		// consumer.Run sees the canceled ctx and returns nil after finishing
		// the in-flight record (graceful drain; uncommitted records redeliver).
		firstErr := <-errCh
		if secondErr := <-errCh; firstErr == nil {
			firstErr = secondErr
		}
		return firstErr
	}
}

// runHealthcheck probes the local /healthz endpoint and returns a process
// exit code. It is the container healthcheck for the distroless image, which
// has no shell or curl (same pattern as the gateway).
func runHealthcheck(addr string) int {
	port := "9501"
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
