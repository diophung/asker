package tenancygrpc

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/asker/asker/platform/tenancy"
)

// captureHealth is a health service implementation that records the tenancy
// seen by the handler, proving the server interceptor installed it.
type captureHealth struct {
	grpc_health_v1.UnimplementedHealthServer

	mu       sync.Mutex
	calls    int
	tenants  []tenancy.TenantID
	subjects []string
}

func (h *captureHealth) Check(ctx context.Context, _ *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	tc, err := tenancy.FromContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "handler reached without tenancy: %v", err)
	}
	h.mu.Lock()
	h.calls++
	h.tenants = append(h.tenants, tc.TenantID())
	h.subjects = append(h.subjects, tc.Subject())
	h.mu.Unlock()
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

func (h *captureHealth) snapshot() (int, []tenancy.TenantID, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls, append([]tenancy.TenantID(nil), h.tenants...), append([]string(nil), h.subjects...)
}

// startServer runs a real gRPC server with UnaryServerInterceptor on a
// localhost listener and returns the capture handler and its address.
func startServer(t *testing.T) (*captureHealth, string) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	h := &captureHealth{}
	srv := grpc.NewServer(grpc.UnaryInterceptor(UnaryServerInterceptor()))
	grpc_health_v1.RegisterHealthServer(srv, h)

	go func() {
		// Serve returns a non-nil error after Stop; nothing to assert here.
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.Stop)

	return h, lis.Addr().String()
}

func dial(t *testing.T, addr string, opts ...grpc.DialOption) grpc_health_v1.HealthClient {
	t.Helper()

	opts = append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return grpc_health_v1.NewHealthClient(conn)
}

func tenantContext(t *testing.T, claims map[string]any) context.Context {
	t.Helper()

	tc, err := tenancy.FromClaims(claims)
	if err != nil {
		t.Fatalf("FromClaims: %v", err)
	}
	return tenancy.WithContext(context.Background(), tc)
}

func callCtx(parent context.Context, t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestClientInterceptorBlocksTenantlessCall(t *testing.T) {
	h, addr := startServer(t)
	client := dial(t, addr, grpc.WithUnaryInterceptor(UnaryClientInterceptor()))

	_, err := client.Check(callCtx(context.Background(), t), &grpc_health_v1.HealthCheckRequest{})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("Check() code = %v (err %v), want %v", got, err, codes.FailedPrecondition)
	}
	if calls, _, _ := h.snapshot(); calls != 0 {
		t.Fatalf("handler ran %d times, want 0: tenant-less RPC must never reach the wire", calls)
	}
}

func TestRoundTripDeliversTenantToHandler(t *testing.T) {
	h, addr := startServer(t)
	client := dial(t, addr, grpc.WithUnaryInterceptor(UnaryClientInterceptor()))

	ctx := tenantContext(t, map[string]any{"tenant_id": "acme", "sub": "user-123"})
	resp, err := client.Check(callCtx(ctx, t), &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Check() unexpected error: %v", err)
	}
	if resp.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("Check() status = %v, want SERVING", resp.GetStatus())
	}

	calls, tenants, subjects := h.snapshot()
	if calls != 1 {
		t.Fatalf("handler ran %d times, want 1", calls)
	}
	if tenants[0] != "acme" {
		t.Errorf("handler saw tenant %q, want %q", tenants[0], "acme")
	}
	// The metadata value carries no subject, so the server-side Context's
	// Subject() collapses to the tenant.
	if subjects[0] != "acme" {
		t.Errorf("handler saw subject %q, want %q", subjects[0], "acme")
	}
}

func TestServerRejectsMissingTenantMetadata(t *testing.T) {
	h, addr := startServer(t)
	client := dial(t, addr) // no client interceptor: nothing injects the tenant

	_, err := client.Check(callCtx(context.Background(), t), &grpc_health_v1.HealthCheckRequest{})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("Check() code = %v (err %v), want %v", got, err, codes.Unauthenticated)
	}
	if calls, _, _ := h.snapshot(); calls != 0 {
		t.Fatalf("handler ran %d times, want 0", calls)
	}
}

func TestServerRejectsMultipleTenantMetadataValues(t *testing.T) {
	h, addr := startServer(t)
	client := dial(t, addr)

	ctx := metadata.AppendToOutgoingContext(context.Background(),
		MetadataKey, "acme",
		MetadataKey, "other",
	)
	_, err := client.Check(callCtx(ctx, t), &grpc_health_v1.HealthCheckRequest{})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("Check() code = %v (err %v), want %v", got, err, codes.Unauthenticated)
	}
	if calls, _, _ := h.snapshot(); calls != 0 {
		t.Fatalf("handler ran %d times, want 0: ambiguous tenant must not reach the handler", calls)
	}
}

func TestServerRejectsInvalidTenantMetadataValue(t *testing.T) {
	h, addr := startServer(t)
	client := dial(t, addr)

	// Values must be printable ASCII or grpc-go itself refuses to send the
	// metadata; non-ASCII forgeries are covered by tenancy's header tests.
	for _, bad := range []string{"acme/evil", "..", "acme evil", strings.Repeat("a", 129)} {
		ctx := metadata.AppendToOutgoingContext(context.Background(), MetadataKey, bad)
		_, err := client.Check(callCtx(ctx, t), &grpc_health_v1.HealthCheckRequest{})
		if got := status.Code(err); got != codes.Unauthenticated {
			t.Fatalf("Check() with value %q: code = %v (err %v), want %v", bad, got, err, codes.Unauthenticated)
		}
	}
	if calls, _, _ := h.snapshot(); calls != 0 {
		t.Fatalf("handler ran %d times, want 0", calls)
	}
}

// TestClientInjectionMakesForgedMetadataAmbiguous documents the fail-closed
// interplay of the two interceptors: if a caller smuggles its own
// x-asker-tenant value into outgoing metadata, the client interceptor still
// appends the authentic one, and the server rejects the now-ambiguous pair
// rather than picking either.
func TestClientInjectionMakesForgedMetadataAmbiguous(t *testing.T) {
	h, addr := startServer(t)
	client := dial(t, addr, grpc.WithUnaryInterceptor(UnaryClientInterceptor()))

	ctx := tenantContext(t, map[string]any{"tenant_id": "acme"})
	ctx = metadata.AppendToOutgoingContext(ctx, MetadataKey, "victim-tenant")

	_, err := client.Check(callCtx(ctx, t), &grpc_health_v1.HealthCheckRequest{})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("Check() code = %v (err %v), want %v", got, err, codes.Unauthenticated)
	}
	if calls, _, _ := h.snapshot(); calls != 0 {
		t.Fatalf("handler ran %d times, want 0", calls)
	}
}

// TestServerInterceptorWithoutMetadataMap invokes the interceptor directly
// with a bare context: a real gRPC transport always installs an incoming
// metadata map, so this defensive branch is unreachable over the wire.
func TestServerInterceptorWithoutMetadataMap(t *testing.T) {
	interceptor := UnaryServerInterceptor()
	handlerRan := false
	_, err := interceptor(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"},
		func(ctx context.Context, req any) (any, error) {
			handlerRan = true
			return nil, nil
		},
	)
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("interceptor code = %v (err %v), want %v", got, err, codes.Unauthenticated)
	}
	if handlerRan {
		t.Fatal("handler ran despite missing metadata map")
	}
}
