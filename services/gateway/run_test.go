package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestRunServesUntilCancelled boots the full run() path (telemetry no-op,
// lazy backend clients, HTTP server) against a free port, probes /healthz,
// then cancels the context and expects a clean graceful shutdown.
func TestRunServesUntilCancelled(t *testing.T) {
	idp := newTestIdP(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	cfg := testGatewayConfig(idp.jwks.URL)
	cfg.Addr = addr

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, discardLogger()) }()

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("run exited early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway did not become healthy in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not shut down after cancel")
	}
}

// TestNewDepsIsLazy: constructing the production dependency set must perform
// no I/O — unreachable backends cannot block or fail startup.
func TestNewDepsIsLazy(t *testing.T) {
	cfg := testGatewayConfig("http://127.0.0.1:1/certs")
	start := time.Now()
	d, cleanup, err := newDeps(cfg, discardLogger())
	if err != nil {
		t.Fatalf("newDeps: %v", err)
	}
	defer cleanup()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("newDeps took %v, want non-blocking construction", elapsed)
	}
	if d.query == nil || d.control == nil || d.counter == nil || d.hubClient == nil {
		t.Error("newDeps left a dependency nil")
	}
	if d.maxUploadBytes != cfg.MaxUploadMB<<20 {
		t.Errorf("maxUploadBytes = %d, want %d", d.maxUploadBytes, cfg.MaxUploadMB<<20)
	}
	if d.hubURL != cfg.HubHTTPURL {
		t.Errorf("hubURL = %q, want %q", d.hubURL, cfg.HubHTTPURL)
	}
}
