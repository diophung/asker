package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/platform/crypto"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy"
)

const (
	tenantA = "tenant-a"
	tenantB = "tenant-b"
)

// testServer builds a server on the in-memory store with a REAL TenantCipher
// (FileKEK in a temp dir + MemDEKStore), so the token vault path is exercised
// with genuine envelope encryption.
func testServer(t *testing.T) (*server, *memStore) {
	t.Helper()
	kek, err := crypto.NewFileKEK(filepath.Join(t.TempDir(), "kek.bin"))
	if err != nil {
		t.Fatalf("NewFileKEK: %v", err)
	}
	st := newMemStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newServer(st, crypto.NewTenantCipher(kek, crypto.NewMemDEKStore()), logger), st
}

// tenantCtx returns a context carrying a tenancy.Context for the tenant, the
// way tenancygrpc.UnaryServerInterceptor installs one in production.
func tenantCtx(t *testing.T, tenant string) context.Context {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"sub": tenant})
	if err != nil {
		t.Fatalf("FromClaims(%q): %v", tenant, err)
	}
	return tenancy.WithContext(context.Background(), tc)
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("status code = %v (err: %v), want %v", got, err, want)
	}
}

// mustCreateInstance creates an ACTIVE instance for the tenant and returns it.
func mustCreateInstance(ctx context.Context, t *testing.T, s *server, connectorID string) *controlplanev1.ConnectorInstance {
	t.Helper()
	resp, err := s.CreateConnectorInstance(ctx, &controlplanev1.CreateConnectorInstanceRequest{
		ConnectorId: connectorID,
		DisplayName: connectorID + " test",
		ConfigJson:  []byte(`{"label":"x"}`),
	})
	if err != nil {
		t.Fatalf("CreateConnectorInstance: %v", err)
	}
	return resp.GetInstance()
}

func TestEnsureTenantIdempotent(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)

	first, err := s.EnsureTenant(ctx, &controlplanev1.EnsureTenantRequest{})
	if err != nil {
		t.Fatalf("EnsureTenant #1: %v", err)
	}
	if got := first.GetTenant().GetTenantId(); got != tenantA {
		t.Errorf("tenant_id = %q, want %q", got, tenantA)
	}
	if !first.GetTenant().GetCreated().IsValid() {
		t.Error("created timestamp is unset/invalid")
	}

	second, err := s.EnsureTenant(ctx, &controlplanev1.EnsureTenantRequest{})
	if err != nil {
		t.Fatalf("EnsureTenant #2: %v", err)
	}
	if !first.GetTenant().GetCreated().AsTime().Equal(second.GetTenant().GetCreated().AsTime()) {
		t.Errorf("created changed across calls: %v -> %v",
			first.GetTenant().GetCreated().AsTime(), second.GetTenant().GetCreated().AsTime())
	}
}

// TestRPCsRequireTenant proves every RPC fails closed without a tenant
// context (defense in depth below the interceptor).
func TestRPCsRequireTenant(t *testing.T) {
	s, _ := testServer(t)
	ctx := context.Background()
	id := uuid.NewString()

	calls := map[string]func() error{
		"EnsureTenant": func() error {
			_, err := s.EnsureTenant(ctx, &controlplanev1.EnsureTenantRequest{})
			return err
		},
		"CreateConnectorInstance": func() error {
			_, err := s.CreateConnectorInstance(ctx, &controlplanev1.CreateConnectorInstanceRequest{ConnectorId: "gmail"})
			return err
		},
		"ListConnectorInstances": func() error {
			_, err := s.ListConnectorInstances(ctx, &controlplanev1.ListConnectorInstancesRequest{})
			return err
		},
		"GetConnectorInstance": func() error {
			_, err := s.GetConnectorInstance(ctx, &controlplanev1.GetConnectorInstanceRequest{Id: id})
			return err
		},
		"DeleteConnectorInstance": func() error {
			_, err := s.DeleteConnectorInstance(ctx, &controlplanev1.DeleteConnectorInstanceRequest{Id: id})
			return err
		},
		"GetSyncState": func() error {
			_, err := s.GetSyncState(ctx, &controlplanev1.GetSyncStateRequest{ConnectorInstanceId: id})
			return err
		},
		"SetSyncState": func() error {
			_, err := s.SetSyncState(ctx, &controlplanev1.SetSyncStateRequest{
				State: &controlplanev1.SyncState{ConnectorInstanceId: id},
			})
			return err
		},
		"PutToken": func() error {
			_, err := s.PutToken(ctx, &controlplanev1.PutTokenRequest{ConnectorInstanceId: id, Token: []byte("x")})
			return err
		},
		"GetToken": func() error {
			_, err := s.GetToken(ctx, &controlplanev1.GetTokenRequest{ConnectorInstanceId: id})
			return err
		},
		"DeleteToken": func() error {
			_, err := s.DeleteToken(ctx, &controlplanev1.DeleteTokenRequest{ConnectorInstanceId: id})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			wantCode(t, call(), codes.Unauthenticated)
		})
	}
}

