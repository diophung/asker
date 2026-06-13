package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/asker/asker/platform/crypto"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy"
)

// TestMemStoreCreateAtomicCap proves the per-tenant connector-instance cap is
// enforced atomically in the store create (finding M6-#8): concurrent creates
// must never exceed the cap, and creates for a different tenant are unaffected.
func TestMemStoreCreateAtomicCap(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	const capLimit = 5
	const goroutines = 50

	var created, rejected, other int64
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.CreateConnectorInstance(ctx, ConnectorInstance{
				TenantID:    tenantA,
				ConnectorID: "gmail",
				ConfigJSON:  []byte("{}"),
				Status:      "ACTIVE",
			}, capLimit)
			switch {
			case err == nil:
				atomic.AddInt64(&created, 1)
			case errors.Is(err, ErrQuotaExceeded):
				atomic.AddInt64(&rejected, 1)
			default:
				t.Errorf("unexpected create error: %v", err)
			}
		}()
	}
	wg.Wait()

	if created != capLimit {
		t.Fatalf("created = %d, want exactly %d (cap not enforced atomically)", created, capLimit)
	}
	if rejected != goroutines-capLimit {
		t.Fatalf("rejected = %d, want %d", rejected, goroutines-capLimit)
	}
	if n, _ := st.CountConnectorInstances(ctx, tenantA); n != capLimit {
		t.Fatalf("stored instances = %d, want %d", n, capLimit)
	}

	// A different tenant is unaffected by tenantA's cap.
	for i := 0; i < capLimit; i++ {
		if _, err := st.CreateConnectorInstance(ctx, ConnectorInstance{
			TenantID: tenantB, ConnectorID: "gmail", ConfigJSON: []byte("{}"), Status: "ACTIVE",
		}, capLimit); err != nil {
			t.Fatalf("tenantB create %d under its own cap: %v", i, err)
		}
		other++
	}
	if other != capLimit {
		t.Fatalf("tenantB created = %d, want %d", other, capLimit)
	}
}

// TestMemStoreCreateCapDisabled confirms maxInstances <= 0 disables the cap.
func TestMemStoreCreateCapDisabled(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if _, err := st.CreateConnectorInstance(ctx, ConnectorInstance{
			TenantID: tenantA, ConnectorID: "gmail", ConfigJSON: []byte("{}"), Status: "ACTIVE",
		}, 0); err != nil {
			t.Fatalf("create %d with cap disabled: %v", i, err)
		}
	}
	if n, _ := st.CountConnectorInstances(ctx, tenantA); n != 100 {
		t.Fatalf("instances = %d, want 100 (cap should be disabled)", n)
	}
}

// failingCachePurger fails its PurgeTenant to exercise the best-effort Redis
// path (finding M6-#7).
type failingCachePurger struct{}

func (failingCachePurger) PurgeTenant(context.Context, tenancy.TenantID) error {
	return errors.New("redis unreachable")
}

// cascadeServer builds a server whose cascade wiring lets a test toggle the
// cache purger. It mirrors deleteTestServer but with a caller-supplied cache.
func cascadeServer(t *testing.T, cache cachePurger) (*server, *memStore) {
	t.Helper()
	kek, err := crypto.NewFileKEK(filepath.Join(t.TempDir(), "kek.bin"))
	if err != nil {
		t.Fatalf("NewFileKEK: %v", err)
	}
	dekStore := crypto.NewMemDEKStore()
	st := newMemStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newServerWithConfig(st, crypto.NewTenantCipher(kek, dekStore), serverConfig{
		maxConnectorInstances: 2,
		dek:                   dekStore,
		vespa:                 &fakeVespaPurger{},
		blobs:                 &fakeBlobPurger{},
		cache:                 cache,
	}, logger)
	return srv, st
}

