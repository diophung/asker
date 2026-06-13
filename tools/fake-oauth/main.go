// Command fake-oauth runs a standards-correct fake OAuth 2.0 authorization
// server for dev and CI. It is NEVER deployed to production: it auto-consents
// (no login UI) so the whole Asker connector OAuth flow runs without real
// Google/Microsoft/Slack/Atlassian credentials. See the server package for the
// endpoint surface; dev points ASKER_OAUTH_<P>_AUTH_URL / _TOKEN_URL at it.
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
	"github.com/asker/asker/tools/fake-oauth/server"
)

type appConfig struct {
	// Addr is the listen address; the contract port for compose is :9500.
	Addr string `env:"ADDR" envDefault:":9500"`
	// Subject is the resource-owner identity auto-consent uses when an
	// authorize request carries no login_hint.
	Subject string `env:"SUBJECT" envDefault:"alice@example.com"`
	// CodeTTL bounds how long a minted authorization code is valid.
	CodeTTL time.Duration `env:"CODE_TTL" envDefault:"5m"`
}

// loadInto fills cfg from FAKE_OAUTH_-prefixed environment variables.
func loadInto(cfg *appConfig) error {
	return config.Load("FAKE_OAUTH_", cfg)
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
		logger.Error("fake-oauth exited", "error", err)
		os.Exit(1)
	}
}

// run starts the HTTP server and blocks until ctx is canceled or SIGINT/
// SIGTERM arrives, then shuts down gracefully. If ready is non-nil the bound
// address is sent on it once the listener is up (used by tests with :0).
func run(ctx context.Context, cfg appConfig, logger *slog.Logger, ready chan<- string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	fake := server.New(
		server.WithLogger(logger),
		server.WithDefaultSubject(cfg.Subject),
		server.WithCodeTTL(cfg.CodeTTL),
	)

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
	logger.Info("fake-oauth listening", "addr", ln.Addr().String(), "subject", fake.DefaultSubject())
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
	logger.Info("fake-oauth stopped")
	return <-errCh
}

// runHealthcheck probes the local /healthz endpoint; the distroless image has
// no shell or curl, so the compose healthcheck runs the binary itself with
// -healthcheck (same pattern as fake-gmail and the gateway).
func runHealthcheck(addr string) int {
	port := "9500"
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