func TestCreateConnectorInstance(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)

	inst := mustCreateInstance(ctx, t, s, "gmail")
	if _, err := uuid.Parse(inst.GetId()); err != nil {
		t.Errorf("instance id %q is not a UUID: %v", inst.GetId(), err)
	}
	if inst.GetConnectorId() != "gmail" {
		t.Errorf("connector_id = %q, want gmail", inst.GetConnectorId())
	}
	if inst.GetStatus() != controlplanev1.ConnectorStatus_ACTIVE {
		t.Errorf("status = %v, want ACTIVE", inst.GetStatus())
	}
	if !inst.GetCreated().IsValid() || !inst.GetUpdated().IsValid() {
		t.Error("created/updated timestamps unset")
	}
	if got := string(inst.GetConfigJson()); got != `{"label":"x"}` {
		t.Errorf("config_json = %s", got)
	}

	// Empty config normalizes to {}.
	resp, err := s.CreateConnectorInstance(ctx, &controlplanev1.CreateConnectorInstanceRequest{ConnectorId: "upload"})
	if err != nil {
		t.Fatalf("CreateConnectorInstance (no config): %v", err)
	}
	if got := string(resp.GetInstance().GetConfigJson()); got != "{}" {
		t.Errorf("default config_json = %q, want {}", got)
	}
}

func TestCreateConnectorInstanceValidation(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)

	cases := []struct {
		name string
		req  *controlplanev1.CreateConnectorInstanceRequest
	}{
		{"missing connector_id", &controlplanev1.CreateConnectorInstanceRequest{}},
		{"connector_id too long", &controlplanev1.CreateConnectorInstanceRequest{
			ConnectorId: strings.Repeat("a", maxConnectorIDLen+1)}},
		{"display_name too long", &controlplanev1.CreateConnectorInstanceRequest{
			ConnectorId: "gmail", DisplayName: strings.Repeat("a", maxDisplayNameLen+1)}},
		{"invalid config_json", &controlplanev1.CreateConnectorInstanceRequest{
			ConnectorId: "gmail", ConfigJson: []byte(`{nope`)}},
		{"config_json too large", &controlplanev1.CreateConnectorInstanceRequest{
			ConnectorId: "gmail", ConfigJson: []byte(`"` + strings.Repeat("a", maxConfigJSONBytes) + `"`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateConnectorInstance(ctx, tc.req)
			wantCode(t, err, codes.InvalidArgument)
		})
	}
}

