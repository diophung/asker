package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/platform/kafkautil"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/telemetry"
	"github.com/asker/asker/platform/tenancy"
	"github.com/asker/asker/platform/tenancy/tenancygrpc"
)

const serviceName = "connector-hub"

// topicPartitions is the dev/CI partition count (ADR-004 §6; prod sizing is
// M5 work in docs/capacity.md).
const topicPartitions = 4

// UploadFunc stores an uploaded file as a blob and builds the canonical
// Document for it. It is the hub-side seam for the upload connector's
// pinned HandleUpload helper, wired in package main so the hub core stays
// independent of connector packages.
type UploadFunc func(ctx context.Context, tenant tenancy.Context, file io.Reader, filename, title, contentType string, size int64) (*askerv1.Document, error)

// Deps are the connector-facing pieces package main wires in.
type Deps struct {
	// Registry catalogs the in-process connectors (gmail, upload).
	Registry *sdk.Registry
	// Upload backs POST /upload.
	Upload UploadFunc
}

func (d Deps) validate() error {
	if d.Registry == nil {
		return errors.New("hub: Deps.Registry is required")
	}
	if d.Upload == nil {
		return errors.New("hub: Deps.Upload is required")
	}
	return nil
}

// Run boots the connector hub and blocks until ctx is canceled or
// SIGINT/SIGTERM arrives, then shuts down gracefully: HTTP listeners first,
// then the scheduler, draining in-flight sync workers.
func Run(ctx context.Context, cfg Config, deps Deps, logger *slog.Logger) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	if err := deps.validate(); err != nil {
		return err
	}

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

	// Idempotent at every producer/consumer service start (shared contract).
	if err := kafkautil.EnsureTopics(ctx, cfg.Kafka, topicPartitions,
		kafkautil.TopicDocsRaw, kafkautil.TopicDocsChunked,
		kafkautil.TopicDocsEnriched, kafkautil.TopicDocsDeadletter,
	); err != nil {
		return fmt.Errorf("ensure topics: %w", err)
	}
	producer, err := kafkautil.NewProducer(cfg.Kafka)
	if err != nil {
		return fmt.Errorf("create kafka producer: %w", err)
	}
	defer producer.Close()

	// Two control-plane connections, deliberately: tenant-scoped RPCs go
	// through the tenancy client interceptor (fail closed without a tenant);
	// the scheduler's cross-tenant ListAllInstances goes through a bare
	// connection because it has no single-tenant scope to assert.
	cpConn, err := grpc.NewClient(cfg.ControlPlaneAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(tenancygrpc.UnaryClientInterceptor()),
	)
	if err != nil {
		return fmt.Errorf("dial control-plane %s: %w", cfg.ControlPlaneAddr, err)
	}
	defer func() { _ = cpConn.Close() }()

	schedConn, err := grpc.NewClient(cfg.ControlPlaneAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("dial control-plane (scheduler) %s: %w", cfg.ControlPlaneAddr, err)
	}
	defer func() { _ = schedConn.Close() }()

	cp := controlplanev1.NewControlPlaneServiceClient(cpConn)
	schedClient := controlplanev1.NewSchedulerServiceClient(schedConn)

	em := newEmitter(producer, kafkautil.TopicDocsRaw, time.Now)
	sch := newScheduler(schedulerOpts{
		cp:           cp,
		sched:        schedClient,
		registry:     deps.Registry,
		emit:         em,
		logger:       logger,
		webhookBase:  cfg.WebhookBase,
		syncInterval: cfg.SyncInterval,
		tick:         cfg.SchedulerTick,
	})

	api := &httpAPI{
		cp:       cp,
		registry: deps.Registry,
		sched:    sch,
		emit:     em,
		upload:   deps.Upload,
		logger:   logger,
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       2 * time.Minute, // 32 MiB uploads over slow links
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	healthSrv := &http.Server{
		Addr: cfg.HealthAddr,
		Handler: newHealthHandler(readiness{
			controlPlane: controlPlaneProbe(schedClient),
			kafkaReady:   func() bool { return true }, // EnsureTopics + NewProducer succeeded above
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	schedDone := make(chan struct{})
	go func() {
		sch.Run(ctx)
		close(schedDone)
	}()

	errCh := make(chan error, 2)
	go func() {
		logger.Info("hub server listening", "addr", cfg.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("hub serve: %w", err)
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
		// One server failed; tear everything down and report.
		_ = httpSrv.Close()
		_ = healthSrv.Close()
		<-errCh
		stop()
		<-schedDone
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if err := httpSrv.Shutdown(shCtx); err != nil {
			logger.Warn("hub server shutdown", "error", err)
		}
		if err := healthSrv.Shutdown(shCtx); err != nil {
			logger.Warn("health server shutdown", "error", err)
		}
		// The scheduler's context is ctx: cancellation has already reached
		// every sync worker; wait for them to drain.
		select {
		case <-schedDone:
		case <-shCtx.Done():
			logger.Warn("sync workers did not drain before shutdown deadline")
		}

		firstErr := <-errCh
		if secondErr := <-errCh; firstErr == nil {
			firstErr = secondErr
		}
		return firstErr
	}
}
