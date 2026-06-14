package plugin

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/asker/asker/connectors/sdk"
	pluginv1 "github.com/asker/asker/platform/proto/gen/go/asker/plugin/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// cursorExpiredSentinel is the status-detail message a server attaches to a
// codes.FailedPrecondition status when a sync RPC's underlying connector
// returned sdk.ErrCursorExpired. The client recognizes it and reconstructs the
// sentinel so errors.Is(err, sdk.ErrCursorExpired) holds across the transport.
// It is a fixed, credential-free marker — never derived from connector output.
const cursorExpiredSentinel = "asker.plugin.v1: sync cursor expired at the source"

// webhookUnsupportedSentinel is the status message a server attaches to a
// codes.Unimplemented status when HandleWebhook returned
// sdk.ErrWebhookUnsupported, so the client can reconstruct that sentinel.
const webhookUnsupportedSentinel = "asker.plugin.v1: connector does not support webhooks"

// server adapts an in-process sdk.Connector to the generated
// ConnectorPluginService. It is the out-of-process half of ADR-010: a
// third-party connector author wraps their sdk.Connector with NewServer (or
// ListenAndServe) and ships a binary the hub dials.
type server struct {
	pluginv1.UnimplementedConnectorPluginServiceServer
	conn sdk.Connector
}

// NewServer returns a ConnectorPluginService that serves c. Register it on a
// grpc.Server with pluginv1.RegisterConnectorPluginServiceServer, or use
// ListenAndServe for the common single-connector binary.
func NewServer(c sdk.Connector) pluginv1.ConnectorPluginServiceServer {
	return &server{conn: c}
}

// ListenAndServe registers a ConnectorPluginService for c on a new grpc.Server,
// serves it on addr (e.g. "127.0.0.1:0" or a unix socket via "unix:///..."),
// and GracefulStops when ctx is canceled. Third-party plugin binaries call this
// from main; the hub dials the resulting address with Dial.
func ListenAndServe(ctx context.Context, addr string, c sdk.Connector) error {
	network, address := "tcp", addr
	if rest, ok := stripUnixScheme(addr); ok {
		network, address = "unix", rest
	}
	lis, err := net.Listen(network, address)
	if err != nil {
		return err
	}
	return serveOn(ctx, lis, c)
}

// serveOn is the body of ListenAndServe once a listener exists. It is separated
// so tests can drive a listener whose Accept fails (forcing gs.Serve to return
// on its own with ctx still live) without racing on real network conditions.
func serveOn(ctx context.Context, lis net.Listener, c sdk.Connector) error {
	gs := grpc.NewServer()
	pluginv1.RegisterConnectorPluginServiceServer(gs, NewServer(c))

	// stopCtx lets the stop goroutine unblock on EITHER ctx cancellation (the
	// caller shutting us down) OR Serve returning on its own (a listener/serve
	// error). Without the defer cancel(), a Serve error with ctx still live
	// would leave the goroutine parked on <-ctx.Done() forever and the <-done
	// wait would deadlock. Canceling once Serve returns guarantees the
	// goroutine always unblocks; GracefulStop is a no-op if Serve already
	// exited.
	stopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		<-stopCtx.Done()
		gs.GracefulStop()
	}()

	serveErr := gs.Serve(lis)
	cancel()
	<-done
	// A graceful stop returns grpc.ErrServerStopped (or nil); surface caller
	// ctx cancellation as a clean shutdown rather than an error. We check the
	// caller's ctx (not stopCtx, which we just canceled) so a genuine Serve
	// failure with the caller ctx still live is reported, not swallowed.
	if serveErr != nil && ctx.Err() != nil {
		return nil
	}
	return serveErr
}

// stripUnixScheme reports whether addr is a "unix://" or "unix:" target and
// returns the bare socket path, so ListenAndServe can listen on a unix socket
// the way grpc.NewClient dials one.
func stripUnixScheme(addr string) (string, bool) {
	switch {
	case len(addr) > 7 && addr[:7] == "unix://":
		return addr[7:], true
	case len(addr) > 5 && addr[:5] == "unix:":
		return addr[5:], true
	default:
		return "", false
	}
}

