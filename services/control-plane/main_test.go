package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Addr != ":9100" {
		t.Errorf("Addr = %q, want :9100", cfg.Addr)
	}
	if cfg.HealthAddr != ":9101" {
		t.Errorf("HealthAddr = %q, want :9101", cfg.HealthAddr)
	}
	if cfg.DatabaseURL != "postgres://asker:asker@postgres:5432/asker" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.KEKFile != "/keys/kek.bin" {
		t.Errorf("KEKFile = %q, want /keys/kek.bin", cfg.KEKFile)
	}
}

func TestLoadConfigEnvOverride(t *testing.T) {
	t.Setenv("CONTROL_PLANE_ADDR", ":7777")
	t.Setenv("DATABASE_URL", "postgres://u:p@h:5432/db")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Addr != ":7777" || cfg.DatabaseURL != "postgres://u:p@h:5432/db" {
		t.Errorf("env override not applied: %+v", cfg)
	}
}

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestHealthEndpoints(t *testing.T) {
	h := newHealthHandler(fakePinger{})
	bad := newHealthHandler(fakePinger{err: errors.New("db down")})

	cases := []struct {
		name       string
		handler    http.Handler
		method     string
		path       string
		wantStatus int
	}{
		{"healthz ok", h, http.MethodGet, "/healthz", http.StatusOK},
		{"readyz ready", h, http.MethodGet, "/readyz", http.StatusOK},
		{"readyz db down", bad, http.MethodGet, "/readyz", http.StatusServiceUnavailable},
		{"healthz wrong method", h, http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{"unknown path", h, http.MethodGet, "/nope", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

func TestRunHealthcheck(t *testing.T) {
	srv := httptest.NewServer(newHealthHandler(fakePinger{}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}

	if code := runHealthcheck(":" + port); code != 0 {
		t.Errorf("healthy probe exit = %d, want 0", code)
	}

	srv.Close()
	if code := runHealthcheck(":" + port); code != 1 {
		t.Errorf("dead probe exit = %d, want 1", code)
	}
}

func TestRunHealthcheckNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	if code := runHealthcheck(":" + port); code != 1 {
		t.Errorf("unhealthy probe exit = %d, want 1", code)
	}
}

func TestRunMigrationsContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := runMigrations(ctx, "postgres://nobody@127.0.0.1:1/none", logger)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestRunMigrationsUnreachableDB(t *testing.T) {
	// Short-circuit the retry loop with a context deadline so the test does
	// not sit through the full backoff schedule.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := runMigrations(ctx, "postgres://nobody:nope@127.0.0.1:1/none?sslmode=disable&connect_timeout=1", logger)
	if err == nil {
		t.Fatal("runMigrations against unreachable DB succeeded, want error")
	}
}