func TestInstanceLifecycle(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)

	first := mustCreateInstance(ctx, t, s, "gmail")
	second := mustCreateInstance(ctx, t, s, "upload")

	list, err := s.ListConnectorInstances(ctx, &controlplanev1.ListConnectorInstancesRequest{})
	if err != nil {
		t.Fatalf("ListConnectorInstances: %v", err)
	}
	if len(list.GetInstances()) != 2 {
		t.Fatalf("list returned %d instances, want 2", len(list.GetInstances()))
	}
	if list.GetInstances()[0].GetId() != first.GetId() && list.GetInstances()[1].GetId() != first.GetId() {
		t.Error("created instance missing from list")
	}

	// Lookup is canonicalizing: an uppercase UUID matches.
	got, err := s.GetConnectorInstance(ctx, &controlplanev1.GetConnectorInstanceRequest{
		Id: strings.ToUpper(first.GetId()),
	})
	if err != nil {
		t.Fatalf("GetConnectorInstance: %v", err)
	}
	if got.GetInstance().GetId() != first.GetId() {
		t.Errorf("get returned id %q, want %q", got.GetInstance().GetId(), first.GetId())
	}

	if _, err := s.DeleteConnectorInstance(ctx, &controlplanev1.DeleteConnectorInstanceRequest{Id: first.GetId()}); err != nil {
		t.Fatalf("DeleteConnectorInstance: %v", err)
	}
	_, err = s.GetConnectorInstance(ctx, &controlplanev1.GetConnectorInstanceRequest{Id: first.GetId()})
	wantCode(t, err, codes.NotFound)
	_, err = s.DeleteConnectorInstance(ctx, &controlplanev1.DeleteConnectorInstanceRequest{Id: first.GetId()})
	wantCode(t, err, codes.NotFound)

	list, err = s.ListConnectorInstances(ctx, &controlplanev1.ListConnectorInstancesRequest{})
	if err != nil {
		t.Fatalf("ListConnectorInstances after delete: %v", err)
	}
	if len(list.GetInstances()) != 1 || list.GetInstances()[0].GetId() != second.GetId() {
		t.Errorf("list after delete = %v, want only %s", list.GetInstances(), second.GetId())
	}
}

// TestDeleteCascades proves deleting an instance also removes its sync state
// and token (the in-memory twin of ON DELETE CASCADE).
func TestDeleteCascades(t *testing.T) {
	s, st := testServer(t)
	ctx := tenantCtx(t, tenantA)
	inst := mustCreateInstance(ctx, t, s, "gmail")

	if _, err := s.SetSyncState(ctx, &controlplanev1.SetSyncStateRequest{State: &controlplanev1.SyncState{
		ConnectorInstanceId: inst.GetId(), Phase: controlplanev1.SyncPhase_FULL_SYNC,
	}}); err != nil {
		t.Fatalf("SetSyncState: %v", err)
	}
	if _, err := s.PutToken(ctx, &controlplanev1.PutTokenRequest{
		ConnectorInstanceId: inst.GetId(), Token: []byte("secret"),
	}); err != nil {
		t.Fatalf("PutToken: %v", err)
	}

	if _, err := s.DeleteConnectorInstance(ctx, &controlplanev1.DeleteConnectorInstanceRequest{Id: inst.GetId()}); err != nil {
		t.Fatalf("DeleteConnectorInstance: %v", err)
	}

	st.mu.Lock()
	_, syncLeft := st.syncs[inst.GetId()]
	_, tokenLeft := st.tokens[inst.GetId()]
	st.mu.Unlock()
	if syncLeft || tokenLeft {
		t.Errorf("cascade failed: sync row left=%v, token row left=%v", syncLeft, tokenLeft)
	}
}

func TestInstanceIDValidation(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)

	for _, bad := range []string{"", "not-a-uuid", "../../etc/passwd"} {
		calls := map[string]func() error{
			"GetConnectorInstance": func() error {
				_, err := s.GetConnectorInstance(ctx, &controlplanev1.GetConnectorInstanceRequest{Id: bad})
				return err
			},
			"DeleteConnectorInstance": func() error {
				_, err := s.DeleteConnectorInstance(ctx, &controlplanev1.DeleteConnectorInstanceRequest{Id: bad})
				return err
			},
			"GetSyncState": func() error {
				_, err := s.GetSyncState(ctx, &controlplanev1.GetSyncStateRequest{ConnectorInstanceId: bad})
				return err
			},
			"SetSyncState": func() error {
				_, err := s.SetSyncState(ctx, &controlplanev1.SetSyncStateRequest{
					State: &controlplanev1.SyncState{ConnectorInstanceId: bad}})
				return err
			},
			"PutToken": func() error {
				_, err := s.PutToken(ctx, &controlplanev1.PutTokenRequest{ConnectorInstanceId: bad, Token: []byte("x")})
				return err
			},
			"GetToken": func() error {
				_, err := s.GetToken(ctx, &controlplanev1.GetTokenRequest{ConnectorInstanceId: bad})
				return err
			},
			"DeleteToken": func() error {
				_, err := s.DeleteToken(ctx, &controlplanev1.DeleteTokenRequest{ConnectorInstanceId: bad})
				return err
			},
		}
		for name, call := range calls {
			t.Run(name+"/"+bad, func(t *testing.T) {
				wantCode(t, call(), codes.InvalidArgument)
			})
		}
	}
}

