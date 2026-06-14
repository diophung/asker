package plugin

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	pluginv1 "github.com/asker/asker/platform/proto/gen/go/asker/plugin/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// serveConn starts a real gRPC server on 127.0.0.1:0 serving c, dials it, and
// returns a *Client with its Spec cached. It registers cleanup that closes the
// client and stops the server. A real TCP listener (not bufconn, which is not a
// dependency) is used so the parity suite exercises the actual transport.
func serveConn(t *testing.T, c sdk.Connector) *Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	pluginv1.RegisterConnectorPluginServiceServer(gs, NewServer(c))

	serveErr := make(chan error, 1)
	go func() { serveErr <- gs.Serve(lis) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cli, err := Dial(ctx, lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		gs.Stop()
		t.Fatalf("dial: %v", err)
	}

	t.Cleanup(func() {
		_ = cli.Close()
		gs.GracefulStop()
		if err := <-serveErr; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("serve: %v", err)
		}
	})
	return cli
}

// recordingCheckpoint is a Checkpoint that appends each cursor it receives. It
// is safe for concurrent use.
type recordingCheckpoint struct {
	mu      sync.Mutex
	cursors []sdk.Cursor
}

func (r *recordingCheckpoint) fn(_ context.Context, cur sdk.Cursor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cursors = append(r.cursors, cur)
	return nil
}

func (r *recordingCheckpoint) snapshot() []sdk.Cursor {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sdk.Cursor, len(r.cursors))
	copy(out, r.cursors)
	return out
}

// TestSpecParity asserts the cached client Spec equals the in-process Spec.
func TestSpecParity(t *testing.T) {
	t.Parallel()
	conn := &fakeConnector{supportsWebhook: true}
	cli := serveConn(t, conn)

	connectortest.RunSpecChecks(t, cli)

	got, want := cli.Spec(), conn.Spec()
	if got.ID != want.ID || got.DisplayName != want.DisplayName ||
		got.AuthType != want.AuthType || got.SupportsWebhook != want.SupportsWebhook {
		t.Fatalf("client Spec = %+v, want %+v", got, want)
	}
	if string(got.ConfigSchema) != string(want.ConfigSchema) {
		t.Fatalf("client ConfigSchema = %q, want %q", got.ConfigSchema, want.ConfigSchema)
	}
}

// TestFullSyncParity runs FullSync in-process and through the transport and
// asserts the emitted docs (proto.Equal), checkpoints, and returned cursor all
// match.
func TestFullSyncParity(t *testing.T) {
	t.Parallel()
	const nDocs = 4
	const finalCursor = sdk.Cursor("full-final")

	// In-process reference run.
	wantDocs, wantCheckpoints, wantCursor := runFullInProc(t, nDocs, finalCursor)

	// Transport run.
	conn := &fakeConnector{fullSyncDocs: nDocs, fullSyncCursor: finalCursor}
	cli := serveConn(t, conn)

	var cp recordingCheckpoint
	cfg, err := testConfig(cp.fn)
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}
	var rec connectortest.EmitRecorder
	gotCursor, err := cli.FullSync(context.Background(), cfg, rec.Emit)
	if err != nil {
		t.Fatalf("client FullSync: %v", err)
	}

	if gotCursor != wantCursor {
		t.Errorf("cursor = %q, want %q", gotCursor, wantCursor)
	}
	assertDocsEqual(t, rec.Docs(), wantDocs)
	assertCheckpointsEqual(t, cp.snapshot(), wantCheckpoints)
	for _, d := range rec.Docs() {
		connectortest.ValidateDocument(t, cfg, d)
	}
}

// runFullInProc runs the fake's FullSync directly and returns its emissions,
// checkpoints, and cursor as the parity reference.
func runFullInProc(t *testing.T, nDocs int, cursor sdk.Cursor) ([]*askerv1.Document, []sdk.Cursor, sdk.Cursor) {
	t.Helper()
	conn := &fakeConnector{fullSyncDocs: nDocs, fullSyncCursor: cursor}
	var cp recordingCheckpoint
	cfg, err := testConfig(cp.fn)
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}
	var rec connectortest.EmitRecorder
	got, err := conn.FullSync(context.Background(), cfg, rec.Emit)
	if err != nil {
		t.Fatalf("in-proc FullSync: %v", err)
	}
	return rec.Docs(), cp.snapshot(), got
}