// TestCascadeRedisFailureNotVerifiedEmpty proves a failed Redis purge keeps the
// erasure non-fatal but does NOT claim complete erasure (finding M6-#7):
// RedisPurged is false and VerifiedEmpty is false, while the authoritative
// stores are still purged.
func TestCascadeRedisFailureNotVerifiedEmpty(t *testing.T) {
	srv, _ := cascadeServer(t, failingCachePurger{})
	aCtx := tenantCtx(t, tenantA)
	mustCreateInstance(aCtx, t, srv, "gmail")

	resp, err := srv.DeleteTenant(aCtx, &controlplanev1.DeleteTenantRequest{Confirm: tenantA})
	if err != nil {
		t.Fatalf("DeleteTenant (Redis failure must be non-fatal): %v", err)
	}
	rep := resp.GetReport()
	if rep.GetRedisPurged() {
		t.Error("RedisPurged = true after a Redis purge failure")
	}
	if rep.GetVerifiedEmpty() {
		t.Error("VerifiedEmpty = true despite a failed best-effort Redis purge; complete erasure must not be claimed")
	}
	// Authoritative stores were still purged + crypto-shredded.
	if !rep.GetDekDestroyed() || !rep.GetVespaGroupPurged() {
		t.Errorf("authoritative stores not purged: %+v", rep)
	}
}

// TestCascadeRedisSuccessVerifiedEmpty is the control: with the cache purge
// succeeding, the cascade reports complete erasure.
func TestCascadeRedisSuccessVerifiedEmpty(t *testing.T) {
	srv, _ := cascadeServer(t, &fakeCachePurger{})
	aCtx := tenantCtx(t, tenantA)
	mustCreateInstance(aCtx, t, srv, "gmail")

	resp, err := srv.DeleteTenant(aCtx, &controlplanev1.DeleteTenantRequest{Confirm: tenantA})
	if err != nil {
		t.Fatalf("DeleteTenant: %v", err)
	}
	rep := resp.GetReport()
	if !rep.GetRedisPurged() || !rep.GetVerifiedEmpty() {
		t.Errorf("clean cascade should be verified empty: %+v", rep)
	}
}

// suspendRecordingStore wraps memStore to record that SetTenantConnectorStatus
// (the re-ingest fence, finding M6-#5) is called BEFORE the Postgres purge.
type suspendRecordingStore struct {
	*memStore
	mu       sync.Mutex
	order    []string
	suspends []tenancy.TenantID
}

func (s *suspendRecordingStore) SetTenantConnectorStatus(ctx context.Context, t tenancy.TenantID, st string) (int64, error) {
	s.mu.Lock()
	s.order = append(s.order, "suspend")
	s.suspends = append(s.suspends, t)
	s.mu.Unlock()
	return s.memStore.SetTenantConnectorStatus(ctx, t, st)
}

func (s *suspendRecordingStore) PurgeTenant(ctx context.Context, t tenancy.TenantID) (PurgeCounts, error) {
	s.mu.Lock()
	s.order = append(s.order, "purge")
	s.mu.Unlock()
	return s.memStore.PurgeTenant(ctx, t)
}

// TestCascadeSuspendsConnectorsBeforePurge proves the re-ingest fence (finding
// M6-#5): the cascade suspends the tenant's connectors BEFORE purging Postgres,
// so the scheduler stops producing docs before the stores are wiped.
func TestCascadeSuspendsConnectorsBeforePurge(t *testing.T) {
	kek, err := crypto.NewFileKEK(filepath.Join(t.TempDir(), "kek.bin"))
	if err != nil {
		t.Fatalf("NewFileKEK: %v", err)
	}
	dekStore := crypto.NewMemDEKStore()
	rec := &suspendRecordingStore{memStore: newMemStore()}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newServerWithConfig(rec, crypto.NewTenantCipher(kek, dekStore), serverConfig{
		maxConnectorInstances: 0,
		dek:                   dekStore,
		vespa:                 &fakeVespaPurger{},
		blobs:                 &fakeBlobPurger{},
		cache:                 &fakeCachePurger{},
	}, logger)
	aCtx := tenantCtx(t, tenantA)
	mustCreateInstance(aCtx, t, srv, "gmail")

	if _, err := srv.DeleteTenant(aCtx, &controlplanev1.DeleteTenantRequest{Confirm: tenantA}); err != nil {
		t.Fatalf("DeleteTenant: %v", err)
	}
	if len(rec.order) < 2 || rec.order[0] != "suspend" {
		t.Fatalf("cascade order = %v, want suspend BEFORE purge", rec.order)
	}
	// Suspend index must precede the (first) purge index.
	suspendAt, purgeAt := -1, -1
	for i, step := range rec.order {
		if step == "suspend" && suspendAt < 0 {
			suspendAt = i
		}
		if step == "purge" && purgeAt < 0 {
			purgeAt = i
		}
	}
	if suspendAt < 0 || purgeAt < 0 || suspendAt >= purgeAt {
		t.Fatalf("suspend (%d) must come before purge (%d): %v", suspendAt, purgeAt, rec.order)
	}
	if len(rec.suspends) == 0 || rec.suspends[0] != tenantA {
		t.Fatalf("suspended tenants = %v, want tenantA first", rec.suspends)
	}
}

