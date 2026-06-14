package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy"
)

// Request validation bounds. Generous for real payloads, tight enough that a
// misbehaving internal caller cannot bloat Postgres rows.
const (
	maxConnectorIDLen  = 128
	maxDisplayNameLen  = 256
	maxConfigJSONBytes = 256 << 10 // 256 KiB
	maxTokenBytes      = 64 << 10  // 64 KiB (OAuth token blobs are ~KB)
	maxCursorBytes     = 64 << 10
	maxLastErrorLen    = 4096
)

// tokenCipher is the slice of *crypto.TenantCipher (platform/crypto pinned
// surface) the server needs: per-tenant envelope encryption for the token
// vault plus Forget for the GDPR crypto-shred. Narrowed to an interface so unit
// tests could substitute it, and so server.go does not couple to the concrete
// type.
type tokenCipher interface {
	Encrypt(ctx context.Context, tc tenancy.Context, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, tc tenancy.Context, ciphertext []byte) ([]byte, error)
	// Forget drops the tenant's cached unwrapped DEK so a crypto-shred is
	// complete (no cached AEAD keeps serving post-delete).
	Forget(tenantID tenancy.TenantID)
}

// dekDeleter is the crypto-shred seam: DEKStore.Delete. The default control-
// plane store is the Postgres tenant_deks table; tests substitute a fake.
type dekDeleter interface {
	Delete(ctx context.Context, tenantID tenancy.TenantID) error
}

// serverConfig carries the quota/abuse caps and the (optional) external-store
// purgers for the GDPR delete cascade. Zero/nil fields disable the
// corresponding control: maxConnectorInstances <= 0 means unlimited, and a nil
// purger skips that store (the cascade still purges Postgres + crypto-shreds
// the DEK, the minimum that renders the tenant's data unreadable).
type serverConfig struct {
	maxConnectorInstances int
	dek                   dekDeleter
	vespa                 vespaPurger
	blobs                 blobPurger
	cache                 cachePurger
}

// server implements controlplanev1.ControlPlaneServiceServer. The tenant is
// NEVER a request field: tenancygrpc.UnaryServerInterceptor validates the
// x-asker-tenant metadata and installs a tenancy.Context; every method
// re-extracts it (fail closed) and scopes every store call to it.
type server struct {
	controlplanev1.UnimplementedControlPlaneServiceServer

	store  Store
	cipher tokenCipher
	cfg    serverConfig
	logger *slog.Logger
}

var _ controlplanev1.ControlPlaneServiceServer = (*server)(nil)

func newServer(store Store, cipher tokenCipher, logger *slog.Logger) *server {
	return &server{store: store, cipher: cipher, logger: logger}
}

// newServerWithConfig is the production constructor: it wires the quota caps
// and the GDPR cascade purgers in addition to the store + cipher.
func newServerWithConfig(store Store, cipher tokenCipher, cfg serverConfig, logger *slog.Logger) *server {
	return &server{store: store, cipher: cipher, cfg: cfg, logger: logger}
}

func (s *server) EnsureTenant(ctx context.Context, _ *controlplanev1.EnsureTenantRequest) (*controlplanev1.EnsureTenantResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.store.EnsureTenant(ctx, tc.TenantID())
	if err != nil {
		return nil, s.rpcErr(ctx, "EnsureTenant", tc, err)
	}
	return &controlplanev1.EnsureTenantResponse{
		Tenant: &controlplanev1.Tenant{
			TenantId: string(t.ID),
			Created:  timestamppb.New(t.CreatedAt),
		},
	}, nil
}

