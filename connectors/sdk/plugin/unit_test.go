package plugin

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/asker/asker/connectors/sdk"
	pluginv1 "github.com/asker/asker/platform/proto/gen/go/asker/plugin/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// TestAuthTypeMappingRoundTrips checks every sdk.AuthType maps to its enum and
// back, and that the unspecified/unknown enum values default to AuthNone.
func TestAuthTypeMappingRoundTrips(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   sdk.AuthType
		want pluginv1.AuthType
	}{
		{sdk.AuthNone, pluginv1.AuthType_AUTH_NONE},
		{sdk.AuthOAuth2, pluginv1.AuthType_AUTH_OAUTH2},
		{sdk.AuthToken, pluginv1.AuthType_AUTH_TOKEN},
	}
	for _, tc := range cases {
		if got := authTypeToProto(tc.in); got != tc.want {
			t.Errorf("authTypeToProto(%v) = %v, want %v", tc.in, got, tc.want)
		}
		if got := authTypeFromProto(tc.want); got != tc.in {
			t.Errorf("authTypeFromProto(%v) = %v, want %v", tc.want, got, tc.in)
		}
	}

	// Unknown sdk auth type -> UNSPECIFIED (never silently a known type).
	if got := authTypeToProto(sdk.AuthType(99)); got != pluginv1.AuthType_AUTH_TYPE_UNSPECIFIED {
		t.Errorf("authTypeToProto(unknown) = %v, want UNSPECIFIED", got)
	}
	// Unspecified / unknown enum -> AuthNone (safe default).
	if got := authTypeFromProto(pluginv1.AuthType_AUTH_TYPE_UNSPECIFIED); got != sdk.AuthNone {
		t.Errorf("authTypeFromProto(UNSPECIFIED) = %v, want AuthNone", got)
	}
	if got := authTypeFromProto(pluginv1.AuthType(99)); got != sdk.AuthNone {
		t.Errorf("authTypeFromProto(unknown) = %v, want AuthNone", got)
	}
}

// TestStripUnixScheme exercises the unix-target parsing both schemes use.
func TestStripUnixScheme(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		want   string
		isUnix bool
	}{
		{"unix:///run/asker/x.sock", "/run/asker/x.sock", true},
		{"unix:relative.sock", "relative.sock", true},
		{"127.0.0.1:8090", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := stripUnixScheme(tc.in)
		if ok != tc.isUnix || got != tc.want {
			t.Errorf("stripUnixScheme(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.isUnix)
		}
	}
}

// TestListenAndServeOverUnix starts a plugin via ListenAndServe on a unix
// socket, dials it, runs a real RPC, and asserts ctx cancellation stops the
// server cleanly (ListenAndServe returns nil).
func TestListenAndServeOverUnix(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "plugin.sock")
	target := "unix://" + sock

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- ListenAndServe(ctx, target, &fakeConnector{fullSyncDocs: 2, fullSyncCursor: "c"}) }()

	// Wait for the socket to exist so the dial doesn't race startup.
	waitForSocket(t, sock)

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	cli, err := Dial(dialCtx, target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		cancel()
		t.Fatalf("dial unix: %v", err)
	}
	cfg, err := testConfig(sdk.NopCheckpoint)
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}
	cur, err := cli.FullSync(context.Background(), cfg, (&sinkEmit{}).emit)
	if err != nil {
		t.Fatalf("FullSync over unix: %v", err)
	}
	if cur != sdk.Cursor("c") {
		t.Errorf("cursor = %q, want c", cur)
	}
	_ = cli.Close()

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("ListenAndServe returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after ctx cancel")
	}
}

// TestListenAndServeBadAddr asserts a listen failure is returned, not swallowed.
func TestListenAndServeBadAddr(t *testing.T) {
	t.Parallel()
	// Port 0 with an unroutable host forces a listen error.
	err := ListenAndServe(context.Background(), "256.256.256.256:99999", &fakeConnector{})
	if err == nil {
		t.Fatal("ListenAndServe with a bad addr returned nil, want an error")
	}
}

