package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/asker/asker/platform/crypto"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy"
)

// fakeVespaPurger / fakeBlobPurger / fakeCachePurger record which tenants were
// purged so a test can assert the cascade fanned out, and assert isolation
// (a sibling tenant is never touched).
type fakeVespaPurger struct {
	mu      sync.Mutex
	purged  []tenancy.TenantID
	failFor tenancy.TenantID
}

func (f *fakeVespaPurger) PurgeGroup(_ context.Context, t tenancy.TenantID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t == f.failFor {
		return errors.New("vespa down")
	}
	f.purged = append(f.purged, t)
	return nil
}

type fakeBlobPurger struct {
	mu     sync.Mutex
	purged []tenancy.TenantID
}

func (f *fakeBlobPurger) DeletePrefix(_ context.Context, tc tenancy.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purged = append(f.purged, tc.TenantID())
	return 3, nil
}

type fakeCachePurger struct {
	mu     sync.Mutex
	purged []tenancy.TenantID
}

func (f *fakeCachePurger) PurgeTenant(_ context.Context, t tenancy.TenantID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purged = append(f.purged, t)
	return nil
}

// deleteTestServer builds a server with the full cascade wiring over the
// in-memory store and a REAL cipher (so the DEK shred is genuine). It returns
// the server, store, the cipher's DEK store, and the three fakes.
func deleteTestServer(t *testing.T) (*server, *adminServer, *memStore, crypto.DEKStore, *fakeVespaPurger, *fakeBlobPurger, *fakeCachePurger) {
	t.Helper()
	kek, err := crypto.NewFileKEK(filepath.Join(t.TempDir(), "kek.bin"))
	if err != nil {
		t.Fatalf("NewFileKEK: %v", err)
	}
	dekStore := crypto.NewMemDEKStore()
	cipher := crypto.NewTenantCipher(kek, dekStore)
	st := newMemStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	vp := &fakeVespaPurger{}
	bp := &fakeBlobPurger{}
	cp := &fakeCachePurger{}
	srv := newServerWithConfig(st, cipher, serverConfig{
		maxConnectorInstances: 2,
		dek:                   dekStore,
		vespa:                 vp,
		blobs:                 bp,
		cache:                 cp,
	}, logger)
	admin := newAdminServer(st, srv, logger)
	return srv, admin, st, dekStore, vp, bp, cp
}