// TestTenantIsolation is the local slice of the sacred cross-tenant suite:
// tenant B attacks every RPC with tenant A's instance ID and must get exactly
// what a nonexistent ID yields — NotFound, same message, no oracle.
func TestTenantIsolation(t *testing.T) {
	s, _ := testServer(t)
	ctxA := tenantCtx(t, tenantA)
	ctxB := tenantCtx(t, tenantB)

	inst := mustCreateInstance(ctxA, t, s, "gmail")
	if _, err := s.PutToken(ctxA, &controlplanev1.PutTokenRequest{
		ConnectorInstanceId: inst.GetId(), Token: []byte("a-secret"),
	}); err != nil {
		t.Fatalf("PutToken as A: %v", err)
	}
	if _, err := s.SetSyncState(ctxA, &controlplanev1.SetSyncStateRequest{State: &controlplanev1.SyncState{
		ConnectorInstanceId: inst.GetId(), Phase: controlplanev1.SyncPhase_INCREMENTAL, Cursor: "c1",
	}}); err != nil {
		t.Fatalf("SetSyncState as A: %v", err)
	}

	attacks := map[string]func(ctx context.Context, id string) error{
		"GetConnectorInstance": func(ctx context.Context, id string) error {
			_, err := s.GetConnectorInstance(ctx, &controlplanev1.GetConnectorInstanceRequest{Id: id})
			return err
		},
		"DeleteConnectorInstance": func(ctx context.Context, id string) error {
			_, err := s.DeleteConnectorInstance(ctx, &controlplanev1.DeleteConnectorInstanceRequest{Id: id})
			return err
		},
		"GetSyncState": func(ctx context.Context, id string) error {
			_, err := s.GetSyncState(ctx, &controlplanev1.GetSyncStateRequest{ConnectorInstanceId: id})
			return err
		},
		"SetSyncState": func(ctx context.Context, id string) error {
			_, err := s.SetSyncState(ctx, &controlplanev1.SetSyncStateRequest{
				State: &controlplanev1.SyncState{ConnectorInstanceId: id, Cursor: "stolen"}})
			return err
		},
		"PutToken": func(ctx context.Context, id string) error {
			_, err := s.PutToken(ctx, &controlplanev1.PutTokenRequest{ConnectorInstanceId: id, Token: []byte("evil")})
			return err
		},
		"GetToken": func(ctx context.Context, id string) error {
			_, err := s.GetToken(ctx, &controlplanev1.GetTokenRequest{ConnectorInstanceId: id})
			return err
		},
		"DeleteToken": func(ctx context.Context, id string) error {
			_, err := s.DeleteToken(ctx, &controlplanev1.DeleteTokenRequest{ConnectorInstanceId: id})
			return err
		},
	}
	ghostID := uuid.NewString()
	for name, attack := range attacks {
		t.Run(name, func(t *testing.T) {
			crossErr := attack(ctxB, inst.GetId())
			wantCode(t, crossErr, codes.NotFound)
			ghostErr := attack(ctxB, ghostID)
			wantCode(t, ghostErr, codes.NotFound)
			// No existence oracle: identical status for foreign vs absent.
			if status.Convert(crossErr).Message() != status.Convert(ghostErr).Message() {
				t.Errorf("cross-tenant error %q differs from absent-row error %q",
					status.Convert(crossErr).Message(), status.Convert(ghostErr).Message())
			}
		})
	}

	// B sees an empty world.
	list, err := s.ListConnectorInstances(ctxB, &controlplanev1.ListConnectorInstancesRequest{})
	if err != nil {
		t.Fatalf("ListConnectorInstances as B: %v", err)
	}
	if len(list.GetInstances()) != 0 {
		t.Errorf("tenant B sees %d instances, want 0", len(list.GetInstances()))
	}

	// A's data survived all of B's attempts.
	got, err := s.GetConnectorInstance(ctxA, &controlplanev1.GetConnectorInstanceRequest{Id: inst.GetId()})
	if err != nil {
		t.Fatalf("A's instance gone after B's attacks: %v", err)
	}
	if got.GetInstance().GetId() != inst.GetId() {
		t.Errorf("instance id changed: %q", got.GetInstance().GetId())
	}
	tok, err := s.GetToken(ctxA, &controlplanev1.GetTokenRequest{ConnectorInstanceId: inst.GetId()})
	if err != nil {
		t.Fatalf("A's token gone after B's attacks: %v", err)
	}
	if !bytes.Equal(tok.GetToken(), []byte("a-secret")) {
		t.Errorf("A's token corrupted: %q", tok.GetToken())
	}
	sync, err := s.GetSyncState(ctxA, &controlplanev1.GetSyncStateRequest{ConnectorInstanceId: inst.GetId()})
	if err != nil {
		t.Fatalf("A's sync state gone: %v", err)
	}
	if sync.GetState().GetCursor() != "c1" {
		t.Errorf("A's cursor = %q, want c1", sync.GetState().GetCursor())
	}
}