// failingListener is a net.Listener whose Accept returns a permanent error
// immediately, so a grpc.Server's Serve loop gives up and returns that error.
// It models a serve-time failure that is NOT triggered by ctx cancellation.
type failingListener struct {
	addr   net.Addr
	closed chan struct{}
}

func newFailingListener() *failingListener {
	return &failingListener{
		addr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0},
		closed: make(chan struct{}),
	}
}

// errAcceptFatal is the non-temporary error Accept reports so Serve stops.
var errAcceptFatal = errors.New("plugin_test: accept failed permanently")

func (l *failingListener) Accept() (net.Conn, error) { return nil, errAcceptFatal }

func (l *failingListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *failingListener) Addr() net.Addr { return l.addr }

// TestServeOnSurfacesServeErrorWithoutLeaking is the regression guard for the
// stop-goroutine deadlock: when gs.Serve returns on its own (a listener/serve
// failure) while ctx is NOT canceled, serveOn must still unblock its stop
// goroutine and return the Serve error promptly instead of parking forever on
// <-done. Before the fix the stop goroutine waited only on ctx.Done(), so this
// call would hang until the test deadline.
func TestServeOnSurfacesServeErrorWithoutLeaking(t *testing.T) {
	t.Parallel()
	// A live, never-canceled context: the failure must come from Serve, not ctx.
	ctx := context.Background()

	ret := make(chan error, 1)
	go func() { ret <- serveOn(ctx, newFailingListener(), &fakeConnector{}) }()

	select {
	case err := <-ret:
		// gs.Serve surfaces the Accept error; with ctx live it must NOT be
		// swallowed as a clean shutdown.
		if err == nil {
			t.Fatal("serveOn returned nil, want the serve error surfaced (ctx was never canceled)")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveOn did not return after Serve failed; the stop goroutine deadlocked")
	}
}

// TestDialUnreachable asserts Dial surfaces a Spec failure when the target is
// not serving.
func TestDialUnreachable(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// 127.0.0.1:1 is reserved and refuses connections quickly.
	_, err := Dial(ctx, "127.0.0.1:1",
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err == nil {
		t.Fatal("Dial to an unreachable target returned nil error")
	}
}

// TestValidateErrorNonStatus asserts validateError passes a non-gRPC error
// through unchanged.
func TestValidateErrorNonStatus(t *testing.T) {
	t.Parallel()
	plain := errors.New("not a status error")
	if got := validateError(plain); !errors.Is(got, plain) {
		t.Errorf("validateError(plain) = %v, want the same error", got)
	}
}

// TestWebhookEmptyDoneNoCursor asserts a webhook that emits nothing completes
// cleanly: the server sends an empty Done and the client returns nil.
func TestWebhookEmptyDoneNoCursor(t *testing.T) {
	t.Parallel()
	conn := &fakeConnector{supportsWebhook: true, webhookDocs: 0}
	cli := serveConn(t, conn)
	cfg, err := testConfig(sdk.NopCheckpoint)
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}
	req := makeWebhookReq()
	if err := cli.HandleWebhook(context.Background(), cfg, req, (&sinkEmit{}).emit); err != nil {
		t.Fatalf("HandleWebhook (no docs) = %v, want nil", err)
	}
}

// TestHttpRequestToProtoRebuffers asserts serializing a request leaves it
// readable by the caller afterward (the body is re-buffered).
func TestHttpRequestToProtoRebuffers(t *testing.T) {
	t.Parallel()
	body := []byte("hello")
	req := makeWebhookReqBody(body)
	if _, err := httpRequestToProto(req); err != nil {
		t.Fatalf("httpRequestToProto: %v", err)
	}
	got := readAll(t, req.Body)
	if string(got) != string(body) {
		t.Errorf("after serialize, body = %q, want %q (re-buffered)", got, body)
	}
	if _, err := httpRequestToProto(nil); err == nil {
		t.Error("httpRequestToProto(nil) = nil error, want error")
	}
}

// TestConfigFromProtoNil asserts a nil wire Config is rejected.
func TestConfigFromProtoNil(t *testing.T) {
	t.Parallel()
	if _, err := configFromProto(nil, sdk.NopCheckpoint); err == nil {
		t.Error("configFromProto(nil) = nil error, want error")
	}
}

// TestHttpRequestFromProtoDefaultsMethod asserts an empty method defaults to
// POST and a nil request is rejected.
func TestHttpRequestFromProtoDefaultsMethod(t *testing.T) {
	t.Parallel()
	req, err := httpRequestFromProto(context.Background(), &pluginv1.HttpRequest{Url: "/x"})
	if err != nil {
		t.Fatalf("httpRequestFromProto: %v", err)
	}
	if req.Method != http.MethodPost {
		t.Errorf("default method = %q, want POST", req.Method)
	}
	if _, err := httpRequestFromProto(context.Background(), nil); err == nil {
		t.Error("httpRequestFromProto(nil) = nil error, want error")
	}
}

// TestSyncInternalErrorMapsBack asserts a non-sentinel connector error surfaces
// as a transport error (codes.Internal) and is NOT mistaken for cursor-expired.
func TestSyncInternalErrorMapsBack(t *testing.T) {
	t.Parallel()
	boom := errors.New("source unreachable")
	conn := &fakeConnector{incrementalErr: boom}
	cli := serveConn(t, conn)

	cfg, err := testConfig(sdk.NopCheckpoint)
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}
	_, err = cli.IncrementalSync(context.Background(), cfg, sdk.Cursor("c"), (&sinkEmit{}).emit)
	if err == nil {
		t.Fatal("IncrementalSync returned nil, want an error")
	}
	if errors.Is(err, sdk.ErrCursorExpired) {
		t.Fatal("a plain connector error was mis-mapped to ErrCursorExpired")
	}
}