func TestDeleteTenantCascadeAndIsolation(t *testing.T) {
	srv, _, st, dekStore, vp, bp, cp := deleteTestServer(t)
	aCtx := tenantCtx(t, tenantA)
	bCtx := tenantCtx(t, tenantB)

	// Seed both tenants: an instance + a token (which provisions a DEK).
	instA := mustCreateInstance(aCtx, t, srv, "gmail")
	if _, err := srv.PutToken(aCtx, &controlplanev1.PutTokenRequest{
		ConnectorInstanceId: instA.GetId(), Token: []byte("secret-A"),
	}); err != nil {
		t.Fatalf("PutToken A: %v", err)
	}
	instB := mustCreateInstance(bCtx, t, srv, "gmail")
	if _, err := srv.PutToken(bCtx, &controlplanev1.PutTokenRequest{
		ConnectorInstanceId: instB.GetId(), Token: []byte("secret-B"),
	}); err != nil {
		t.Fatalf("PutToken B: %v", err)
	}

	// A's DEK exists before the shred.
	if _, err := dekStore.GetWrappedDEK(context.Background(), tenantA); err != nil {
		t.Fatalf("A DEK missing before delete: %v", err)
	}

	// Erase A.
	resp, err := srv.DeleteTenant(aCtx, &controlplanev1.DeleteTenantRequest{Confirm: tenantA})
	if err != nil {
		t.Fatalf("DeleteTenant A: %v", err)
	}
	report := resp.GetReport()
	if !report.GetDekDestroyed() || !report.GetVespaGroupPurged() || !report.GetRedisPurged() || !report.GetVerifiedEmpty() {
		t.Errorf("report missing a cascade step: %+v", report)
	}
	if report.GetConnectorInstancesDeleted() != 1 || report.GetTokensDeleted() != 1 || report.GetBlobsDeleted() != 3 {
		t.Errorf("unexpected counts: %+v", report)
	}

	// A's DEK is crypto-shredded.
	if _, err := dekStore.GetWrappedDEK(context.Background(), tenantA); !errors.Is(err, crypto.ErrDEKNotFound) {
		t.Errorf("A DEK after delete: err = %v, want ErrDEKNotFound", err)
	}
	// A's Postgres rows are gone.
	empty, err := st.TenantResidue(context.Background(), tenantA)
	if err != nil || !empty {
		t.Errorf("A residue after delete: empty=%v err=%v, want empty", empty, err)
	}
	// A's token is unreadable now (instance gone -> NotFound).
	if _, err := srv.GetToken(aCtx, &controlplanev1.GetTokenRequest{ConnectorInstanceId: instA.GetId()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetToken A after delete: code = %v, want NotFound", status.Code(err))
	}

	// B is COMPLETELY untouched (isolation).
	if _, err := dekStore.GetWrappedDEK(context.Background(), tenantB); err != nil {
		t.Errorf("B DEK destroyed by A's delete: %v", err)
	}
	if got, err := srv.GetToken(bCtx, &controlplanev1.GetTokenRequest{ConnectorInstanceId: instB.GetId()}); err != nil {
		t.Errorf("B token lost after A delete: %v", err)
	} else if string(got.GetToken()) != "secret-B" {
		t.Errorf("B token corrupted: %q", got.GetToken())
	}
	for _, p := range []struct {
		name   string
		purged []tenancy.TenantID
	}{
		{"vespa", vp.purged}, {"blob", bp.purged}, {"cache", cp.purged},
	} {
		if len(p.purged) != 1 || p.purged[0] != tenantA {
			t.Errorf("%s purger touched %v, want exactly [tenant-a]", p.name, p.purged)
		}
	}
}

func TestDeleteTenantRequiresConfirmMatch(t *testing.T) {
	srv, _, _, _, _, _, _ := deleteTestServer(t)
	aCtx := tenantCtx(t, tenantA)
	// Wrong confirm token (another tenant's id) is rejected without deleting.
	if _, err := srv.DeleteTenant(aCtx, &controlplanev1.DeleteTenantRequest{Confirm: tenantB}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("DeleteTenant wrong confirm: code = %v, want InvalidArgument", status.Code(err))
	}
	// Empty confirm is also rejected.
	if _, err := srv.DeleteTenant(aCtx, &controlplanev1.DeleteTenantRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("DeleteTenant empty confirm: code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestDeleteTenantRequiresTenant(t *testing.T) {
	srv, _, _, _, _, _, _ := deleteTestServer(t)
	if _, err := srv.DeleteTenant(context.Background(), &controlplanev1.DeleteTenantRequest{Confirm: tenantA}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("DeleteTenant without tenant ctx: code = %v, want Unauthenticated", status.Code(err))
	}
}

func TestDeleteTenantIsIdempotent(t *testing.T) {
	srv, _, _, _, _, _, _ := deleteTestServer(t)
	aCtx := tenantCtx(t, tenantA)
	mustCreateInstance(aCtx, t, srv, "gmail")
	if _, err := srv.DeleteTenant(aCtx, &controlplanev1.DeleteTenantRequest{Confirm: tenantA}); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// Second delete of an already-empty tenant still succeeds (converges).
	if _, err := srv.DeleteTenant(aCtx, &controlplanev1.DeleteTenantRequest{Confirm: tenantA}); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestConnectorInstanceQuotaCap(t *testing.T) {
	srv, _, _, _, _, _, _ := deleteTestServer(t) // cap is 2
	aCtx := tenantCtx(t, tenantA)
	mustCreateInstance(aCtx, t, srv, "gmail")
	mustCreateInstance(aCtx, t, srv, "gmail")
	// Third exceeds the cap.
	_, err := srv.CreateConnectorInstance(aCtx, &controlplanev1.CreateConnectorInstanceRequest{
		ConnectorId: "gmail", ConfigJson: []byte("{}"),
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("over-quota create: code = %v, want ResourceExhausted", status.Code(err))
	}
	// A different tenant is unaffected by A's quota.
	bCtx := tenantCtx(t, tenantB)
	if _, err := srv.CreateConnectorInstance(bCtx, &controlplanev1.CreateConnectorInstanceRequest{
		ConnectorId: "gmail", ConfigJson: []byte("{}"),
	}); err != nil {
		t.Errorf("B create under its own quota: %v", err)
	}
}

func TestAdminListAndUsageAndSuspend(t *testing.T) {
	srv, admin, _, _, _, _, _ := deleteTestServer(t)
	aCtx := tenantCtx(t, tenantA)
	bCtx := tenantCtx(t, tenantB)
	mustCreateInstance(aCtx, t, srv, "gmail")
	mustCreateInstance(bCtx, t, srv, "gmail")

	ctx := context.Background()
	list, err := admin.ListTenants(ctx, &controlplanev1.ListTenantsRequest{})
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	if len(list.GetTenants()) != 2 {
		t.Fatalf("ListTenants returned %d tenants, want 2", len(list.GetTenants()))
	}

	// GetTenantUsage for a known tenant.
	usage, err := admin.GetTenantUsage(ctx, &controlplanev1.GetTenantUsageRequest{TenantId: tenantA})
	if err != nil {
		t.Fatalf("GetTenantUsage: %v", err)
	}
	if usage.GetUsage().GetConnectorInstances() != 1 {
		t.Errorf("usage instances = %d, want 1", usage.GetUsage().GetConnectorInstances())
	}
	if usage.GetUsage().GetSuspended() {
		t.Error("tenant A unexpectedly suspended")
	}

	// Suspend A: all its instances PAUSE; usage reflects it; B is unaffected.
	sresp, err := admin.SuspendTenant(ctx, &controlplanev1.SuspendTenantRequest{TenantId: tenantA, Suspended: true})
	if err != nil {
		t.Fatalf("SuspendTenant: %v", err)
	}
	if sresp.GetInstancesChanged() != 1 {
		t.Errorf("instances changed = %d, want 1", sresp.GetInstancesChanged())
	}
	usage, _ = admin.GetTenantUsage(ctx, &controlplanev1.GetTenantUsageRequest{TenantId: tenantA})
	if !usage.GetUsage().GetSuspended() {
		t.Error("tenant A not suspended after SuspendTenant")
	}
	usageB, _ := admin.GetTenantUsage(ctx, &controlplanev1.GetTenantUsageRequest{TenantId: tenantB})
	if usageB.GetUsage().GetSuspended() {
		t.Error("tenant B wrongly suspended by A's suspend")
	}

	// Unknown tenant -> NotFound (no oracle distinction beyond admin scope).
	if _, err := admin.GetTenantUsage(ctx, &controlplanev1.GetTenantUsageRequest{TenantId: "tenant-zzz"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTenantUsage unknown: code = %v, want NotFound", status.Code(err))
	}
	// Invalid tenant id (syntax) -> InvalidArgument.
	if _, err := admin.GetTenantUsage(ctx, &controlplanev1.GetTenantUsageRequest{TenantId: "../etc"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetTenantUsage bad id: code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestAdminDeleteTenantErasesArbitraryTenant(t *testing.T) {
	srv, admin, st, dekStore, _, _, _ := deleteTestServer(t)
	aCtx := tenantCtx(t, tenantA)
	instA := mustCreateInstance(aCtx, t, srv, "gmail")
	if _, err := srv.PutToken(aCtx, &controlplanev1.PutTokenRequest{
		ConnectorInstanceId: instA.GetId(), Token: []byte("secret-A"),
	}); err != nil {
		t.Fatalf("PutToken A: %v", err)
	}

	// Admin deletes A's tenant by request id (no caller-tenant scope).
	resp, err := admin.AdminDeleteTenant(context.Background(), &controlplanev1.AdminDeleteTenantRequest{TenantId: tenantA})
	if err != nil {
		t.Fatalf("AdminDeleteTenant: %v", err)
	}
	if !resp.GetReport().GetVerifiedEmpty() || !resp.GetReport().GetDekDestroyed() {
		t.Errorf("admin delete report incomplete: %+v", resp.GetReport())
	}
	if _, err := dekStore.GetWrappedDEK(context.Background(), tenantA); !errors.Is(err, crypto.ErrDEKNotFound) {
		t.Errorf("A DEK after admin delete: err = %v, want ErrDEKNotFound", err)
	}
	empty, _ := st.TenantResidue(context.Background(), tenantA)
	if !empty {
		t.Error("A residue remains after admin delete")
	}

	// Bad target id is rejected.
	if _, err := admin.AdminDeleteTenant(context.Background(), &controlplanev1.AdminDeleteTenantRequest{TenantId: ""}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("AdminDeleteTenant empty id: code = %v, want InvalidArgument", status.Code(err))
	}
}