func (s *server) CreateConnectorInstance(ctx context.Context, req *controlplanev1.CreateConnectorInstanceRequest) (*controlplanev1.CreateConnectorInstanceResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case req.GetConnectorId() == "":
		return nil, status.Error(codes.InvalidArgument, "connector_id is required")
	case len(req.GetConnectorId()) > maxConnectorIDLen:
		return nil, status.Errorf(codes.InvalidArgument, "connector_id exceeds %d bytes", maxConnectorIDLen)
	case len(req.GetDisplayName()) > maxDisplayNameLen:
		return nil, status.Errorf(codes.InvalidArgument, "display_name exceeds %d bytes", maxDisplayNameLen)
	case len(req.GetConfigJson()) > maxConfigJSONBytes:
		return nil, status.Errorf(codes.InvalidArgument, "config_json exceeds %d bytes", maxConfigJSONBytes)
	}
	configJSON := req.GetConfigJson()
	if len(configJSON) == 0 {
		configJSON = []byte("{}")
	}
	if !json.Valid(configJSON) {
		return nil, status.Error(codes.InvalidArgument, "config_json is not valid JSON")
	}

	// Per-tenant connector-instance quota (M6 abuse control): cap how many
	// instances one tenant can create, so a single tenant cannot spawn unbounded
	// scheduler goroutines in the hub. The cap is enforced ATOMICALLY inside the
	// store create (a gated INSERT .. SELECT under a per-tenant lock), NOT a
	// check-then-insert here, so concurrent creates cannot race past it (finding
	// M6-#8). <= 0 disables the cap. ErrQuotaExceeded -> RESOURCE_EXHAUSTED.
	inst, err := s.store.CreateConnectorInstance(ctx, ConnectorInstance{
		TenantID:    tc.TenantID(),
		ConnectorID: req.GetConnectorId(),
		DisplayName: req.GetDisplayName(),
		ConfigJSON:  configJSON,
		Status:      controlplanev1.ConnectorStatus_ACTIVE.String(),
	}, s.cfg.maxConnectorInstances)
	if errors.Is(err, ErrQuotaExceeded) {
		return nil, status.Errorf(codes.ResourceExhausted,
			"connector instance quota reached (%d); delete an existing connector first", s.cfg.maxConnectorInstances)
	}
	if err != nil {
		return nil, s.rpcErr(ctx, "CreateConnectorInstance", tc, err)
	}
	return &controlplanev1.CreateConnectorInstanceResponse{Instance: instanceToProto(inst)}, nil
}

func (s *server) ListConnectorInstances(ctx context.Context, _ *controlplanev1.ListConnectorInstancesRequest) (*controlplanev1.ListConnectorInstancesResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	instances, err := s.store.ListConnectorInstances(ctx, tc.TenantID())
	if err != nil {
		return nil, s.rpcErr(ctx, "ListConnectorInstances", tc, err)
	}
	out := make([]*controlplanev1.ConnectorInstance, 0, len(instances))
	for _, inst := range instances {
		out = append(out, instanceToProto(inst))
	}
	return &controlplanev1.ListConnectorInstancesResponse{Instances: out}, nil
}

func (s *server) GetConnectorInstance(ctx context.Context, req *controlplanev1.GetConnectorInstanceRequest) (*controlplanev1.GetConnectorInstanceResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseInstanceID(req.GetId(), "id")
	if err != nil {
		return nil, err
	}
	inst, err := s.store.GetConnectorInstance(ctx, tc.TenantID(), id)
	if err != nil {
		return nil, s.rpcErr(ctx, "GetConnectorInstance", tc, err)
	}
	return &controlplanev1.GetConnectorInstanceResponse{Instance: instanceToProto(inst)}, nil
}

func (s *server) DeleteConnectorInstance(ctx context.Context, req *controlplanev1.DeleteConnectorInstanceRequest) (*controlplanev1.DeleteConnectorInstanceResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseInstanceID(req.GetId(), "id")
	if err != nil {
		return nil, err
	}
	if err := s.store.DeleteConnectorInstance(ctx, tc.TenantID(), id); err != nil {
		return nil, s.rpcErr(ctx, "DeleteConnectorInstance", tc, err)
	}
	return &controlplanev1.DeleteConnectorInstanceResponse{}, nil
}

func (s *server) GetSyncState(ctx context.Context, req *controlplanev1.GetSyncStateRequest) (*controlplanev1.GetSyncStateResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseInstanceID(req.GetConnectorInstanceId(), "connector_instance_id")
	if err != nil {
		return nil, err
	}
	st, err := s.store.GetSyncState(ctx, tc.TenantID(), id)
	if err != nil {
		return nil, s.rpcErr(ctx, "GetSyncState", tc, err)
	}
	return &controlplanev1.GetSyncStateResponse{State: syncStateToProto(st)}, nil
}