// TestIncrementalCursorExpiredRoundTrips asserts a connector that emits some
// docs then returns sdk.ErrCursorExpired round-trips through the transport:
// errors.Is(err, sdk.ErrCursorExpired) holds and the docs emitted before the
// error are delivered.
func TestIncrementalCursorExpiredRoundTrips(t *testing.T) {
	t.Parallel()
	conn := &fakeConnector{incrementalDocs: 2, incrementalErr: sdk.ErrCursorExpired}
	cli := serveConn(t, conn)

	var cp recordingCheckpoint
	cfg, err := testConfig(cp.fn)
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}
	var rec connectortest.EmitRecorder
	_, err = cli.IncrementalSync(context.Background(), cfg, sdk.Cursor("stale"), rec.Emit)
	if !errors.Is(err, sdk.ErrCursorExpired) {
		t.Fatalf("IncrementalSync err = %v, want errors.Is ErrCursorExpired", err)
	}
	if n := len(rec.Docs()); n != 2 {
		t.Errorf("emitted %d docs before cursor-expired, want 2", n)
	}
}

// TestIncrementalSyncParity asserts a clean incremental pass matches in-proc.
func TestIncrementalSyncParity(t *testing.T) {
	t.Parallel()
	conn := &fakeConnector{incrementalDocs: 3, incrementalCursor: sdk.Cursor("inc-final")}
	cli := serveConn(t, conn)

	var cp recordingCheckpoint
	cfg, err := testConfig(cp.fn)
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}
	var rec connectortest.EmitRecorder
	cur, err := cli.IncrementalSync(context.Background(), cfg, sdk.Cursor("start"), rec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if cur != sdk.Cursor("inc-final") {
		t.Errorf("cursor = %q, want inc-final", cur)
	}
	if n := len(rec.Docs()); n != 3 {
		t.Errorf("emitted %d docs, want 3", n)
	}
	wantCheckpoints := []sdk.Cursor{"inc-cp-0", "inc-cp-1", "inc-cp-2"}
	assertCheckpointsEqual(t, cp.snapshot(), wantCheckpoints)
}