func TestGetSyncStateDefaultsPending(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)
	inst := mustCreateInstance(ctx, t, s, "gmail")

	resp, err := s.GetSyncState(ctx, &controlplanev1.GetSyncStateRequest{ConnectorInstanceId: inst.GetId()})
	if err != nil {
		t.Fatalf("GetSyncState: %v", err)
	}
	st := resp.GetState()
	if st.GetPhase() != controlplanev1.SyncPhase_PENDING {
		t.Errorf("phase = %v, want PENDING", st.GetPhase())
	}
	if st.GetCursor() != "" || st.GetDocsEmitted() != 0 || st.GetLastError() != "" {
		t.Errorf("default state not empty: %v", st)
	}
	if st.GetLastSyncStarted() != nil || st.GetLastSyncCompleted() != nil {
		t.Errorf("default state has sync timestamps: %v", st)
	}
	if st.GetConnectorInstanceId() != inst.GetId() {
		t.Errorf("connector_instance_id = %q, want %q", st.GetConnectorInstanceId(), inst.GetId())
	}
}

func TestSetSyncStateUpsert(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)
	inst := mustCreateInstance(ctx, t, s, "gmail")

	started := timestamppb.Now()
	first, err := s.SetSyncState(ctx, &controlplanev1.SetSyncStateRequest{State: &controlplanev1.SyncState{
		ConnectorInstanceId: inst.GetId(),
		Phase:               controlplanev1.SyncPhase_FULL_SYNC,
		Cursor:              "page-1",
		LastSyncStarted:     started,
		DocsEmitted:         10,
	}})
	if err != nil {
		t.Fatalf("SetSyncState #1: %v", err)
	}
	if first.GetState().GetPhase() != controlplanev1.SyncPhase_FULL_SYNC || first.GetState().GetCursor() != "page-1" {
		t.Errorf("first set returned %v", first.GetState())
	}

	completed := timestamppb.Now()
	second, err := s.SetSyncState(ctx, &controlplanev1.SetSyncStateRequest{State: &controlplanev1.SyncState{
		ConnectorInstanceId: inst.GetId(),
		Phase:               controlplanev1.SyncPhase_INCREMENTAL,
		Cursor:              "page-9",
		LastSyncStarted:     started,
		LastSyncCompleted:   completed,
		LastError:           "",
		DocsEmitted:         25,
	}})
	if err != nil {
		t.Fatalf("SetSyncState #2 (upsert): %v", err)
	}
	if second.GetState().GetCursor() != "page-9" || second.GetState().GetDocsEmitted() != 25 {
		t.Errorf("upsert returned %v", second.GetState())
	}

	got, err := s.GetSyncState(ctx, &controlplanev1.GetSyncStateRequest{ConnectorInstanceId: inst.GetId()})
	if err != nil {
		t.Fatalf("GetSyncState: %v", err)
	}
	st := got.GetState()
	if st.GetPhase() != controlplanev1.SyncPhase_INCREMENTAL {
		t.Errorf("phase = %v, want INCREMENTAL", st.GetPhase())
	}
	if st.GetCursor() != "page-9" || st.GetDocsEmitted() != 25 {
		t.Errorf("state = %v, want cursor page-9 / docs 25", st)
	}
	if !st.GetLastSyncCompleted().AsTime().Equal(completed.AsTime()) {
		t.Errorf("last_sync_completed = %v, want %v", st.GetLastSyncCompleted(), completed)
	}
}