// Spec maps sdk.Spec to a SpecResponse, including the AuthType enum.
func (s *server) Spec(context.Context, *pluginv1.SpecRequest) (*pluginv1.SpecResponse, error) {
	spec := s.conn.Spec()
	return &pluginv1.SpecResponse{
		Id:              spec.ID,
		DisplayName:     spec.DisplayName,
		AuthType:        authTypeToProto(spec.AuthType),
		ConfigSchema:    []byte(spec.ConfigSchema),
		SupportsWebhook: spec.SupportsWebhook,
	}, nil
}

// Validate rebuilds an sdk.Config from the request and runs the connector's
// validation. The tenant is re-derived from the wire tenant_id and fails closed
// when it is missing or malformed (never trusted as user input). Validation
// failures map to codes.InvalidArgument; anything else to codes.Internal. The
// connector's message is surfaced verbatim and must be credential-free per the
// sdk contract.
func (s *server) Validate(ctx context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateResponse, error) {
	// Validate emits nothing, so the never-nil Checkpoint guarantee is met
	// with a no-op.
	cfg, err := configFromProto(req.GetConfig(), sdk.NopCheckpoint)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.conn.Validate(ctx, cfg); err != nil {
		// A connector's Validate error is a config/credential problem the hub
		// surfaces to the user — InvalidArgument carries that intent.
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pluginv1.ValidateResponse{}, nil
}

// FullSync runs the connector's backfill, streaming each emitted Document as a
// SyncEvent{document} and each checkpoint as SyncEvent{checkpoint}, terminating
// with SyncEvent{done{cursor}}.
func (s *server) FullSync(req *pluginv1.FullSyncRequest, stream grpc.ServerStreamingServer[pluginv1.SyncEvent]) error {
	cfg, emit, err := newSyncSession(stream, req.GetConfig())
	if err != nil {
		return err
	}
	cur, err := s.conn.FullSync(stream.Context(), cfg, emit)
	return finishSync(stream, cur, err)
}

// IncrementalSync mirrors FullSync for the poll path. A returned
// sdk.ErrCursorExpired is mapped to a codes.FailedPrecondition status carrying
// the cursor-expired sentinel so the client can reconstruct the error.
func (s *server) IncrementalSync(req *pluginv1.IncrementalSyncRequest, stream grpc.ServerStreamingServer[pluginv1.SyncEvent]) error {
	cfg, emit, err := newSyncSession(stream, req.GetConfig())
	if err != nil {
		return err
	}
	cur, err := s.conn.IncrementalSync(stream.Context(), cfg, sdk.Cursor(req.GetCursor()), emit)
	return finishSync(stream, cur, err)
}

// HandleWebhook rebuilds an *http.Request from the wire HttpRequest, runs the
// connector's push path, and streams any emitted documents. An empty terminal
// Done is sent for stream symmetry (webhooks carry no cursor).
// sdk.ErrWebhookUnsupported maps to codes.Unimplemented.
func (s *server) HandleWebhook(req *pluginv1.HandleWebhookRequest, stream grpc.ServerStreamingServer[pluginv1.SyncEvent]) error {
	cfg, emit, err := newSyncSession(stream, req.GetConfig())
	if err != nil {
		return err
	}
	httpReq, err := httpRequestFromProto(stream.Context(), req.GetRequest())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.conn.HandleWebhook(stream.Context(), cfg, httpReq, emit); err != nil {
		if errors.Is(err, sdk.ErrWebhookUnsupported) {
			return status.Error(codes.Unimplemented, webhookUnsupportedSentinel)
		}
		return mapSyncError(stream, err)
	}
	if err := stream.Send(&pluginv1.SyncEvent{
		Event: &pluginv1.SyncEvent_Done{Done: &pluginv1.SyncDone{}},
	}); err != nil {
		return err
	}
	return nil
}

// newSyncSession rebuilds the per-pass sdk.Config and the Emit closure shared by
// the three streaming RPCs. Emit sends SyncEvent{document} and Checkpoint sends
// SyncEvent{checkpoint}; a Send failure (the hub canceled the stream because a
// hub-side callback failed) is returned so the connector aborts the pass —
// preserving the in-process rule that an Emit/Checkpoint error is terminal.
func newSyncSession(stream grpc.ServerStreamingServer[pluginv1.SyncEvent], pc *pluginv1.Config) (sdk.Config, sdk.Emit, error) {
	checkpoint := func(ctx context.Context, cur sdk.Cursor) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return stream.Send(&pluginv1.SyncEvent{
			Event: &pluginv1.SyncEvent_Checkpoint{Checkpoint: string(cur)},
		})
	}
	cfg, err := configFromProto(pc, checkpoint)
	if err != nil {
		return sdk.Config{}, nil, status.Error(codes.InvalidArgument, err.Error())
	}
	emit := func(ctx context.Context, doc *askerv1.Document) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return stream.Send(&pluginv1.SyncEvent{
			Event: &pluginv1.SyncEvent_Document{Document: doc},
		})
	}
	return cfg, emit, nil
}