// TestWebhookParity asserts HandleWebhook round-trips the *http.Request
// (method/url/headers/body) and the emitted documents.
func TestWebhookParity(t *testing.T) {
	t.Parallel()
	conn := &fakeConnector{supportsWebhook: true, webhookDocs: 2}
	cli := serveConn(t, conn)

	cfg, err := testConfig(sdk.NopCheckpoint)
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}

	body := []byte(`{"event":"ping","n":7}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/fake?x=1&y=2", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add("X-Multi", "a")
	req.Header.Add("X-Multi", "b")

	var rec connectortest.EmitRecorder
	if err := cli.HandleWebhook(context.Background(), cfg, req, rec.Emit); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if n := len(rec.Docs()); n != 2 {
		t.Errorf("emitted %d docs, want 2", n)
	}

	got := conn.seenWebhook
	if got == nil {
		t.Fatal("server connector never saw a webhook request")
	}
	if got.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.Method)
	}
	if got.URL.String() != "/webhooks/fake?x=1&y=2" {
		t.Errorf("url = %q, want /webhooks/fake?x=1&y=2", got.URL.String())
	}
	if ct := got.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if multi := got.Header.Values("X-Multi"); len(multi) != 2 || multi[0] != "a" || multi[1] != "b" {
		t.Errorf("X-Multi = %v, want [a b]", multi)
	}
	gotBody := readAll(t, got.Body)
	if string(gotBody) != string(body) {
		t.Errorf("body = %q, want %q", gotBody, body)
	}
}

// TestWebhookUnsupportedRoundTrips asserts a connector returning
// sdk.ErrWebhookUnsupported surfaces as that sentinel on the client.
func TestWebhookUnsupportedRoundTrips(t *testing.T) {
	t.Parallel()
	conn := &fakeConnector{webhookErr: sdk.ErrWebhookUnsupported}
	cli := serveConn(t, conn)

	cfg, err := testConfig(sdk.NopCheckpoint)
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/hook", nil)

	var rec connectortest.EmitRecorder
	err = cli.HandleWebhook(context.Background(), cfg, req, rec.Emit)
	if !errors.Is(err, sdk.ErrWebhookUnsupported) {
		t.Fatalf("HandleWebhook err = %v, want errors.Is ErrWebhookUnsupported", err)
	}
}

// TestValidateErrorMapsBack asserts a Validate failure surfaces with the
// connector's message, and that a successful validation returns nil.
func TestValidateErrorMapsBack(t *testing.T) {
	t.Parallel()
	const msg = "fake: user_email is required"
	conn := &fakeConnector{validateErr: errors.New(msg)}
	cli := serveConn(t, conn)

	cfg, err := testConfig(sdk.NopCheckpoint)
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}
	if err := cli.Validate(context.Background(), cfg); err == nil || err.Error() != msg {
		t.Fatalf("Validate err = %v, want %q", err, msg)
	}

	ok := &fakeConnector{}
	okCli := serveConn(t, ok)
	if err := okCli.Validate(context.Background(), cfg); err != nil {
		t.Fatalf("Validate (ok) err = %v, want nil", err)
	}
}

// TestEmitErrorAbortsServerPass asserts that when a hub-side emit fails
// mid-stream, the server's connector observes ctx cancellation and aborts the
// pass (it does not run to completion emitting all docs).
func TestEmitErrorAbortsServerPass(t *testing.T) {
	t.Parallel()

	// sawCancel is closed when the connector observes ctx cancellation.
	sawCancel := make(chan struct{})
	emittedAll := make(chan struct{})
	conn := &ctxObservingConnector{
		total:       50,
		sawCancel:   sawCancel,
		emittedAll:  emittedAll,
		emitGapWait: 20 * time.Millisecond,
	}
	cli := serveConn(t, conn)

	cfg, err := testConfig(sdk.NopCheckpoint)
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}

	emitCount := 0
	failingEmit := func(_ context.Context, _ *askerv1.Document) error {
		emitCount++
		if emitCount == 2 {
			return errors.New("hub: emit failed")
		}
		return nil
	}

	_, err = cli.FullSync(context.Background(), cfg, failingEmit)
	if err == nil || err.Error() != "hub: emit failed" {
		t.Fatalf("FullSync err = %v, want hub: emit failed", err)
	}

	select {
	case <-sawCancel:
		// Good: the connector's emit saw ctx canceled and bailed.
	case <-emittedAll:
		t.Fatal("connector emitted all docs; the pass was not aborted by the failing emit")
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the server pass to observe cancellation")
	}
}

// TestInvalidTenantRejected asserts the server fails closed on a malformed
// tenant_id even though the client could not normally produce one (a hostile or
// buggy hub). The check exercises the server's Validate path directly.
func TestInvalidTenantRejected(t *testing.T) {
	t.Parallel()
	cfg, err := configFromProto(badTenantConfig(), sdk.NopCheckpoint)
	if err == nil {
		t.Fatalf("configFromProto accepted an invalid tenant: %+v", cfg)
	}
}

// assertDocsEqual asserts two document slices are equal by proto.Equal in order.
func assertDocsEqual(t *testing.T, got, want []*askerv1.Document) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d docs, want %d", len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("doc[%d] mismatch:\n got %v\nwant %v", i, got[i], want[i])
		}
	}
}

// assertCheckpointsEqual asserts two cursor slices match in order.
func assertCheckpointsEqual(t *testing.T, got, want []sdk.Cursor) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d checkpoints %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("checkpoint[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