func TestSetSyncStateValidation(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)
	inst := mustCreateInstance(ctx, t, s, "gmail")

	// nil state.
	_, err := s.SetSyncState(ctx, &controlplanev1.SetSyncStateRequest{})
	wantCode(t, err, codes.InvalidArgument)

	// Negative docs_emitted.
	_, err = s.SetSyncState(ctx, &controlplanev1.SetSyncStateRequest{State: &controlplanev1.SyncState{
		ConnectorInstanceId: inst.GetId(), DocsEmitted: -1}})
	wantCode(t, err, codes.InvalidArgument)

	// Oversized cursor.
	_, err = s.SetSyncState(ctx, &controlplanev1.SetSyncStateRequest{State: &controlplanev1.SyncState{
		ConnectorInstanceId: inst.GetId(), Cursor: strings.Repeat("c", maxCursorBytes+1)}})
	wantCode(t, err, codes.InvalidArgument)

	// Unknown phase value.
	_, err = s.SetSyncState(ctx, &controlplanev1.SetSyncStateRequest{State: &controlplanev1.SyncState{
		ConnectorInstanceId: inst.GetId(), Phase: controlplanev1.SyncPhase(99)}})
	wantCode(t, err, codes.InvalidArgument)

	// Invalid timestamp.
	_, err = s.SetSyncState(ctx, &controlplanev1.SetSyncStateRequest{State: &controlplanev1.SyncState{
		ConnectorInstanceId: inst.GetId(),
		LastSyncStarted:     &timestamppb.Timestamp{Seconds: 1, Nanos: -1}}})
	wantCode(t, err, codes.InvalidArgument)

	// UNSPECIFIED phase (bare checkpoint write) normalizes to PENDING.
	resp, err := s.SetSyncState(ctx, &controlplanev1.SetSyncStateRequest{State: &controlplanev1.SyncState{
		ConnectorInstanceId: inst.GetId(), Cursor: "checkpoint"}})
	if err != nil {
		t.Fatalf("SetSyncState with UNSPECIFIED phase: %v", err)
	}
	if resp.GetState().GetPhase() != controlplanev1.SyncPhase_PENDING {
		t.Errorf("UNSPECIFIED phase stored as %v, want PENDING", resp.GetState().GetPhase())
	}
}

