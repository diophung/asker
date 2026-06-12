package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asker/asker/platform/crypto"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy"
)

// pgTestEnv names the opt-in switch for the Postgres integration test. CI
// unit jobs leave it unset (the SQL paths are covered by the wave-3 compose
// e2e); locally, point it at the dev stack:
//
//	CONTROL_PLANE_TEST_DATABASE_URL='postgres://asker:asker@127.0.0.1:15432/asker?sslmode=disable' \
//	  go test ./services/control-plane/...
//
// The test creates (and drops) its own scratch database on that server; it
// never writes to the database named in the URL.
const pgTestEnv = "CONTROL_PLANE_TEST_DATABASE_URL"

// newScratchDB provisions a throwaway database on the server named by
// pgTestEnv (skipping the test when unset) and returns its URL. The database
// is dropped on cleanup.
func newScratchDB(t *testing.T) string {
	t.Helper()
	baseURL := os.Getenv(pgTestEnv)
	if baseURL == "" {
		t.Skipf("%s not set; skipping Postgres integration test", pgTestEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dbName := fmt.Sprintf("asker_cp_test_%d", time.Now().UnixNano())

	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect %s: %v", pgTestEnv, err)
	}
	// CREATE DATABASE cannot be parameterized; dbName is generated above and
	// contains only [a-z0-9_].
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("create scratch database: %v", err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		admin, err := pgx.Connect(ctx, baseURL)
		if err != nil {
			t.Errorf("cleanup connect: %v", err)
			return
		}
		defer func() { _ = admin.Close(ctx) }()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+dbName+" WITH (FORCE)"); err != nil {
			t.Errorf("drop scratch database %s: %v", dbName, err)
		}
	})

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", pgTestEnv, err)
	}
	u.Path = "/" + dbName
	return u.String()
}

