// Command query is the Asker search service: it executes tenant-scoped
// hybrid (keyword + vector) searches against Vespa with graceful degradation
// to keyword-only when TEI or the hybrid profile fails (spec §2.6, ADR-006).
//
// Trust model (ADR-009): the tenant identity arrives as x-asker-tenant gRPC
// metadata installed by the gateway — which derived it from a VERIFIED
// Keycloak JWT — and is re-validated and fail-closed by
// tenancygrpc.UnaryServerInterceptor. The tenant is NEVER read from request
// fields, and every Vespa query is scoped to that tenant's streaming group.
//
// Observability gap (M4): otelgrpc server instrumentation is not wired
// because the dependency is not in go.mod for M1; only the HTTP health
// endpoints carry telemetry middleware (same as control-plane).
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
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	"github.com/asker/asker/platform/telemetry"
	"github.com/asker/asker/platform/tenancy/tenancygrpc"
)

const serviceName = "query"

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit 0/1 (container healthcheck)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", serviceName)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if *healthcheck {
		os.Exit(runHealthcheck(cfg.HealthAddr))
	}

	if err := run(context.Background(), cfg, logger); err != nil {
		logger.Error("query exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg queryConfig, logger *slog.Logger) error {
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

	logger.Info("query service configuration",
		"vespa_url", cfg.VespaURL, "tei_url", cfg.TEIURL,
		"embedding_dim", cfg.EmbeddingDim, "embed_timeout", cfg.EmbedTimeout,
		"clip_url", cfg.ClipURL, "clip_dim", cfg.ClipDim, "clip_timeout", cfg.ClipTimeout,
		"redis_addr", cfg.RedisAddr)

	cache := newRedisCache(cfg.RedisAddr)
	defer cache.Close()

	srv := newServer(
		newTEIEmbedder(cfg.TEIURL, cfg.EmbeddingDim, cfg.EmbedTimeout),
		newClipEmbedder(cfg.ClipURL, cfg.ClipDim, cfg.ClipTimeout),
		newVespaClient(cfg.VespaURL),
		cache,
		logger,
	)

	// otelgrpc stats handler records RPC-level RED metrics + traces (the M4-noted
	// gap). It uses the global meter/tracer providers, so it is no-op-safe when
	// telemetry was initialized without an endpoint. The tenancy interceptor
	// still runs first in the unary chain — instrumentation never sees the
	// tenant and adds no high-cardinality labels.
	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(tenancygrpc.UnaryServerInterceptor()),
	)
	queryv1.RegisterQueryServiceServer(grpcServer, srv)

	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}

	healthSrv := &http.Server{
		Addr:              cfg.HealthAddr,
		Handler:           newHealthHandler(cfg.VespaURL),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("gRPC server listening", "addr", cfg.Addr)
		if err := grpcServer.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			errCh <- fmt.Errorf("grpc serve: %w", err)
			return
		}
		errCh <- nil
	}()
	go func() {
		logger.Info("health server listening", "addr", cfg.HealthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("health serve: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		// One server failed; tear the other down and report.
		grpcServer.Stop()
		_ = healthSrv.Close()
		<-errCh
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := healthSrv.Shutdown(shCtx); err != nil {
			logger.Warn("health server shutdown", "error", err)
		}
		gracefulStop(shCtx, grpcServer, logger)

		firstErr := <-errCh
		if secondErr := <-errCh; firstErr == nil {
			firstErr = secondErr
		}
		return firstErr
	}
}

// gracefulStop drains in-flight RPCs, falling back to a hard stop when the
// shutdown context expires first.
func gracefulStop(ctx context.Context, grpcServer *grpc.Server, logger *slog.Logger) {
	done := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		logger.Warn("graceful stop timed out; forcing stop")
		grpcServer.Stop()
		<-done
	}
}

// runHealthcheck probes the local /healthz endpoint and returns a process
// exit code. It is the container healthcheck for the distroless image, which
// has no shell or curl (same pattern as the gateway).
func runHealthcheck(addr string) int {
	port := "9201"
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
