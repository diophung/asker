// Command control-plane is the tenant-scoped metadata service: tenants,
// connector instances, sync cursors, and the encrypted token vault, all in
// Postgres. It serves the asker.controlplane.v1.ControlPlaneService gRPC API
// on CONTROL_PLANE_ADDR and plain HTTP health endpoints on
// CONTROL_PLANE_HEALTH_ADDR.
//
// Trust model (ADR-009 direction): the tenant identity arrives as
// x-asker-tenant gRPC metadata, NOT from a verified JWT here. That metadata
// is trusted because only the gateway and connector-hub — which derive it
// from a VERIFIED Keycloak JWT — can reach this service on the compose/
// cluster-internal network; it is never exposed on a host or public port.
// tenancygrpc.UnaryServerInterceptor still re-validates the value against the
// tenant syntax allowlist and fails closed, and M4 adds mTLS so the network
// assumption becomes a cryptographic one.
//
// Observability gap (M4): otelgrpc server instrumentation is not wired
// because the dependency is not in go.mod for M1; only the HTTP health
// endpoints carry telemetry middleware.
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

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/asker/asker/platform/crypto"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/telemetry"
)

const serviceName = "control-plane"

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
		logger.Error("control-plane exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg controlPlaneConfig, logger *slog.Logger) error {
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

	if err := runMigrations(ctx, cfg.DatabaseURL, logger); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("create postgres pool: %w", err)
	}
	defer pool.Close()

	kek, err := crypto.NewFileKEK(cfg.KEKFile)
	if err != nil {
		return fmt.Errorf("load KEK %s: %w", cfg.KEKFile, err)
	}
	cipher := crypto.NewTenantCipher(kek, newPGDEKStore(pool))

	store := newPGStore(pool)
	srv := newServer(store, cipher, logger)

	// SchedulerService.ListAllInstances is the single cross-tenant RPC and is
	// exempted from the tenant-metadata requirement by exact method name; all
	// ControlPlaneService RPCs keep the fail-closed tenancy interceptor.
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(tenantInterceptorSkipping(
			controlplanev1.SchedulerService_ListAllInstances_FullMethodName,
		)),
	)
	controlplanev1.RegisterControlPlaneServiceServer(grpcServer, srv)
	controlplanev1.RegisterSchedulerServiceServer(grpcServer, newSchedulerServer(store, logger))

	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}

	healthSrv := &http.Server{
		Addr:              cfg.HealthAddr,
		Handler:           newHealthHandler(pool),
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
	port := "9101"
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