// TestServerRejectsInvalidTenantOnStream asserts the server fails closed when a
// streaming RPC carries a malformed tenant_id (defense against a hostile hub):
// the connector never runs and the client sees an error.
func TestServerRejectsInvalidTenantOnStream(t *testing.T) {
	t.Parallel()
	conn := &fakeConnector{fullSyncDocs: 3, fullSyncCursor: "c"}
	cli := serveConn(t, conn)

	// Call the underlying RPC directly with a bad tenant so the server's
	// configFromProto rejects it before the connector runs.
	stream, err := cli.rpc.FullSync(context.Background(), &pluginv1.FullSyncRequest{Config: badTenantConfig()})
	if err != nil {
		t.Fatalf("open FullSync: %v", err)
	}
	if _, rerr := stream.Recv(); rerr == nil {
		t.Fatal("FullSync with an invalid tenant returned an event, want an error")
	}
}

// TestDrainSyncFakeStream exercises drainSync's branches that a live happy-path
// stream does not reach: an unexpected (empty) event and an EOF without a Done.
func TestDrainSyncFakeStream(t *testing.T) {
	t.Parallel()
	cfg, err := testConfig(sdk.NopCheckpoint)
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}

	// Unexpected empty event -> error.
	empty := &scriptedStream{events: []*pluginv1.SyncEvent{{}}}
	if _, err := drainSync(context.Background(), func() {}, empty, cfg, (&sinkEmit{}).emit); err == nil {
		t.Error("drainSync with an empty event returned nil, want an error")
	}

	// EOF before Done -> empty cursor, no error (defensive).
	eofStream := &scriptedStream{events: nil}
	cur, err := drainSync(context.Background(), func() {}, eofStream, cfg, (&sinkEmit{}).emit)
	if err != nil || cur != "" {
		t.Errorf("drainSync on EOF = (%q,%v), want (\"\",nil)", cur, err)
	}
}

// scriptedStream is a fake syncStream that replays a fixed event slice then EOF.
type scriptedStream struct {
	events []*pluginv1.SyncEvent
	i      int
}

func (s *scriptedStream) Recv() (*pluginv1.SyncEvent, error) {
	if s.i >= len(s.events) {
		return nil, io.EOF
	}
	ev := s.events[s.i]
	s.i++
	return ev, nil
}

// sinkEmit discards emitted documents; used where the test only cares about the
// returned cursor or error.
type sinkEmit struct{ n int }

func (s *sinkEmit) emit(context.Context, *askerv1.Document) error { s.n++; return nil }

// waitForSocket blocks until the unix socket path exists or the test times out.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("socket %q never appeared", path)
}
