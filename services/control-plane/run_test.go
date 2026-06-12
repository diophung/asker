package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy/tenancygrpc"
)

// freeAddr grabs an ephemeral 127.0.0.1 port. The tiny close-then-reuse race
// is acceptable in an opt-in local integration test.
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

// TestRunEndToEnd boots the whole service (migrations, pgxpool, FileKEK,
// gRPC + interceptor, health server) against a scratch database, talks to it
// over the wire exactly like the connector-hub will (client interceptor
// injecting x-asker-tenant), proves a metadata-less call is rejected, and
// shuts down gracefully.
func TestRunEndToEnd(t *testing.T) {
	scratchURL := newScratchDB(t)

	cfg := controlPlaneConfig{
		Addr:        freeAddr(t),
		HealthAddr:  freeAddr(t),
		DatabaseURL: scratchURL,
		KEKFile:     filepath.Join(t.TempDir(), "kek.bin"),
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

	// Wait for readiness (migrations + listeners up).
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
	client := controlplanev1.NewControlPlaneServiceClient(conn)

	tctx := tenantCtx(t, "tenant-e2e")
	ensured, err := client.EnsureTenant(tctx, &controlplanev1.EnsureTenantRequest{})
	if err != nil {
		t.Fatalf("EnsureTenant over the wire: %v", err)
	}
	if ensured.GetTenant().GetTenantId() != "tenant-e2e" {
		t.Errorf("tenant_id = %q, want tenant-e2e", ensured.GetTenant().GetTenantId())
	}

	created, err := client.CreateConnectorInstance(tctx, &controlplanev1.CreateConnectorInstanceRequest{
		ConnectorId: "gmail",
	})
	if err != nil {
		t.Fatalf("CreateConnectorInstance over the wire: %v", err)
	}
	state, err := client.GetSyncState(tctx, &controlplanev1.GetSyncStateRequest{
		ConnectorInstanceId: created.GetInstance().GetId(),
	})
	if err != nil {
		t.Fatalf("GetSyncState over the wire: %v", err)
	}
	if state.GetState().GetPhase() != controlplanev1.SyncPhase_PENDING {
		t.Errorf("phase = %v, want PENDING", state.GetState().GetPhase())
	}

	// A tenant-less call must be stopped client-side by the interceptor...
	_, err = client.EnsureTenant(context.Background(), &controlplanev1.EnsureTenantRequest{})
	wantCode(t, err, codes.FailedPrecondition)

	// ...and a connection WITHOUT the client interceptor (no metadata at all)
	// must be rejected server-side.
	rawConn, err := grpc.NewClient(cfg.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient (raw): %v", err)
	}
	defer func() { _ = rawConn.Close() }()
	_, err = controlplanev1.NewControlPlaneServiceClient(rawConn).
		EnsureTenant(context.Background(), &controlplanev1.EnsureTenantRequest{})
	wantCode(t, err, codes.Unauthenticated)
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("service never became ready at %s: %v", url, lastErr)
}