// newPGTestStore provisions a scratch database, applies the embedded
// migrations to it, and returns a connected pool.
func newPGTestStore(t *testing.T) *pgxpool.Pool {
	t.Helper()
	scratchURL := newScratchDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := runMigrations(ctx, scratchURL, logger); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	// Idempotency: a second run must be a clean no-op.
	if err := runMigrations(ctx, scratchURL, logger); err != nil {
		t.Fatalf("migrations re-run: %v", err)
	}

	pool, err := pgxpool.New(ctx, scratchURL)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPGStoreIntegration(t *testing.T) {
	pool := newPGTestStore(t)
	store := newPGStore(pool)
	ctx := context.Background()
	tA := tenancy.TenantID(tenantA)
	tB := tenancy.TenantID(tenantB)

	t.Run("EnsureTenantIdempotent", func(t *testing.T) {
		first, err := store.EnsureTenant(ctx, tA)
		if err != nil {
			t.Fatalf("EnsureTenant #1: %v", err)
		}
		second, err := store.EnsureTenant(ctx, tA)
		if err != nil {
			t.Fatalf("EnsureTenant #2: %v", err)
		}
		if !first.CreatedAt.Equal(second.CreatedAt) {
			t.Errorf("created_at changed: %v -> %v", first.CreatedAt, second.CreatedAt)
		}
	})

	var instID string
	t.Run("CreateGetList", func(t *testing.T) {
		inst, err := store.CreateConnectorInstance(ctx, ConnectorInstance{
			TenantID:    tA,
			ConnectorID: "gmail",
			DisplayName: "work mail",
			ConfigJSON:  []byte(`{"label":"INBOX"}`),
			Status:      "ACTIVE",
		})
		if err != nil {
			t.Fatalf("CreateConnectorInstance: %v", err)
		}
		instID = inst.ID
		if inst.ID == "" || inst.CreatedAt.IsZero() || inst.UpdatedAt.IsZero() {
			t.Fatalf("create did not assign id/timestamps: %+v", inst)
		}

		got, err := store.GetConnectorInstance(ctx, tA, instID)
		if err != nil {
			t.Fatalf("GetConnectorInstance: %v", err)
		}
		if got.ConnectorID != "gmail" || got.DisplayName != "work mail" || got.Status != "ACTIVE" {
			t.Errorf("got %+v", got)
		}
		if !bytes.Equal(got.ConfigJSON, []byte(`{"label": "INBOX"}`)) &&
			!bytes.Equal(got.ConfigJSON, []byte(`{"label":"INBOX"}`)) {
			t.Errorf("config_json round trip = %s", got.ConfigJSON)
		}

		list, err := store.ListConnectorInstances(ctx, tA)
		if err != nil {
			t.Fatalf("ListConnectorInstances: %v", err)
		}
		if len(list) != 1 || list[0].ID != instID {
			t.Errorf("list = %+v, want 1 instance %s", list, instID)
		}
	})

	t.Run("CreateImplicitlyEnsuresTenant", func(t *testing.T) {
		inst, err := store.CreateConnectorInstance(ctx, ConnectorInstance{
			TenantID:    "tenant-implicit",
			ConnectorID: "upload",
			ConfigJSON:  []byte("{}"),
			Status:      "ACTIVE",
		})
		if err != nil {
			t.Fatalf("create for unregistered tenant: %v", err)
		}
		if _, err := store.GetConnectorInstance(ctx, "tenant-implicit", inst.ID); err != nil {
			t.Errorf("instance unreadable after implicit tenant create: %v", err)
		}
	})

	t.Run("SyncStateLifecycle", func(t *testing.T) {
		// LEFT JOIN default before any checkpoint.
		st, err := store.GetSyncState(ctx, tA, instID)
		if err != nil {
			t.Fatalf("GetSyncState (default): %v", err)
		}
		if st.Phase != "PENDING" || st.Cursor != "" || st.DocsEmitted != 0 ||
			st.LastSyncStarted != nil || st.LastSyncCompleted != nil {
			t.Errorf("default sync state = %+v, want synthesized PENDING", st)
		}

		started := time.Now().UTC().Truncate(time.Microsecond) // timestamptz is µs
		set, err := store.SetSyncState(ctx, tA, SyncState{
			ConnectorInstanceID: instID,
			TenantID:            tA,
			Cursor:              "hist-1000",
			Phase:               "FULL_SYNC",
			LastSyncStarted:     &started,
			DocsEmitted:         42,
		})
		if err != nil {
			t.Fatalf("SetSyncState (insert): %v", err)
		}
		if set.Cursor != "hist-1000" || set.Phase != "FULL_SYNC" || set.DocsEmitted != 42 {
			t.Errorf("insert returned %+v", set)
		}
		if set.LastSyncStarted == nil || !set.LastSyncStarted.Equal(started) {
			t.Errorf("last_sync_started = %v, want %v", set.LastSyncStarted, started)
		}

		completed := time.Now().UTC().Truncate(time.Microsecond)
		set, err = store.SetSyncState(ctx, tA, SyncState{
			ConnectorInstanceID: instID,
			TenantID:            tA,
			Cursor:              "hist-2000",
			Phase:               "INCREMENTAL",
			LastSyncStarted:     &started,
			LastSyncCompleted:   &completed,
			LastError:           "",
			DocsEmitted:         99,
		})
		if err != nil {
			t.Fatalf("SetSyncState (upsert): %v", err)
		}
		if set.Cursor != "hist-2000" || set.Phase != "INCREMENTAL" || set.DocsEmitted != 99 {
			t.Errorf("upsert returned %+v", set)
		}

		got, err := store.GetSyncState(ctx, tA, instID)
		if err != nil {
			t.Fatalf("GetSyncState: %v", err)
		}
		if got.Cursor != "hist-2000" || got.Phase != "INCREMENTAL" || got.DocsEmitted != 99 {
			t.Errorf("read back %+v", got)
		}
		if got.LastSyncCompleted == nil || !got.LastSyncCompleted.Equal(completed) {
			t.Errorf("last_sync_completed = %v, want %v", got.LastSyncCompleted, completed)
		}
	})

	t.Run("TokenLifecycle", func(t *testing.T) {
		ciphertext := []byte("opaque-ciphertext-v1")
		if _, err := store.GetToken(ctx, tA, instID); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetToken before put: %v, want ErrNotFound", err)
		}
		if err := store.PutToken(ctx, tA, instID, ciphertext); err != nil {
			t.Fatalf("PutToken: %v", err)
		}
		got, err := store.GetToken(ctx, tA, instID)
		if err != nil {
			t.Fatalf("GetToken: %v", err)
		}
		if !bytes.Equal(got, ciphertext) {
			t.Errorf("token = %q, want %q", got, ciphertext)
		}
		// Upsert path.
		if err := store.PutToken(ctx, tA, instID, []byte("rotated")); err != nil {
			t.Fatalf("PutToken (rotate): %v", err)
		}
		got, err = store.GetToken(ctx, tA, instID)
		if err != nil {
			t.Fatalf("GetToken after rotate: %v", err)
		}
		if !bytes.Equal(got, []byte("rotated")) {
			t.Errorf("token after rotate = %q", got)
		}
		if err := store.DeleteToken(ctx, tA, instID); err != nil {
			t.Fatalf("DeleteToken: %v", err)
		}
		if err := store.DeleteToken(ctx, tA, instID); !errors.Is(err, ErrNotFound) {
			t.Errorf("second DeleteToken: %v, want ErrNotFound", err)
		}
	})

	t.Run("TenantIsolation", func(t *testing.T) {
		if _, err := store.EnsureTenant(ctx, tB); err != nil {
			t.Fatalf("EnsureTenant B: %v", err)
		}
		if err := store.PutToken(ctx, tA, instID, []byte("a-ciphertext")); err != nil {
			t.Fatalf("PutToken as A: %v", err)
		}

		if _, err := store.GetConnectorInstance(ctx, tB, instID); !errors.Is(err, ErrNotFound) {
			t.Errorf("B GetConnectorInstance(A's id): %v, want ErrNotFound", err)
		}
		if err := store.DeleteConnectorInstance(ctx, tB, instID); !errors.Is(err, ErrNotFound) {
			t.Errorf("B DeleteConnectorInstance(A's id): %v, want ErrNotFound", err)
		}
		if _, err := store.GetSyncState(ctx, tB, instID); !errors.Is(err, ErrNotFound) {
			t.Errorf("B GetSyncState(A's id): %v, want ErrNotFound", err)
		}
		if _, err := store.SetSyncState(ctx, tB, SyncState{
			ConnectorInstanceID: instID, TenantID: tB, Phase: "FAILED", Cursor: "stolen",
		}); !errors.Is(err, ErrNotFound) {
			t.Errorf("B SetSyncState(A's id): %v, want ErrNotFound", err)
		}
		if err := store.PutToken(ctx, tB, instID, []byte("evil")); !errors.Is(err, ErrNotFound) {
			t.Errorf("B PutToken(A's id): %v, want ErrNotFound", err)
		}
		if _, err := store.GetToken(ctx, tB, instID); !errors.Is(err, ErrNotFound) {
			t.Errorf("B GetToken(A's id): %v, want ErrNotFound", err)
		}
		if err := store.DeleteToken(ctx, tB, instID); !errors.Is(err, ErrNotFound) {
			t.Errorf("B DeleteToken(A's id): %v, want ErrNotFound", err)
		}
		if list, err := store.ListConnectorInstances(ctx, tB); err != nil || len(list) != 0 {
			t.Errorf("B list = %v, %v; want empty", list, err)
		}

		// A's rows survived and are unchanged.
		st, err := store.GetSyncState(ctx, tA, instID)
		if err != nil || st.Cursor != "hist-2000" {
			t.Errorf("A's sync state after attacks = %+v, %v", st, err)
		}
		tok, err := store.GetToken(ctx, tA, instID)
		if err != nil || !bytes.Equal(tok, []byte("a-ciphertext")) {
			t.Errorf("A's token after attacks = %q, %v", tok, err)
		}
	})

	t.Run("DeleteCascades", func(t *testing.T) {
		if err := store.DeleteConnectorInstance(ctx, tA, instID); err != nil {
			t.Fatalf("DeleteConnectorInstance: %v", err)
		}
		var syncRows, tokenRows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM sync_states WHERE connector_instance_id = $1`, instID).Scan(&syncRows); err != nil {
			t.Fatalf("count sync_states: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM tokens WHERE connector_instance_id = $1`, instID).Scan(&tokenRows); err != nil {
			t.Fatalf("count tokens: %v", err)
		}
		if syncRows != 0 || tokenRows != 0 {
			t.Errorf("cascade failed: %d sync rows, %d token rows left", syncRows, tokenRows)
		}
		if _, err := store.GetConnectorInstance(ctx, tA, instID); !errors.Is(err, ErrNotFound) {
			t.Errorf("instance still readable after delete: %v", err)
		}
	})
}

