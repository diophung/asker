package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy"
	"github.com/asker/asker/platform/tenancy/tenancygrpc"
)

// startGRPC boots a real gRPC server on 127.0.0.1:0 wired EXACTLY like
// production main.go: both services registered, with the wrapped interceptor
// exempting only SchedulerService/ListAllInstances. It returns the address.
func startGRPC(t *testing.T, srv *server, store Store) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(tenantInterceptorSkipping(
			controlplanev1.SchedulerService_ListAllInstances_FullMethodName,
		)),
	)
	controlplanev1.RegisterControlPlaneServiceServer(grpcServer, srv)
	controlplanev1.RegisterSchedulerServiceServer(grpcServer, newSchedulerServer(store, srv.logger))
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)
	return lis.Addr().String()
}

func dialCP(t *testing.T, addr string, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	opts = append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestSchedulerListAllInstancesWithoutTenantMetadata proves (a) the exempted
// RPC works over the wire with NO tenant metadata at all, and (c) its results
// span tenants.
func TestSchedulerListAllInstancesWithoutTenantMetadata(t *testing.T) {
	srv, store := testServer(t)
	addr := startGRPC(t, srv, store)

	// Seed instances in two different tenants through the tenant-scoped API.
	instA := mustCreateInstance(tenantCtx(t, tenantA), t, srv, "gmail")
	instB := mustCreateInstance(tenantCtx(t, tenantB), t, srv, "upload")

	// Raw connection: no client interceptor, no metadata — the scheduler has
	// no single-tenant scope.
	conn := dialCP(t, addr)
	sched := controlplanev1.NewSchedulerServiceClient(conn)

	resp, err := sched.ListAllInstances(testCtx(t), &controlplanev1.ListAllInstancesRequest{})
	if err != nil {
		t.Fatalf("ListAllInstances without tenant metadata: %v", err)
	}

	byInstance := make(map[string]string) // instance ID -> tenant ID
	for _, ti := range resp.GetInstances() {
		byInstance[ti.GetInstance().GetId()] = ti.GetTenantId()
	}
	if len(byInstance) != 2 {
		t.Fatalf("ListAllInstances returned %d instances, want 2: %v", len(byInstance), byInstance)
	}
	if got := byInstance[instA.GetId()]; got != tenantA {
		t.Errorf("instance %s tenant = %q, want %q", instA.GetId(), got, tenantA)
	}
	if got := byInstance[instB.GetId()]; got != tenantB {
		t.Errorf("instance %s tenant = %q, want %q", instB.GetId(), got, tenantB)
	}
}

// TestEveryOtherRPCStillRequiresTenantMetadata proves (b): the exemption is
// surgical. Every ControlPlaneService RPC, called over the wire with no
// tenant metadata, is rejected Unauthenticated by the interceptor.
func TestEveryOtherRPCStillRequiresTenantMetadata(t *testing.T) {
	srv, store := testServer(t)
	addr := startGRPC(t, srv, store)

	conn := dialCP(t, addr) // no metadata of any kind
	cp := controlplanev1.NewControlPlaneServiceClient(conn)
	id := uuid.NewString()

	calls := map[string]func(ctx context.Context) error{
		"EnsureTenant": func(ctx context.Context) error {
			_, err := cp.EnsureTenant(ctx, &controlplanev1.EnsureTenantRequest{})
			return err
		},
		"CreateConnectorInstance": func(ctx context.Context) error {
			_, err := cp.CreateConnectorInstance(ctx, &controlplanev1.CreateConnectorInstanceRequest{ConnectorId: "gmail"})
			return err
		},
		"ListConnectorInstances": func(ctx context.Context) error {
			_, err := cp.ListConnectorInstances(ctx, &controlplanev1.ListConnectorInstancesRequest{})
			return err
		},
		"GetConnectorInstance": func(ctx context.Context) error {
			_, err := cp.GetConnectorInstance(ctx, &controlplanev1.GetConnectorInstanceRequest{Id: id})
			return err
		},
		"DeleteConnectorInstance": func(ctx context.Context) error {
			_, err := cp.DeleteConnectorInstance(ctx, &controlplanev1.DeleteConnectorInstanceRequest{Id: id})
			return err
		},
		"GetSyncState": func(ctx context.Context) error {
			_, err := cp.GetSyncState(ctx, &controlplanev1.GetSyncStateRequest{ConnectorInstanceId: id})
			return err
		},
		"SetSyncState": func(ctx context.Context) error {
			_, err := cp.SetSyncState(ctx, &controlplanev1.SetSyncStateRequest{
				State: &controlplanev1.SyncState{ConnectorInstanceId: id},
			})
			return err
		},
		"PutToken": func(ctx context.Context) error {
			_, err := cp.PutToken(ctx, &controlplanev1.PutTokenRequest{ConnectorInstanceId: id, Token: []byte("x")})
			return err
		},
		"GetToken": func(ctx context.Context) error {
			_, err := cp.GetToken(ctx, &controlplanev1.GetTokenRequest{ConnectorInstanceId: id})
			return err
		},
		"DeleteToken": func(ctx context.Context) error {
			_, err := cp.DeleteToken(ctx, &controlplanev1.DeleteTokenRequest{ConnectorInstanceId: id})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			wantCode(t, call(testCtx(t)), codes.Unauthenticated)
		})
	}
}

