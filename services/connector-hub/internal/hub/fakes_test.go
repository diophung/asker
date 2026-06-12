package hub

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
	"github.com/asker/asker/platform/tenancy/tenancygrpc"
)

const (
	tenantA = "tenant-a"
	tenantB = "tenant-b"
)

func mustTenant(t *testing.T, id string) tenancy.Context {
	t.Helper()
	tc, err := tenancy.FromHeaderValue(id)
	if err != nil {
		t.Fatalf("FromHeaderValue(%q): %v", id, err)
	}
	return tc
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v: %s", timeout, msg)
}

// ---------------------------------------------------------------------------
// Fake control plane (ControlPlaneService + SchedulerService) served over a
// REAL gRPC server on 127.0.0.1:0. The tenant-scoped methods enforce the
// production contract themselves: exactly one valid x-asker-tenant metadata
// value, rows scoped to it, ErrNotFound shape for foreign rows.

type stateWrite struct {
	tenant string
	state  *controlplanev1.SyncState
}

type fakeControlPlane struct {
	controlplanev1.UnimplementedControlPlaneServiceServer
	controlplanev1.UnimplementedSchedulerServiceServer

	mu        sync.Mutex
	instances map[string]*controlplanev1.TenantInstance
	states    map[string]*controlplanev1.SyncState
	tokens    map[string][]byte
	stateLog  []stateWrite
	listCalls int
}

func newFakeControlPlane() *fakeControlPlane {
	return &fakeControlPlane{
		instances: make(map[string]*controlplanev1.TenantInstance),
		states:    make(map[string]*controlplanev1.SyncState),
		tokens:    make(map[string][]byte),
	}
}

// addInstance registers an instance for tenant and returns its ID.
func (f *fakeControlPlane) addInstance(id, tenant, connectorID string, configJSON []byte, st controlplanev1.ConnectorStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(configJSON) == 0 {
		configJSON = []byte("{}")
	}
	f.instances[id] = &controlplanev1.TenantInstance{
		TenantId: tenant,
		Instance: &controlplanev1.ConnectorInstance{
			Id:          id,
			ConnectorId: connectorID,
			DisplayName: connectorID + " test",
			ConfigJson:  configJSON,
			Status:      st,
			Created:     timestamppb.New(time.Now().UTC()),
			Updated:     timestamppb.New(time.Now().UTC()),
		},
	}
}

func (f *fakeControlPlane) setStatus(id string, st controlplanev1.ConnectorStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances[id].Instance.Status = st
}

func (f *fakeControlPlane) removeInstance(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.instances, id)
}

func (f *fakeControlPlane) setToken(id string, token []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[id] = token
}

func (f *fakeControlPlane) seedState(st *controlplanev1.SyncState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[st.GetConnectorInstanceId()] = proto.Clone(st).(*controlplanev1.SyncState)
}

func (f *fakeControlPlane) writes() []stateWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]stateWrite, len(f.stateLog))
	for i, w := range f.stateLog {
		out[i] = stateWrite{tenant: w.tenant, state: proto.Clone(w.state).(*controlplanev1.SyncState)}
	}
	return out
}

func (f *fakeControlPlane) lastWrite() (stateWrite, bool) {
	ws := f.writes()
	if len(ws) == 0 {
		return stateWrite{}, false
	}
	return ws[len(ws)-1], true
}

// callerTenant enforces the metadata contract for tenant-scoped methods.
func callerTenant(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing metadata")
	}
	vals := md.Get(tenancygrpc.MetadataKey)
	if len(vals) != 1 {
		return "", status.Errorf(codes.Unauthenticated, "want exactly 1 tenant metadata value, got %d", len(vals))
	}
	if _, err := tenancy.FromHeaderValue(vals[0]); err != nil {
		return "", status.Error(codes.Unauthenticated, "invalid tenant metadata")
	}
	return vals[0], nil
}

func (f *fakeControlPlane) ownedInstance(tenant, id string) (*controlplanev1.TenantInstance, bool) {
	ti, ok := f.instances[id]
	if !ok || ti.GetTenantId() != tenant {
		return nil, false
	}
	return ti, true
}

func (f *fakeControlPlane) ListAllInstances(_ context.Context, _ *controlplanev1.ListAllInstancesRequest) (*controlplanev1.ListAllInstancesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	out := make([]*controlplanev1.TenantInstance, 0, len(f.instances))
	for _, ti := range f.instances {
		out = append(out, proto.Clone(ti).(*controlplanev1.TenantInstance))
	}
	return &controlplanev1.ListAllInstancesResponse{Instances: out}, nil
}

func (f *fakeControlPlane) ListConnectorInstances(ctx context.Context, _ *controlplanev1.ListConnectorInstancesRequest) (*controlplanev1.ListConnectorInstancesResponse, error) {
	tenant, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*controlplanev1.ConnectorInstance
	for _, ti := range f.instances {
		if ti.GetTenantId() == tenant {
			out = append(out, proto.Clone(ti.GetInstance()).(*controlplanev1.ConnectorInstance))
		}
	}
	return &controlplanev1.ListConnectorInstancesResponse{Instances: out}, nil
}

func (f *fakeControlPlane) GetSyncState(ctx context.Context, req *controlplanev1.GetSyncStateRequest) (*controlplanev1.GetSyncStateResponse, error) {
	tenant, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.ownedInstance(tenant, req.GetConnectorInstanceId()); !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	st, ok := f.states[req.GetConnectorInstanceId()]
	if !ok {
		return &controlplanev1.GetSyncStateResponse{State: &controlplanev1.SyncState{
			ConnectorInstanceId: req.GetConnectorInstanceId(),
			Phase:               controlplanev1.SyncPhase_PENDING,
		}}, nil
	}
	return &controlplanev1.GetSyncStateResponse{State: proto.Clone(st).(*controlplanev1.SyncState)}, nil
}

