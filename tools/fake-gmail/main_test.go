package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRunServesAndShutsDownGracefully(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, appConfig{Addr: "127.0.0.1:0"}, logger, ready)
	}()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start within 5s")
	}

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status %d, want 200", resp.StatusCode)
	}

	// The -healthcheck self-probe succeeds against the live server.
	if code := runHealthcheck(addr); code != 0 {
		t.Fatalf("runHealthcheck = %d, want 0", code)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error on graceful shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not shut down within 10s")
	}
}

func TestRunListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err = run(context.Background(), appConfig{Addr: ln.Addr().String()}, logger, nil)
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("err = %v, want listen error", err)
	}
}

func TestRunHealthcheckFailure(t *testing.T) {
	// Grab a port and close it so nothing is listening there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if code := runHealthcheck(addr); code != 1 {
		t.Fatalf("runHealthcheck against dead port = %d, want 1", code)
	}
}

func TestRunHealthcheckBadStatus(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}),
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	if code := runHealthcheck(ln.Addr().String()); code != 1 {
		t.Fatalf("runHealthcheck against 503 = %d, want 1", code)
	}
}

func TestConfigDefaultsAndEnv(t *testing.T) {
	var cfg appConfig
	t.Setenv("FAKE_GMAIL_ADDR", "")
	if err := loadInto(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":9400" {
		t.Fatalf("default addr = %q, want :9400", cfg.Addr)
	}

	t.Setenv("FAKE_GMAIL_ADDR", "127.0.0.1:7777")
	var cfg2 appConfig
	if err := loadInto(&cfg2); err != nil {
		t.Fatal(err)
	}
	if cfg2.Addr != "127.0.0.1:7777" {
		t.Fatalf("addr = %q, want 127.0.0.1:7777", cfg2.Addr)
	}
}
