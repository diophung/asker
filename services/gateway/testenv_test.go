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
	mu        sync.Mutex
	nextID    int
	ensured   []string
	instances map[string]map[string]*controlplanev1.ConnectorInstance
	order     map[string][]string
	tokens    map[string][]byte
	// injectable failures
	ensureErr error
	listErr   error
	syncErr   error
}

func newFakeControlPlane() *fakeControlPlane {
	return &fakeControlPlane{
		instances: map[string]map[string]*controlplanev1.ConnectorInstance{},
		order:     map[string][]string{},
		tokens:    map[string][]byte{},
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

// startGRPCBackends serves both fakes on a loopback listener behind the real
// tenancygrpc server interceptor and returns a client conn that uses the real
// tenancygrpc client interceptor — the exact production wiring.
func startGRPCBackends(t *testing.T, query queryv1.QueryServiceServer, control controlplanev1.ControlPlaneServiceServer) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.UnaryInterceptor(tenancygrpc.UnaryServerInterceptor()))
	queryv1.RegisterQueryServiceServer(srv, query)
	controlplanev1.RegisterControlPlaneServiceServer(srv, control)
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
	counter *fakeCounter
	deps    *deps
	cfg     gatewayConfig
}

func newTestEnv(t *testing.T, opts ...func(cfg *gatewayConfig, d *deps)) *testEnv {
	t.Helper()
	idp := newTestIdP(t)
	query := &fakeQuery{}
	control := newFakeControlPlane()
	conn := startGRPCBackends(t, query, control)

	counter := &fakeCounter{}
	cfg := testGatewayConfig(idp.jwks.URL)
	d := &deps{
		query:          queryv1.NewQueryServiceClient(conn),
		control:        controlplanev1.NewControlPlaneServiceClient(conn),
		hubURL:         "http://127.0.0.1:1",
		hubClient:      &http.Client{Timeout: 5 * time.Second},
		mediaClient:    &http.Client{Timeout: 5 * time.Second},
		counter:        counter,
		maxUploadBytes: cfg.MaxUploadMB << 20,
		maxMediaBytes:  cfg.MaxMediaMB << 20,
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
		counter: counter,
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