func (s *server) SetSyncState(ctx context.Context, req *controlplanev1.SetSyncStateRequest) (*controlplanev1.SetSyncStateResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	in := req.GetState()
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "state is required")
	}
	id, err := parseInstanceID(in.GetConnectorInstanceId(), "state.connector_instance_id")
	if err != nil {
		return nil, err
	}
	if len(in.GetCursor()) > maxCursorBytes {
		return nil, status.Errorf(codes.InvalidArgument, "state.cursor exceeds %d bytes", maxCursorBytes)
	}
	phase, err := normalizePhase(in.GetPhase())
	if err != nil {
		return nil, err
	}
	started, err := timeFromProto(in.GetLastSyncStarted(), "state.last_sync_started")
	if err != nil {
		return nil, err
	}
	completed, err := timeFromProto(in.GetLastSyncCompleted(), "state.last_sync_completed")
	if err != nil {
		return nil, err
	}
	if in.GetDocsEmitted() < 0 {
		return nil, status.Error(codes.InvalidArgument, "state.docs_emitted must be >= 0")
	}
	lastError := in.GetLastError()
	if len(lastError) > maxLastErrorLen {
		lastError = lastError[:maxLastErrorLen]
	}

	st, err := s.store.SetSyncState(ctx, tc.TenantID(), SyncState{
		ConnectorInstanceID: id,
		TenantID:            tc.TenantID(),
		Cursor:              in.GetCursor(),
		Phase:               phase,
		LastSyncStarted:     started,
		LastSyncCompleted:   completed,
		LastError:           lastError,
		DocsEmitted:         in.GetDocsEmitted(),
	})
	if err != nil {
		return nil, s.rpcErr(ctx, "SetSyncState", tc, err)
	}
	return &controlplanev1.SetSyncStateResponse{State: syncStateToProto(st)}, nil
}

func (s *server) PutToken(ctx context.Context, req *controlplanev1.PutTokenRequest) (*controlplanev1.PutTokenResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseInstanceID(req.GetConnectorInstanceId(), "connector_instance_id")
	if err != nil {
		return nil, err
	}
	switch {
	case len(req.GetToken()) == 0:
		return nil, status.Error(codes.InvalidArgument, "token is required")
	case len(req.GetToken()) > maxTokenBytes:
		return nil, status.Errorf(codes.InvalidArgument, "token exceeds %d bytes", maxTokenBytes)
	}
	ciphertext, err := s.cipher.Encrypt(ctx, tc, req.GetToken())
	if err != nil {
		s.logger.ErrorContext(ctx, "token encryption failed",
			"rpc", "PutToken", "tenant", tc.TenantID(), "error", err)
		return nil, status.Error(codes.Internal, "PutToken failed")
	}
	if err := s.store.PutToken(ctx, tc.TenantID(), id, ciphertext); err != nil {
		return nil, s.rpcErr(ctx, "PutToken", tc, err)
	}
	return &controlplanev1.PutTokenResponse{}, nil
}

func (s *server) GetToken(ctx context.Context, req *controlplanev1.GetTokenRequest) (*controlplanev1.GetTokenResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseInstanceID(req.GetConnectorInstanceId(), "connector_instance_id")
	if err != nil {
		return nil, err
	}
	ciphertext, err := s.store.GetToken(ctx, tc.TenantID(), id)
	if err != nil {
		return nil, s.rpcErr(ctx, "GetToken", tc, err)
	}
	token, err := s.cipher.Decrypt(ctx, tc, ciphertext)
	if err != nil {
		// Wrong-tenant decryption cannot get here (the store lookup is
		// tenant-scoped); this is corruption or a key problem. Fail closed.
		s.logger.ErrorContext(ctx, "token decryption failed",
			"rpc", "GetToken", "tenant", tc.TenantID(), "error", err)
		return nil, status.Error(codes.Internal, "GetToken failed")
	}
	return &controlplanev1.GetTokenResponse{Token: token}, nil
}

func (s *server) DeleteToken(ctx context.Context, req *controlplanev1.DeleteTokenRequest) (*controlplanev1.DeleteTokenResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseInstanceID(req.GetConnectorInstanceId(), "connector_instance_id")
	if err != nil {
		return nil, err
	}
	if err := s.store.DeleteToken(ctx, tc.TenantID(), id); err != nil {
		return nil, s.rpcErr(ctx, "DeleteToken", tc, err)
	}
	return &controlplanev1.DeleteTokenResponse{}, nil
}