func TestPGDEKStoreIntegration(t *testing.T) {
	pool := newPGTestStore(t)
	deks := newPGDEKStore(pool)
	ctx := context.Background()
	tA := tenancy.TenantID(tenantA)

	if _, err := deks.GetWrappedDEK(ctx, tA); !errors.Is(err, crypto.ErrDEKNotFound) {
		t.Fatalf("GetWrappedDEK before put: %v, want ErrDEKNotFound", err)
	}
	if err := deks.PutWrappedDEK(ctx, tA, []byte("wrapped-1")); err != nil {
		t.Fatalf("PutWrappedDEK: %v", err)
	}
	got, err := deks.GetWrappedDEK(ctx, tA)
	if err != nil || !bytes.Equal(got, []byte("wrapped-1")) {
		t.Fatalf("GetWrappedDEK = %q, %v", got, err)
	}
	// First-writer-wins: a second put never replaces the stored DEK.
	if err := deks.PutWrappedDEK(ctx, tA, []byte("wrapped-2")); err != nil {
		t.Fatalf("PutWrappedDEK #2: %v", err)
	}
	got, err = deks.GetWrappedDEK(ctx, tA)
	if err != nil || !bytes.Equal(got, []byte("wrapped-1")) {
		t.Fatalf("DEK was replaced: %q, %v (first writer must win)", got, err)
	}
	if err := deks.PutWrappedDEK(ctx, tA, nil); err == nil {
		t.Error("PutWrappedDEK accepted an empty DEK")
	}
	if _, err := deks.GetWrappedDEK(ctx, ""); err == nil {
		t.Error("GetWrappedDEK accepted an empty tenant")
	}
	if err := deks.PutWrappedDEK(ctx, "", []byte("x")); err == nil {
		t.Error("PutWrappedDEK accepted an empty tenant")
	}
}