// TestAdminActorFromMetadata proves the operator identity flows from gRPC
// metadata into the audit actor (finding M6-#6): the AdminDeleteTenant report
// records "admin:<subject>" from x-asker-admin-subject, falling back to
// "admin:unknown" when absent.
func TestAdminActorFromMetadata(t *testing.T) {
	t.Run("subject present", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs(adminSubjectMetadataKey, "operator-123"))
		if got := adminActor(ctx); got != "admin:operator-123" {
			t.Fatalf("adminActor = %q, want admin:operator-123", got)
		}
	})
	t.Run("subject absent", func(t *testing.T) {
		if got := adminActor(context.Background()); got != "admin:unknown" {
			t.Fatalf("adminActor = %q, want admin:unknown", got)
		}
	})
	t.Run("blank subject", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs(adminSubjectMetadataKey, "   "))
		if got := adminActor(ctx); got != "admin:unknown" {
			t.Fatalf("adminActor = %q, want admin:unknown", got)
		}
	})
}

// TestAdminDeleteRecordsActor proves AdminDeleteTenant records the forwarded
// operator subject in the DeleteReport.Actor field end-to-end.
func TestAdminDeleteRecordsActor(t *testing.T) {
	srv, admin, _, _, _, _, _ := deleteTestServer(t)
	aCtx := tenantCtx(t, tenantA)
	mustCreateInstance(aCtx, t, srv, "gmail")

	adminCtx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(adminSubjectMetadataKey, "op@example.com"))
	resp, err := admin.AdminDeleteTenant(adminCtx, &controlplanev1.AdminDeleteTenantRequest{TenantId: tenantA})
	if err != nil {
		t.Fatalf("AdminDeleteTenant: %v", err)
	}
	if got := resp.GetReport().GetActor(); got != "admin:op@example.com" {
		t.Fatalf("report actor = %q, want admin:op@example.com", got)
	}
}

// TestIsProdEnvNormalization proves the prod KEK guard normalizes ASKER_ENV so a
// casing/whitespace typo cannot silently disable fail-closed (finding M6-#9).
func TestIsProdEnvNormalization(t *testing.T) {
	prod := []string{"production", "Production", "PROD", "prod ", " prod", "  Staging  ", "staging"}
	for _, v := range prod {
		if !isProdEnv(v) {
			t.Errorf("isProdEnv(%q) = false, want true (must be treated as prod)", v)
		}
	}
	notProd := []string{"", "dev", "development", "local", "test"}
	for _, v := range notProd {
		if isProdEnv(v) {
			t.Errorf("isProdEnv(%q) = true, want false", v)
		}
	}
}

// TestProdEnvFailsClosedWithoutVault ties the normalization to the actual
// fail-closed behavior: "Production"/"prod " with no VAULT_ADDR must select a
// production deployment and so refuse the dev file KEK.
func TestProdEnvFailsClosedWithoutVault(t *testing.T) {
	for _, env := range []string{"Production", "prod ", "staging"} {
		cfg := controlPlaneConfig{Env: env, KEKFile: filepath.Join(t.TempDir(), "kek.bin")}
		if !cfg.isProd() {
			t.Fatalf("cfg.isProd() for ASKER_ENV=%q = false, want true", env)
		}
		_, err := crypto.SelectKEK(crypto.KEKSelection{KEKFile: cfg.KEKFile, IsProd: cfg.isProd()})
		if !errors.Is(err, crypto.ErrProdRequiresVault) {
			t.Fatalf("SelectKEK for ASKER_ENV=%q: err = %v, want ErrProdRequiresVault", env, err)
		}
	}
}