// callerTenant extracts the tenancy.Context installed by the server
// interceptor. Defense in depth: the interceptor already rejects tenant-less
// calls, but every method re-checks and fails closed rather than assuming
// middleware ordering.
func callerTenant(ctx context.Context) (tenancy.Context, error) {
	tc, err := tenancy.FromContext(ctx)
	if err != nil {
		return tenancy.Context{}, status.Error(codes.Unauthenticated, "no tenant in request context")
	}
	return tc, nil
}

// rpcErr maps store errors to gRPC statuses. ErrNotFound passes through as
// codes.NotFound (cross-tenant and absent look identical by construction);
// everything else is logged in full and returned as an opaque Internal so
// SQL/internal detail never crosses the trust boundary.
func (s *server) rpcErr(ctx context.Context, rpc string, tc tenancy.Context, err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, "not found")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	}
	s.logger.ErrorContext(ctx, "store operation failed",
		"rpc", rpc, "tenant", tc.TenantID(), "error", err)
	return status.Errorf(codes.Internal, "%s failed", rpc)
}

// parseInstanceID validates and canonicalizes a connector instance UUID. A
// malformed ID is InvalidArgument (it could never exist, so this reveals
// nothing about any tenant's data).
func parseInstanceID(id, field string) (string, error) {
	if id == "" {
		return "", status.Errorf(codes.InvalidArgument, "%s is required", field)
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "%s must be a UUID", field)
	}
	return u.String(), nil
}

// normalizePhase maps the request SyncPhase onto the stored enum name.
// UNSPECIFIED (the proto3 zero value) is treated as PENDING so bare
// checkpoint writes are valid; unknown values are rejected.
func normalizePhase(p controlplanev1.SyncPhase) (string, error) {
	if p == controlplanev1.SyncPhase_SYNC_PHASE_UNSPECIFIED {
		p = controlplanev1.SyncPhase_PENDING
	}
	name, ok := controlplanev1.SyncPhase_name[int32(p)]
	if !ok {
		return "", status.Errorf(codes.InvalidArgument, "state.phase %d is not a known SyncPhase", p)
	}
	return name, nil
}

func instanceToProto(inst ConnectorInstance) *controlplanev1.ConnectorInstance {
	return &controlplanev1.ConnectorInstance{
		Id:          inst.ID,
		ConnectorId: inst.ConnectorID,
		DisplayName: inst.DisplayName,
		ConfigJson:  inst.ConfigJSON,
		Status:      statusFromText(inst.Status),
		Created:     timestamppb.New(inst.CreatedAt),
		Updated:     timestamppb.New(inst.UpdatedAt),
	}
}

func syncStateToProto(st SyncState) *controlplanev1.SyncState {
	out := &controlplanev1.SyncState{
		ConnectorInstanceId: st.ConnectorInstanceID,
		Cursor:              st.Cursor,
		Phase:               phaseFromText(st.Phase),
		LastError:           st.LastError,
		DocsEmitted:         st.DocsEmitted,
	}
	if st.LastSyncStarted != nil {
		out.LastSyncStarted = timestamppb.New(*st.LastSyncStarted)
	}
	if st.LastSyncCompleted != nil {
		out.LastSyncCompleted = timestamppb.New(*st.LastSyncCompleted)
	}
	return out
}

// statusFromText maps a stored status name back to the enum; unknown text
// (schema drift) reads as UNSPECIFIED rather than failing the RPC.
func statusFromText(s string) controlplanev1.ConnectorStatus {
	if v, ok := controlplanev1.ConnectorStatus_value[s]; ok {
		return controlplanev1.ConnectorStatus(v)
	}
	return controlplanev1.ConnectorStatus_CONNECTOR_STATUS_UNSPECIFIED
}

// phaseFromText maps a stored phase name back to the enum; unknown text reads
// as UNSPECIFIED.
func phaseFromText(s string) controlplanev1.SyncPhase {
	if v, ok := controlplanev1.SyncPhase_value[s]; ok {
		return controlplanev1.SyncPhase(v)
	}
	return controlplanev1.SyncPhase_SYNC_PHASE_UNSPECIFIED
}

// timeFromProto converts an optional proto timestamp to a nullable time.
func timeFromProto(ts *timestamppb.Timestamp, field string) (*time.Time, error) {
	if ts == nil {
		return nil, nil
	}
	if err := ts.CheckValid(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%s is not a valid timestamp", field)
	}
	t := ts.AsTime()
	return &t, nil
}