// finishSync terminates a sync stream: it maps a connector error to a status (or
// reconstructible sentinel) and otherwise sends the terminal Done{cursor}.
func finishSync(stream grpc.ServerStreamingServer[pluginv1.SyncEvent], cur sdk.Cursor, err error) error {
	if err != nil {
		return mapSyncError(stream, err)
	}
	return stream.Send(&pluginv1.SyncEvent{
		Event: &pluginv1.SyncEvent_Done{Done: &pluginv1.SyncDone{Cursor: string(cur)}},
	})
}

// mapSyncError turns a connector error into the gRPC status the stream ends
// with. A canceled stream context (the hub aborted because an Emit/Checkpoint
// callback failed) is propagated as codes.Canceled. sdk.ErrCursorExpired maps to
// codes.FailedPrecondition + sentinel; everything else to codes.Internal with a
// credential-free message.
func mapSyncError(stream grpc.ServerStreamingServer[pluginv1.SyncEvent], err error) error {
	if ctxErr := stream.Context().Err(); ctxErr != nil {
		// The hub canceled the stream; the connector observed it as a Send
		// failure and returned. Report cancellation, not Internal.
		return status.FromContextError(ctxErr).Err()
	}
	if errors.Is(err, sdk.ErrCursorExpired) {
		return status.Error(codes.FailedPrecondition, cursorExpiredSentinel)
	}
	return status.Error(codes.Internal, err.Error())
}

// configFromProto rebuilds an sdk.Config from the wire form. The tenant is
// re-derived from tenant_id via tenancy.FromHeaderValue and fails closed on an
// invalid value — the plugin transport never trusts a tenant string outside a
// validated tenancy.Context. checkpoint supplies the never-nil Checkpoint.
func configFromProto(pc *pluginv1.Config, checkpoint sdk.Checkpoint) (sdk.Config, error) {
	if pc == nil {
		return sdk.Config{}, errors.New("plugin: missing config")
	}
	tc, err := tenancy.FromHeaderValue(pc.GetTenantId())
	if err != nil {
		return sdk.Config{}, err
	}
	return sdk.Config{
		Tenant:     tc,
		InstanceID: pc.GetInstanceId(),
		ConfigJSON: pc.GetConfigJson(),
		Token:      pc.GetToken(),
		Checkpoint: checkpoint,
	}, nil
}

// httpRequestFromProto rebuilds an *http.Request from the wire HttpRequest. The
// body is carried as a bytes.Reader so the connector can read it like a real
// delivery; headers are copied verbatim. The request is bound to ctx so the
// connector's outbound fetches honor stream cancellation.
func httpRequestFromProto(ctx context.Context, pr *pluginv1.HttpRequest) (*http.Request, error) {
	if pr == nil {
		return nil, errors.New("plugin: missing webhook request")
	}
	method := pr.GetMethod()
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, pr.GetUrl(), bytes.NewReader(pr.GetBody()))
	if err != nil {
		return nil, err
	}
	for key, vals := range pr.GetHeaders() {
		for _, v := range vals.GetValues() {
			req.Header.Add(key, v)
		}
	}
	return req, nil
}
