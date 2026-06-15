package main

// Shared M1 test infrastructure: in-proc gRPC fakes for QueryService and
// ControlPlaneService (both registered behind the REAL tenancygrpc server
// interceptor, so the tenant chokepoint is exercised end to end), a fake
// rate counter, and a handler builder wiring them together.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	"github.com/asker/asker/platform/tenancy"
	"github.com/asker/asker/platform/tenancy/tenancygrpc"
)

func testGatewayConfig(jwksURL string) gatewayConfig {
	return gatewayConfig{
		Addr:                 ":0",
		OIDCIssuer:           testIssuer,
		OIDCJWKSURL:          jwksURL,
		OIDCAudience:         testAudience,
		QueryGRPCAddr:        "dns:///query.invalid:9200",
		ControlPlaneGRPCAddr: "dns:///control-plane.invalid:9100",
		HubHTTPURL:           "http://127.0.0.1:1",
		RedisAddr:            "127.0.0.1:1",
		RateLimitPerMinute:   600,
		CORSAllowedOrigins:   "http://localhost:3000",
		MaxUploadMB:          32,
		MaxMediaMB:           25,
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newFakeDeps returns deps sufficient for routes that touch no backend (the
// M0 routes). The gRPC clients are nil: calling them would be a test bug.
func newFakeDeps(t *testing.T) *deps {
	t.Helper()
	return &deps{
		hubURL:         "http://127.0.0.1:1",
		hubClient:      &http.Client{Timeout: time.Second},
		mediaClient:    &http.Client{Timeout: time.Second},
		counter:        &fakeCounter{},
		maxUploadBytes: 32 << 20,
		maxMediaBytes:  25 << 20,
		logger:         discardLogger(),
	}
}

// fakeCounter is an in-memory rateCounter.
type fakeCounter struct {
	mu     sync.Mutex
	counts map[string]int64
	ttls   map[string]time.Duration
	err    error
	calls  int
}

func (f *fakeCounter) Incr(_ context.Context, key string, ttl time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	if f.counts == nil {
		f.counts = map[string]int64{}
		f.ttls = map[string]time.Duration{}
	}
	f.counts[key]++
	f.ttls[key] = ttl
	return f.counts[key], nil
}

func (f *fakeCounter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeRecent is an in-memory recentSearchStore: most-recent-first, deduped,
// capped — mirroring the Redis list semantics so handler tests are realistic.
type fakeRecent struct {
	mu   sync.Mutex
	data map[string][]string // key -> list (front = newest)
	err  error
}

func newFakeRecent() *fakeRecent { return &fakeRecent{data: map[string][]string{}} }

func (f *fakeRecent) RecordRecent(_ context.Context, key, value string, maxN int, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	out := []string{value}
	for _, v := range f.data[key] {
		if v != value {
			out = append(out, v)
		}
	}
	if len(out) > maxN {
		out = out[:maxN]
	}
	f.data[key] = out
	return nil
}

func (f *fakeRecent) RecentList(_ context.Context, key string, n int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	list := f.data[key]
	if n < len(list) {
		list = list[:n]
	}
	return append([]string(nil), list...), nil
}

func (f *fakeRecent) RemoveRecent(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	out := []string{}
	for _, v := range f.data[key] {
		if v != value {
			out = append(out, v)
		}
	}
	f.data[key] = out
	return nil
}

func (f *fakeRecent) ClearRecent(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	delete(f.data, key)
	return nil
}

func (f *fakeRecent) snapshot(key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.data[key]...)
}

// fakePrefs is an in-memory prefWriteStore: it captures the personalization
// write-through cache so tests can assert what the gateway cached under
// RedisProfileKey/RedisWeightsKey and that reset drops the model key. nil-safe
// in the handler (a Set/DeleteKey error never fails the request); the err knobs
// let a test exercise that degradation.
type fakePrefs struct {
	mu      sync.Mutex
	vals    map[string]string
	deleted []string
	setErr  error
	delErr  error
}

func newFakePrefs() *fakePrefs { return &fakePrefs{vals: map[string]string{}} }

func (f *fakePrefs) Set(_ context.Context, key, value string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.vals[key] = value
	return nil
}

func (f *fakePrefs) DeleteKey(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		return f.delErr
	}
	delete(f.vals, key)
	f.deleted = append(f.deleted, key)
	return nil
}

// get returns the cached value and whether the key is present.
func (f *fakePrefs) get(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vals[key]
	return v, ok
}

// wasDeleted reports whether DeleteKey was ever called for key.
func (f *fakePrefs) wasDeleted(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.deleted {
		if k == key {
			return true
		}
	}
	return false
}

// fakeQuery captures the tenant (as installed by the REAL tenancygrpc server
// interceptor) and the request, then plays back a canned response.
type fakeQuery struct {
	queryv1.UnimplementedQueryServiceServer
	mu        sync.Mutex
	gotTenant tenancy.TenantID
	gotReq    *queryv1.SearchRequest
	resp      *queryv1.SearchResponse
	err       error
}

func (f *fakeQuery) Search(ctx context.Context, req *queryv1.SearchRequest) (*queryv1.SearchResponse, error) {
	tc, err := tenancy.FromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "no tenant in context")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotTenant = tc.TenantID()
	f.gotReq = req
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &queryv1.SearchResponse{}, nil
}

func (f *fakeQuery) captured() (tenancy.TenantID, *queryv1.SearchRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotTenant, f.gotReq
}

// fakeControlPlane is a tenant-scoped in-memory control plane: instances
// created under one tenant are invisible (NotFound) to every other tenant,
// which lets tests prove the gateway forwards the JWT tenant on each RPC.
type fakeControlPlane struct {
	controlplanev1.UnimplementedControlPlaneServiceServer
	mu             sync.Mutex
	nextID         int
	ensured        []string
	instances      map[string]map[string]*controlplanev1.ConnectorInstance
	order          map[string][]string
	tokens         map[string][]byte
	deletedTenants []string

	// Per-tenant personalization store (v3.2): keyed off the caller tenant the
	// tenancygrpc server interceptor installs from the JWT, so one tenant's
	// preferences/model are invisible to every other tenant. profileJSON is the
	// verbatim Profile the gateway persisted; version is bumped on each PUT;
	// weightsJSON is the learned LearnedModel; sampleCount tracks feedback events.
	profiles    map[string]string
	versions    map[string]int64
	weights     map[string]string
	sampleCount map[string]int64
	// recordedFeedback captures the FeedbackEvents per tenant so tests can assert
	// the gateway forwarded the body to RecordFeedback.
	recordedFeedback map[string][]*controlplanev1.FeedbackEvent

	// injectable failures
	ensureErr   error
	listErr     error
	syncErr     error
	getPersErr  error
	putPrefErr  error
	feedbackErr error
	resetErr    error
}

func newFakeControlPlane() *fakeControlPlane {
	return &fakeControlPlane{
		instances:        map[string]map[string]*controlplanev1.ConnectorInstance{},
		order:            map[string][]string{},
		tokens:           map[string][]byte{},
		profiles:         map[string]string{},
		versions:         map[string]int64{},
		weights:          map[string]string{},
		sampleCount:      map[string]int64{},
		recordedFeedback: map[string][]*controlplanev1.FeedbackEvent{},
	}
}

func fakeCallerTenant(ctx context.Context) (string, error) {
	tc, err := tenancy.FromContext(ctx)
	if err != nil {
		return "", status.Error(codes.Unauthenticated, "no tenant in context")
	}
	return string(tc.TenantID()), nil
}

var fakeNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func (f *fakeControlPlane) EnsureTenant(ctx context.Context, _ *controlplanev1.EnsureTenantRequest) (*controlplanev1.EnsureTenantResponse, error) {
	tenant, err := fakeCallerTenant(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ensureErr != nil {
		return nil, f.ensureErr
	}
	f.ensured = append(f.ensured, tenant)
	return &controlplanev1.EnsureTenantResponse{
		Tenant: &controlplanev1.Tenant{TenantId: tenant, Created: timestamppb.New(fakeNow)},
	}, nil
}

func (f *fakeControlPlane) CreateConnectorInstance(ctx context.Context, req *controlplanev1.CreateConnectorInstanceRequest) (*controlplanev1.CreateConnectorInstanceResponse, error) {
	tenant, err := fakeCallerTenant(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetConnectorId() == "" {
		return nil, status.Error(codes.InvalidArgument, "connector_id is required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	inst := &controlplanev1.ConnectorInstance{
		Id:          fmt.Sprintf("inst-%d", f.nextID),
		ConnectorId: req.GetConnectorId(),
		DisplayName: req.GetDisplayName(),
		ConfigJson:  req.GetConfigJson(),
		Status:      controlplanev1.ConnectorStatus_ACTIVE,
		Created:     timestamppb.New(fakeNow),
		Updated:     timestamppb.New(fakeNow),
	}
	if f.instances[tenant] == nil {
		f.instances[tenant] = map[string]*controlplanev1.ConnectorInstance{}
	}
	f.instances[tenant][inst.GetId()] = inst
	f.order[tenant] = append(f.order[tenant], inst.GetId())
	return &controlplanev1.CreateConnectorInstanceResponse{Instance: inst}, nil
}

func (f *fakeControlPlane) ListConnectorInstances(ctx context.Context, _ *controlplanev1.ListConnectorInstancesRequest) (*controlplanev1.ListConnectorInstancesResponse, error) {
	tenant, err := fakeCallerTenant(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]*controlplanev1.ConnectorInstance, 0, len(f.order[tenant]))
	for _, id := range f.order[tenant] {
		if inst, ok := f.instances[tenant][id]; ok {
			out = append(out, inst)
		}
	}
	return &controlplanev1.ListConnectorInstancesResponse{Instances: out}, nil
}

func (f *fakeControlPlane) DeleteConnectorInstance(ctx context.Context, req *controlplanev1.DeleteConnectorInstanceRequest) (*controlplanev1.DeleteConnectorInstanceResponse, error) {
	tenant, err := fakeCallerTenant(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.instances[tenant][req.GetId()]; !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	delete(f.instances[tenant], req.GetId())
	return &controlplanev1.DeleteConnectorInstanceResponse{}, nil
}

func (f *fakeControlPlane) GetSyncState(ctx context.Context, req *controlplanev1.GetSyncStateRequest) (*controlplanev1.GetSyncStateResponse, error) {
	tenant, err := fakeCallerTenant(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.syncErr != nil {
		return nil, f.syncErr
	}
	if _, ok := f.instances[tenant][req.GetConnectorInstanceId()]; !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return &controlplanev1.GetSyncStateResponse{
		State: &controlplanev1.SyncState{
			ConnectorInstanceId: req.GetConnectorInstanceId(),
			Phase:               controlplanev1.SyncPhase_PENDING,
		},
	}, nil
}

func (f *fakeControlPlane) PutToken(ctx context.Context, req *controlplanev1.PutTokenRequest) (*controlplanev1.PutTokenResponse, error) {
	tenant, err := fakeCallerTenant(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.instances[tenant][req.GetConnectorInstanceId()]; !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	f.tokens[tenant+"/"+req.GetConnectorInstanceId()] = req.GetToken()
	return &controlplanev1.PutTokenResponse{}, nil
}

// DeleteTenant records the (caller tenant, confirm) pair so the gateway test
// can assert the gateway derives the confirm token from the verified tenant —
// never from the request body — and returns a canned erasure report.
func (f *fakeControlPlane) DeleteTenant(ctx context.Context, req *controlplanev1.DeleteTenantRequest) (*controlplanev1.DeleteTenantResponse, error) {
	tenant, err := fakeCallerTenant(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetConfirm() != tenant {
		return nil, status.Error(codes.InvalidArgument, "confirm must equal caller tenant")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedTenants = append(f.deletedTenants, tenant)
	delete(f.instances, tenant)
	return &controlplanev1.DeleteTenantResponse{Report: &controlplanev1.DeleteReport{
		TenantId: tenant, DekDestroyed: true, VespaGroupPurged: true,
		RedisPurged: true, VerifiedEmpty: true,
	}}, nil
}

// --- Personalization RPCs (v3.2), per-tenant in-memory ----------------------

// GetPersonalization returns the caller tenant's stored profile/model (empty
// strings when nothing was ever saved, so the gateway substitutes defaults).
func (f *fakeControlPlane) GetPersonalization(ctx context.Context, _ *controlplanev1.GetPersonalizationRequest) (*controlplanev1.GetPersonalizationResponse, error) {
	tenant, err := fakeCallerTenant(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getPersErr != nil {
		return nil, f.getPersErr
	}
	profile, exists := f.profiles[tenant]
	return &controlplanev1.GetPersonalizationResponse{
		ProfileJson: profile,
		Version:     f.versions[tenant],
		WeightsJson: f.weights[tenant],
		SampleCount: f.sampleCount[tenant],
		Exists:      exists,
	}, nil
}

// PutPreferences stores the profile verbatim under the caller tenant and bumps
// the version (mirroring the real Postgres-of-record behavior).
func (f *fakeControlPlane) PutPreferences(ctx context.Context, req *controlplanev1.PutPreferencesRequest) (*controlplanev1.PutPreferencesResponse, error) {
	tenant, err := fakeCallerTenant(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putPrefErr != nil {
		return nil, f.putPrefErr
	}
	f.profiles[tenant] = req.GetProfileJson()
	f.versions[tenant]++
	return &controlplanev1.PutPreferencesResponse{Version: f.versions[tenant]}, nil
}

// RecordFeedback captures the event, increments the per-tenant sample count, and
// returns a non-empty learned model so the gateway write-through fires.
func (f *fakeControlPlane) RecordFeedback(ctx context.Context, req *controlplanev1.RecordFeedbackRequest) (*controlplanev1.RecordFeedbackResponse, error) {
	tenant, err := fakeCallerTenant(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.feedbackErr != nil {
		return nil, f.feedbackErr
	}
	f.recordedFeedback[tenant] = append(f.recordedFeedback[tenant], req.GetEvent())
	f.sampleCount[tenant]++
	weights := fmt.Sprintf(`{"weights":{},"bias":0,"samples":%d}`, f.sampleCount[tenant])
	f.weights[tenant] = weights
	return &controlplanev1.RecordFeedbackResponse{
		WeightsJson: weights,
		SampleCount: f.sampleCount[tenant],
	}, nil
}

// ResetLearning drops the caller tenant's learned model + feedback history,
// returning how many feedback events were removed.
func (f *fakeControlPlane) ResetLearning(ctx context.Context, _ *controlplanev1.ResetLearningRequest) (*controlplanev1.ResetLearningResponse, error) {
	tenant, err := fakeCallerTenant(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resetErr != nil {
		return nil, f.resetErr
	}
	deleted := int64(len(f.recordedFeedback[tenant]))
	delete(f.recordedFeedback, tenant)
	delete(f.weights, tenant)
	delete(f.sampleCount, tenant)
	return &controlplanev1.ResetLearningResponse{FeedbackDeleted: deleted}, nil
}

// storedProfile returns the verbatim profile JSON the gateway persisted for a
// tenant (or "" when none).
func (f *fakeControlPlane) storedProfile(tenant string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.profiles[tenant]
}

// recordedFeedbackFor returns a snapshot of the events recorded for a tenant.
func (f *fakeControlPlane) recordedFeedbackFor(tenant string) []*controlplanev1.FeedbackEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*controlplanev1.FeedbackEvent(nil), f.recordedFeedback[tenant]...)
}

// fakeAdmin is the in-proc AdminService for gateway admin-route tests. It
// records the target tenants it acted on; the gateway's admin-claim gate is
// what these tests exercise, so the fake's logic is minimal.
type fakeAdmin struct {
	controlplanev1.UnimplementedAdminServiceServer
	mu            sync.Mutex
	listed        bool
	deletedTenant string
	suspended     map[string]bool
}

func newFakeAdmin() *fakeAdmin { return &fakeAdmin{suspended: map[string]bool{}} }

func (a *fakeAdmin) ListTenants(_ context.Context, _ *controlplanev1.ListTenantsRequest) (*controlplanev1.ListTenantsResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listed = true
	return &controlplanev1.ListTenantsResponse{
		Tenants: []*controlplanev1.TenantUsage{{TenantId: "tenant-x", ConnectorInstances: 2}},
	}, nil
}

func (a *fakeAdmin) GetTenantUsage(_ context.Context, req *controlplanev1.GetTenantUsageRequest) (*controlplanev1.GetTenantUsageResponse, error) {
	return &controlplanev1.GetTenantUsageResponse{Usage: &controlplanev1.TenantUsage{TenantId: req.GetTenantId()}}, nil
}

func (a *fakeAdmin) SuspendTenant(_ context.Context, req *controlplanev1.SuspendTenantRequest) (*controlplanev1.SuspendTenantResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.suspended[req.GetTenantId()] = req.GetSuspended()
	return &controlplanev1.SuspendTenantResponse{InstancesChanged: 1}, nil
}

func (a *fakeAdmin) AdminDeleteTenant(_ context.Context, req *controlplanev1.AdminDeleteTenantRequest) (*controlplanev1.AdminDeleteTenantResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deletedTenant = req.GetTenantId()
	return &controlplanev1.AdminDeleteTenantResponse{Report: &controlplanev1.DeleteReport{
		TenantId: req.GetTenantId(), DekDestroyed: true, VerifiedEmpty: true,
	}}, nil
}

// startGRPCBackends serves the fakes on a loopback listener behind the real
// tenancygrpc server interceptor and returns a client conn that uses the real
// tenancygrpc client interceptor — the exact production wiring. The admin
// methods are exempted from the tenant requirement, mirroring the real
// control-plane main.go wiring.
func startGRPCBackends(t *testing.T, query queryv1.QueryServiceServer, control controlplanev1.ControlPlaneServiceServer, admin controlplanev1.AdminServiceServer) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.UnaryInterceptor(tenancygrpc.UnaryServerInterceptor()))
	queryv1.RegisterQueryServiceServer(srv, query)
	controlplanev1.RegisterControlPlaneServiceServer(srv, control)
	if admin != nil {
		controlplanev1.RegisterAdminServiceServer(srv, admin)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(tenancygrpc.UnaryClientInterceptor()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// testEnv is a fully wired gateway handler over in-proc fakes.
type testEnv struct {
	t       *testing.T
	idp     *testIdP
	handler http.Handler
	query   *fakeQuery
	control *fakeControlPlane
	admin   *fakeAdmin
	counter *fakeCounter
	recent  *fakeRecent
	prefs   *fakePrefs
	deps    *deps
	cfg     gatewayConfig
}

func newTestEnv(t *testing.T, opts ...func(cfg *gatewayConfig, d *deps)) *testEnv {
	t.Helper()
	idp := newTestIdP(t)
	query := &fakeQuery{}
	control := newFakeControlPlane()
	admin := newFakeAdmin()
	conn := startGRPCBackends(t, query, control, admin)

	counter := &fakeCounter{}
	recent := newFakeRecent()
	prefs := newFakePrefs()
	cfg := testGatewayConfig(idp.jwks.URL)
	d := &deps{
		query:          queryv1.NewQueryServiceClient(conn),
		control:        controlplanev1.NewControlPlaneServiceClient(conn),
		admin:          controlplanev1.NewAdminServiceClient(conn),
		hubURL:         "http://127.0.0.1:1",
		hubClient:      &http.Client{Timeout: 5 * time.Second},
		mediaClient:    &http.Client{Timeout: 5 * time.Second},
		counter:        counter,
		recent:         recent,
		prefs:          prefs,
		maxUploadBytes: cfg.MaxUploadMB << 20,
		maxMediaBytes:  cfg.MaxMediaMB << 20,
		oidcAudience:   cfg.OIDCAudience,
		maxQueryChars:  cfg.MaxQueryChars,
		logger:         discardLogger(),
	}
	for _, opt := range opts {
		opt(&cfg, d)
	}
	auth := newAuthenticator(t.Context(), cfg.OIDCIssuer, cfg.OIDCJWKSURL, cfg.OIDCAudience, discardLogger())
	return &testEnv{
		t:       t,
		idp:     idp,
		handler: newHandler(cfg, auth, d),
		query:   query,
		control: control,
		admin:   admin,
		counter: counter,
		recent:  recent,
		prefs:   prefs,
		deps:    d,
		cfg:     cfg,
	}
}

func (e *testEnv) bearer() string {
	return "Bearer " + e.idp.mint(e.t, baseClaims())
}

func (e *testEnv) bearerFor(sub string) string {
	claims := baseClaims()
	claims["sub"] = sub
	return "Bearer " + e.idp.mint(e.t, claims)
}

// do sends an authenticated request through the full handler chain.
func (e *testEnv) do(method, path string, body io.Reader, hdr http.Header) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", e.bearer())
	for k, vs := range hdr {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// doWithClaims sends a request authenticated by a token minted from the given
// claims (so a test can set/omit the admin role/scope).
func (e *testEnv) doWithClaims(method, path string, body io.Reader, claims map[string]any) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+e.idp.mint(e.t, claims))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// newRawRequest builds an unauthenticated request + recorder pair.
func newRawRequest(method, path string) (*http.Request, *httptest.ResponseRecorder) {
	return httptest.NewRequest(method, path, nil), httptest.NewRecorder()
}

func decodeObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not a JSON object: %v (body: %q)", err, rec.Body.String())
	}
	return out
}

func decodeArray(t *testing.T, rec *httptest.ResponseRecorder) []any {
	t.Helper()
	var out []any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not a JSON array: %v (body: %q)", err, rec.Body.String())
	}
	return out
}