func TestTokenRoundTrip(t *testing.T) {
	s, st := testServer(t)
	ctx := tenantCtx(t, tenantA)
	inst := mustCreateInstance(ctx, t, s, "gmail")
	secret := []byte(`{"access_token":"ya29.secret","refresh_token":"1//refresh"}`)

	// Absent before put.
	_, err := s.GetToken(ctx, &controlplanev1.GetTokenRequest{ConnectorInstanceId: inst.GetId()})
	wantCode(t, err, codes.NotFound)

	if _, err := s.PutToken(ctx, &controlplanev1.PutTokenRequest{
		ConnectorInstanceId: inst.GetId(), Token: secret,
	}); err != nil {
		t.Fatalf("PutToken: %v", err)
	}

	// Encrypted at rest: the stored bytes are not the plaintext and do not
	// contain it.
	st.mu.Lock()
	stored := bytes.Clone(st.tokens[inst.GetId()].ciphertext)
	st.mu.Unlock()
	if bytes.Equal(stored, secret) || bytes.Contains(stored, []byte("ya29.secret")) {
		t.Fatal("token stored in plaintext")
	}

	got, err := s.GetToken(ctx, &controlplanev1.GetTokenRequest{ConnectorInstanceId: inst.GetId()})
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if !bytes.Equal(got.GetToken(), secret) {
		t.Errorf("round-trip mismatch: got %q", got.GetToken())
	}

	// Overwrite (token refresh path) round-trips the new value.
	rotated := []byte("rotated")
	if _, err := s.PutToken(ctx, &controlplanev1.PutTokenRequest{
		ConnectorInstanceId: inst.GetId(), Token: rotated,
	}); err != nil {
		t.Fatalf("PutToken (rotate): %v", err)
	}
	got, err = s.GetToken(ctx, &controlplanev1.GetTokenRequest{ConnectorInstanceId: inst.GetId()})
	if err != nil {
		t.Fatalf("GetToken after rotate: %v", err)
	}
	if !bytes.Equal(got.GetToken(), rotated) {
		t.Errorf("rotate mismatch: got %q", got.GetToken())
	}

	if _, err := s.DeleteToken(ctx, &controlplanev1.DeleteTokenRequest{ConnectorInstanceId: inst.GetId()}); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	_, err = s.GetToken(ctx, &controlplanev1.GetTokenRequest{ConnectorInstanceId: inst.GetId()})
	wantCode(t, err, codes.NotFound)
	_, err = s.DeleteToken(ctx, &controlplanev1.DeleteTokenRequest{ConnectorInstanceId: inst.GetId()})
	wantCode(t, err, codes.NotFound)
}

func TestPutTokenValidation(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)
	inst := mustCreateInstance(ctx, t, s, "gmail")

	_, err := s.PutToken(ctx, &controlplanev1.PutTokenRequest{ConnectorInstanceId: inst.GetId()})
	wantCode(t, err, codes.InvalidArgument)

	_, err = s.PutToken(ctx, &controlplanev1.PutTokenRequest{
		ConnectorInstanceId: inst.GetId(), Token: bytes.Repeat([]byte("x"), maxTokenBytes+1)})
	wantCode(t, err, codes.InvalidArgument)

	// Token for a nonexistent instance.
	_, err = s.PutToken(ctx, &controlplanev1.PutTokenRequest{
		ConnectorInstanceId: uuid.NewString(), Token: []byte("x")})
	wantCode(t, err, codes.NotFound)
}

// TestTokenCiphertextIsTenantBound smuggles tenant A's ciphertext into a row
// owned by tenant B (simulating a storage-layer compromise) and proves the
// envelope encryption still refuses to decrypt it for B: the DEK is
// per-tenant, so isolation holds even if row scoping were bypassed.
func TestTokenCiphertextIsTenantBound(t *testing.T) {
	s, st := testServer(t)
	ctxA := tenantCtx(t, tenantA)
	ctxB := tenantCtx(t, tenantB)

	instA := mustCreateInstance(ctxA, t, s, "gmail")
	instB := mustCreateInstance(ctxB, t, s, "gmail")

	if _, err := s.PutToken(ctxA, &controlplanev1.PutTokenRequest{
		ConnectorInstanceId: instA.GetId(), Token: []byte("a-secret"),
	}); err != nil {
		t.Fatalf("PutToken as A: %v", err)
	}

	st.mu.Lock()
	stolen := bytes.Clone(st.tokens[instA.GetId()].ciphertext)
	st.tokens[instB.GetId()] = memToken{tenantID: tenantB, ciphertext: stolen}
	st.mu.Unlock()

	_, err := s.GetToken(ctxB, &controlplanev1.GetTokenRequest{ConnectorInstanceId: instB.GetId()})
	wantCode(t, err, codes.Internal) // fails closed, never returns A's secret
}