// TestTenantScopedRPCsStillWorkWithMetadata proves the wrapping changed
// nothing for the legitimate path: the client interceptor + a tenant context
// round-trips exactly as before.
func TestTenantScopedRPCsStillWorkWithMetadata(t *testing.T) {
	srv, store := testServer(t)
	addr := startGRPC(t, srv, store)

	conn := dialCP(t, addr, grpc.WithUnaryInterceptor(tenancygrpc.UnaryClientInterceptor()))
	cp := controlplanev1.NewControlPlaneServiceClient(conn)

	ctx, cancel := context.WithTimeout(tenantCtx(t, tenantA), 10*time.Second)
	defer cancel()

	created, err := cp.CreateConnectorInstance(ctx, &controlplanev1.CreateConnectorInstanceRequest{
		ConnectorId: "gmail",
	})
	if err != nil {
		t.Fatalf("CreateConnectorInstance with tenant metadata: %v", err)
	}
	list, err := cp.ListConnectorInstances(ctx, &controlplanev1.ListConnectorInstancesRequest{})
	if err != nil {
		t.Fatalf("ListConnectorInstances with tenant metadata: %v", err)
	}
	if len(list.GetInstances()) != 1 || list.GetInstances()[0].GetId() != created.GetInstance().GetId() {
		t.Errorf("list = %v, want exactly the created instance", list.GetInstances())
	}
}

// TestMemStoreListAllSpansTenants pins the Store.ListAll contract at the
// store level: cross-tenant rows, stable tenant-then-creation order.
func TestMemStoreListAllSpansTenants(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()

	mk := func(tenant, connector string) ConnectorInstance {
		t.Helper()
		inst, err := store.CreateConnectorInstance(ctx, ConnectorInstance{
			TenantID:    tenancy.TenantID(tenant),
			ConnectorID: connector,
			ConfigJSON:  []byte("{}"),
			Status:      "ACTIVE",
		}, 0)
		if err != nil {
			t.Fatalf("CreateConnectorInstance(%s): %v", tenant, err)
		}
		return inst
	}
	b := mk("tenant-b", "upload")
	a1 := mk("tenant-a", "gmail")
	a2 := mk("tenant-a", "upload")

	all, err := store.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListAll returned %d rows, want 3", len(all))
	}
	// tenant-a rows first (ordered by creation), then tenant-b.
	wantOrder := []string{a1.ID, a2.ID, b.ID}
	for i, want := range wantOrder {
		if all[i].ID != want {
			t.Errorf("ListAll[%d].ID = %s, want %s (got order %v)", i, all[i].ID, want, instanceIDs(all))
		}
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.ListAll(canceled); err == nil {
		t.Error("ListAll with canceled context succeeded, want error")
	}
}

// TestSchedulerServerListAllInstancesStoreError proves store failures surface
// as opaque Internal errors, never raw detail.
func TestSchedulerServerListAllInstancesStoreError(t *testing.T) {
	srv, _ := testServer(t)
	s := newSchedulerServer(failingStore{}, srv.logger)
	_, err := s.ListAllInstances(context.Background(), &controlplanev1.ListAllInstancesRequest{})
	wantCode(t, err, codes.Internal)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	memBacked := newSchedulerServer(newMemStore(), srv.logger)
	_, err = memBacked.ListAllInstances(canceled, &controlplanev1.ListAllInstancesRequest{})
	wantCode(t, err, codes.Canceled)
}

// failingStore stubs Store with a ListAll that always fails; the tenant-
// scoped methods are never reached in these tests.
type failingStore struct{ Store }

func (failingStore) ListAll(context.Context) ([]ConnectorInstance, error) {
	return nil, errSentinel("pq: secret table detail")
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

func instanceIDs(in []ConnectorInstance) []string {
	out := make([]string, len(in))
	for i, inst := range in {
		out[i] = inst.ID
	}
	return out
}
