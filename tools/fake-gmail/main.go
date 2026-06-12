// Command fake-gmail runs an in-memory fake of the Gmail REST v1 API for
// dev and CI (ADR-008). It is never deployed to production. See the server
// package for the API surface (Gmail subset + unauthenticated /admin API).
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

	"github.com/asker/asker/platform/config"
	"github.com/asker/asker/tools/fake-gmail/server"
)

type appConfig struct {
	// Addr is the listen address; the contract port for compose is :9400.
	Addr string `env:"ADDR" envDefault:":9400"`
}

// loadInto fills cfg from FAKE_GMAIL_-prefixed environment variables.
func loadInto(cfg *appConfig) error {
	return config.Load("FAKE_GMAIL_", cfg)
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit 0/1 (container healthcheck)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	var cfg appConfig
	if err := loadInto(&cfg); err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if *healthcheck {
		os.Exit(runHealthcheck(cfg.Addr))
	}

	if err := run(context.Background(), cfg, logger, nil); err != nil {
		logger.Error("fake-gmail exited", "error", err)
		os.Exit(1)
	}
}

// run starts the HTTP server and blocks until ctx is canceled or SIGINT/
// SIGTERM arrives, then shuts down gracefully. If ready is non-nil the bound
// address is sent on it once the listener is up (used by tests with :0).
func run(ctx context.Context, cfg appConfig, logger *slog.Logger, ready chan<- string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	fake := server.New(server.WithLogger(logger))
	defer fake.Close()

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}

	srv := &http.Server{
		Handler:           fake,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
		close(errCh)
	}()
	logger.Info("fake-gmail listening", "addr", ln.Addr().String())
	if ready != nil {
		ready <- ln.Addr().String()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Info("fake-gmail stopped")
	return <-errCh
}

// runHealthcheck probes the local /healthz endpoint; the distroless image
// has no shell or curl, so the compose healthcheck runs the binary itself
// with -healthcheck (same pattern as the gateway).
func runHealthcheck(addr string) int {
	port := "9400"
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
