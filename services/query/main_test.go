package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	"github.com/asker/asker/platform/tenancy/tenancygrpc"
)

// freeAddr grabs an ephemeral 127.0.0.1 port. The tiny close-then-reuse race
// is acceptable in a local test.
func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never became ready", url)
}

// TestRunEndToEnd boots the whole service (config, telemetry no-op, gRPC with
// the tenant interceptor, health server with the Vespa readiness probe) and
// talks to it over the wire exactly like the gateway will. Redis points at a
// dead port: the cache must be skipped silently, not break searches.
func TestRunEndToEnd(t *testing.T) {
	tei, _ := newTEIStub(t, testDim)
	clip, _ := newClipStub(t, testDim)
	vespa := newVespaStub(t)
	deadRedis := freeAddr(t)

	cfg := queryConfig{
		Addr:         freeAddr(t),
		HealthAddr:   freeAddr(t),
		TEIURL:       tei.URL,
		VespaURL:     vespa.srv.URL,
		RedisAddr:    deadRedis,
		EmbeddingDim: testDim,
		EmbedTimeout: time.Second,
		ClipURL:      clip.URL,
		ClipDim:      testDim,
		ClipTimeout:  time.Second,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, logger) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run returned error on shutdown: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("run did not shut down within 15s")
		}
	}()

	waitReady(t, "http://"+cfg.HealthAddr+"/readyz")

	// Container healthcheck self-probe path.
	if code := runHealthcheck(cfg.HealthAddr); code != 0 {
		t.Errorf("runHealthcheck = %d, want 0", code)
	}

	conn, err := grpc.NewClient(cfg.Addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(tenancygrpc.UnaryClientInterceptor()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer func() { _ = conn.Close() }()
	client := queryv1.NewQueryServiceClient(conn)

	resp, err := client.Search(tenantCtx(t, "tenant-e2e"), &queryv1.SearchRequest{Query: "quarterly plans"})
	if err != nil {
		t.Fatalf("Search over the wire: %v", err)
	}
	if len(resp.GetHits()) == 0 {
		t.Error("Search returned no hits")
	}
	if got := vespa.lastBody(t)["streaming.groupname"]; got != "tenant-e2e" {
		t.Errorf("streaming.groupname = %v, want tenant-e2e", got)
	}

	// A metadata-less connection is rejected server-side.
	rawConn, err := grpc.NewClient(cfg.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient (raw): %v", err)
	}
	defer func() { _ = rawConn.Close() }()
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	_, err = queryv1.NewQueryServiceClient(rawConn).Search(rctx, &queryv1.SearchRequest{Query: "plans"})
	wantCode(t, err, codes.Unauthenticated)
}

func TestRunHealthcheckFailure(t *testing.T) {
	if code := runHealthcheck(freeAddr(t)); code == 0 {
		t.Error("runHealthcheck against nothing = 0, want non-zero")
	}
}

func TestHealthHandler(t *testing.T) {
	vespa := newVespaStub(t)
	srv := httptest.NewServer(newHealthHandler(vespa.srv.URL))
	defer srv.Close()
	client := srv.Client()

	for path, want := range map[string]int{
		"/healthz": http.StatusOK,
		"/readyz":  http.StatusOK,
		"/nope":    http.StatusNotFound,
	} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}

	// Vespa down: liveness stays up, readiness flips.
	vespa.srv.Close()
	resp, err := client.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz with Vespa down = %d, want 503", resp.StatusCode)
	}
	resp, err = client.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz with Vespa down = %d, want 200", resp.StatusCode)
	}
}

func TestLoadConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.Addr != ":9200" || cfg.HealthAddr != ":9201" {
			t.Errorf("addrs = %q/%q, want :9200/:9201", cfg.Addr, cfg.HealthAddr)
		}
		if cfg.EmbeddingDim != 1024 {
			t.Errorf("EmbeddingDim = %d, want 1024", cfg.EmbeddingDim)
		}
		if cfg.EmbedTimeout != 2*time.Second {
			t.Errorf("EmbedTimeout = %v, want 2s", cfg.EmbedTimeout)
		}
		if cfg.TEIURL != "http://tei:80" || cfg.VespaURL != "http://vespa:8080" || cfg.RedisAddr != "redis:6379" {
			t.Errorf("endpoints = %q/%q/%q, want contract defaults", cfg.TEIURL, cfg.VespaURL, cfg.RedisAddr)
		}
		if cfg.ClipURL != "http://clip:9800" {
			t.Errorf("ClipURL = %q, want http://clip:9800", cfg.ClipURL)
		}
		if cfg.ClipDim != 512 {
			t.Errorf("ClipDim = %d, want 512", cfg.ClipDim)
		}
		if cfg.ClipTimeout != 2*time.Second {
			t.Errorf("ClipTimeout = %v, want 2s", cfg.ClipTimeout)
		}
	})
	t.Run("env overrides", func(t *testing.T) {
		t.Setenv("EMBEDDING_DIM", "384")
		t.Setenv("QUERY_EMBED_TIMEOUT", "750ms")
		t.Setenv("CLIP_URL", "http://clip-host:9800")
		t.Setenv("CLIP_DIM", "512")
		t.Setenv("QUERY_CLIP_TIMEOUT", "1500ms")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.EmbeddingDim != 384 {
			t.Errorf("EmbeddingDim = %d, want 384", cfg.EmbeddingDim)
		}
		if cfg.EmbedTimeout != 750*time.Millisecond {
			t.Errorf("EmbedTimeout = %v, want 750ms", cfg.EmbedTimeout)
		}
		if cfg.ClipURL != "http://clip-host:9800" {
			t.Errorf("ClipURL = %q, want override", cfg.ClipURL)
		}
		if cfg.ClipDim != 512 {
			t.Errorf("ClipDim = %d, want 512", cfg.ClipDim)
		}
		if cfg.ClipTimeout != 1500*time.Millisecond {
			t.Errorf("ClipTimeout = %v, want 1500ms", cfg.ClipTimeout)
		}
	})
	t.Run("invalid dim rejected", func(t *testing.T) {
		t.Setenv("EMBEDDING_DIM", "0")
		if _, err := loadConfig(); err == nil {
			t.Error("loadConfig accepted EMBEDDING_DIM=0")
		}
	})
	t.Run("invalid timeout rejected", func(t *testing.T) {
		t.Setenv("QUERY_EMBED_TIMEOUT", "0s")
		if _, err := loadConfig(); err == nil {
			t.Error("loadConfig accepted QUERY_EMBED_TIMEOUT=0s")
		}
	})
	t.Run("invalid clip dim rejected", func(t *testing.T) {
		t.Setenv("CLIP_DIM", "0")
		if _, err := loadConfig(); err == nil {
			t.Error("loadConfig accepted CLIP_DIM=0")
		}
	})
	t.Run("invalid clip timeout rejected", func(t *testing.T) {
		t.Setenv("QUERY_CLIP_TIMEOUT", "0s")
		if _, err := loadConfig(); err == nil {
			t.Error("loadConfig accepted QUERY_CLIP_TIMEOUT=0s")
		}
	})
}