// TestPGTokenVaultEndToEnd runs the full production token path — gRPC server
// on pgStore, TenantCipher backed by FileKEK + pgDEKStore — and proves the
// plaintext round-trips while only ciphertext and a wrapped DEK hit Postgres.
func TestPGTokenVaultEndToEnd(t *testing.T) {
	pool := newPGTestStore(t)
	kek, err := crypto.NewFileKEK(filepath.Join(t.TempDir(), "kek.bin"))
	if err != nil {
		t.Fatalf("NewFileKEK: %v", err)
	}
	cipher := crypto.NewTenantCipher(kek, newPGDEKStore(pool))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newServer(newPGStore(pool), cipher, logger)

	ctx := tenantCtx(t, tenantA)
	inst := mustCreateInstance(ctx, t, srv, "gmail")
	secret := []byte(`{"refresh_token":"1//very-secret"}`)

	if _, err := srv.PutToken(ctx, &controlplanev1.PutTokenRequest{
		ConnectorInstanceId: inst.GetId(), Token: secret,
	}); err != nil {
		t.Fatalf("PutToken: %v", err)
	}

	var stored []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT ciphertext FROM tokens WHERE connector_instance_id = $1`, inst.GetId()).Scan(&stored); err != nil {
		t.Fatalf("read raw ciphertext: %v", err)
	}
	if bytes.Contains(stored, []byte("very-secret")) {
		t.Fatal("plaintext token reached Postgres")
	}
	var dekRows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tenant_deks WHERE tenant_id = $1`, tenantA).Scan(&dekRows); err != nil {
		t.Fatalf("count tenant_deks: %v", err)
	}
	if dekRows != 1 {
		t.Errorf("tenant_deks rows = %d, want 1 (lazy provisioning)", dekRows)
	}

	got, err := srv.GetToken(ctx, &controlplanev1.GetTokenRequest{ConnectorInstanceId: inst.GetId()})
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if !bytes.Equal(got.GetToken(), secret) {
		t.Errorf("round trip = %q, want %q", got.GetToken(), secret)
	}
}