func (f *fakeControlPlane) SetSyncState(ctx context.Context, req *controlplanev1.SetSyncStateRequest) (*controlplanev1.SetSyncStateResponse, error) {
	tenant, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	st := req.GetState()
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.ownedInstance(tenant, st.GetConnectorInstanceId()); !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	stored := proto.Clone(st).(*controlplanev1.SyncState)
	f.states[st.GetConnectorInstanceId()] = stored
	f.stateLog = append(f.stateLog, stateWrite{tenant: tenant, state: proto.Clone(stored).(*controlplanev1.SyncState)})
	return &controlplanev1.SetSyncStateResponse{State: proto.Clone(stored).(*controlplanev1.SyncState)}, nil
}

func (f *fakeControlPlane) GetToken(ctx context.Context, req *controlplanev1.GetTokenRequest) (*controlplanev1.GetTokenResponse, error) {
	tenant, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.ownedInstance(tenant, req.GetConnectorInstanceId()); !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	token, ok := f.tokens[req.GetConnectorInstanceId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return &controlplanev1.GetTokenResponse{Token: token}, nil
}

// startFakeControlPlane serves f on 127.0.0.1:0 and returns the production-
// shaped clients: a tenant-scoped ControlPlane client (tenancy client
// interceptor installed) and a bare Scheduler client (deliberately without).
func startFakeControlPlane(t *testing.T, f *fakeControlPlane) (controlplanev1.ControlPlaneServiceClient, controlplanev1.SchedulerServiceClient, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	srv := grpc.NewServer()
	controlplanev1.RegisterControlPlaneServiceServer(srv, f)
	controlplanev1.RegisterSchedulerServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	addr := lis.Addr().String()
	cpConn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(tenancygrpc.UnaryClientInterceptor()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient (cp): %v", err)
	}
	t.Cleanup(func() { _ = cpConn.Close() })
	schedConn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient (sched): %v", err)
	}
	t.Cleanup(func() { _ = schedConn.Close() })
	return controlplanev1.NewControlPlaneServiceClient(cpConn), controlplanev1.NewSchedulerServiceClient(schedConn), addr
}

// ---------------------------------------------------------------------------
// Fake connector

type fakeConnector struct {
	id string

	mu         sync.Mutex
	fullSyncs  int
	incSyncs   int
	webhooks   int
	configs    []sdk.Config
	incCursors []sdk.Cursor

	fullSyncFn func(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error)
	incFn      func(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error)
	webhookFn  func(ctx context.Context, cfg sdk.Config, r *http.Request, emit sdk.Emit) error
}

var _ sdk.Connector = (*fakeConnector)(nil)

func (c *fakeConnector) Spec() sdk.Spec {
	return sdk.Spec{ID: c.id, DisplayName: "Fake " + c.id, AuthType: sdk.AuthToken,
		ConfigSchema: []byte(`{"type":"object"}`), SupportsWebhook: c.webhookFn != nil}
}

func (c *fakeConnector) Validate(context.Context, sdk.Config) error { return nil }

func (c *fakeConnector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	c.mu.Lock()
	c.fullSyncs++
	c.configs = append(c.configs, cfg)
	fn := c.fullSyncFn
	c.mu.Unlock()
	if fn == nil {
		return "full-done", nil
	}
	return fn(ctx, cfg, emit)
}

func (c *fakeConnector) IncrementalSync(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	c.mu.Lock()
	c.incSyncs++
	c.configs = append(c.configs, cfg)
	c.incCursors = append(c.incCursors, cur)
	fn := c.incFn
	c.mu.Unlock()
	if fn == nil {
		return cur, nil
	}
	return fn(ctx, cfg, cur, emit)
}

func (c *fakeConnector) HandleWebhook(ctx context.Context, cfg sdk.Config, r *http.Request, emit sdk.Emit) error {
	c.mu.Lock()
	c.webhooks++
	c.configs = append(c.configs, cfg)
	fn := c.webhookFn
	c.mu.Unlock()
	if fn == nil {
		return sdk.ErrWebhookUnsupported
	}
	return fn(ctx, cfg, r, emit)
}

func (c *fakeConnector) counts() (full, inc, hooks int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fullSyncs, c.incSyncs, c.webhooks
}

func (c *fakeConnector) lastConfig() (sdk.Config, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.configs) == 0 {
		return sdk.Config{}, false
	}
	return c.configs[len(c.configs)-1], true
}

func (c *fakeConnector) cursorsSeen() []sdk.Cursor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]sdk.Cursor(nil), c.incCursors...)
}

// ---------------------------------------------------------------------------
// Fake producer

type producedDoc struct {
	topic  string
	tenant string // tenancy.Context tenant carried by ctx at produce time
	doc    *askerv1.Document
}

type fakeProducer struct {
	mu       sync.Mutex
	produced []producedDoc
	err      error
}

func (p *fakeProducer) ProduceDocument(ctx context.Context, topic string, doc *askerv1.Document) error {
	tc, err := tenancy.FromContext(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.produced = append(p.produced, producedDoc{
		topic:  topic,
		tenant: string(tc.TenantID()),
		doc:    proto.Clone(doc).(*askerv1.Document),
	})
	return nil
}

func (p *fakeProducer) docs() []producedDoc {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]producedDoc(nil), p.produced...)
}
