package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/asker/asker/connectors/sdk"
	pluginv1 "github.com/asker/asker/platform/proto/gen/go/asker/plugin/v1"
	"github.com/asker/asker/platform/tenancy"
)

// maxWebhookBody bounds the request body the client serializes into an
// HttpRequest before sending it to the plugin. Webhook deliveries are small;
// this guards a hostile or buggy caller from streaming an unbounded body.
const maxWebhookBody = 1 << 20

// Client adapts a remote ConnectorPluginService back to the in-process
// sdk.Connector interface, so the hub treats an out-of-process plugin exactly
// like a built-in connector (ADR-010). It caches the plugin's Spec at dial time
// (Spec must be cheap and stable) so Spec() needs no live call.
type Client struct {
	rpc  pluginv1.ConnectorPluginServiceClient
	spec sdk.Spec
	// closer closes the owned ClientConn when Dial created it; nil when the
	// caller supplied the connection via NewClient.
	closer io.Closer
}

var _ sdk.Connector = (*Client)(nil)

// Dial connects to a plugin listening at target (a gRPC dial target such as
// "127.0.0.1:8090" or "unix:///run/asker/notes.sock"), fetches and caches its
// Spec, and returns a Client. The caller must Close the result. Callers that
// want an insecure transport must pass grpc.WithTransportCredentials
// explicitly; no default credentials are assumed.
//
// If the plugin cannot be reached for the initial Spec call, Dial closes the
// connection and returns the error.
func Dial(ctx context.Context, target string, opts ...grpc.DialOption) (*Client, error) {
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("plugin: dial %q: %w", target, err)
	}
	c, err := newClientWithSpec(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	c.closer = conn
	return c, nil
}

// NewClient adapts an already-established connection (e.g. one a test created
// over a 127.0.0.1 listener) into a Client whose Close is a no-op — the caller
// owns conn. It does NOT fetch the Spec, so Spec() returns the zero Spec until
// one is populated; callers that need a cached Spec should use Dial (or
// DialContext-style helpers) instead. NewClient exists chiefly for tests and
// callers that manage the ClientConn lifecycle themselves.
func NewClient(conn grpc.ClientConnInterface) *Client {
	return &Client{rpc: pluginv1.NewConnectorPluginServiceClient(conn)}
}

// newClientWithSpec builds a Client over conn and eagerly fetches the Spec, so
// Spec() is a pure accessor afterward.
func newClientWithSpec(ctx context.Context, conn grpc.ClientConnInterface) (*Client, error) {
	c := NewClient(conn)
	resp, err := c.rpc.Spec(ctx, &pluginv1.SpecRequest{})
	if err != nil {
		return nil, fmt.Errorf("plugin: fetch spec: %w", err)
	}
	c.spec = sdk.Spec{
		ID:              resp.GetId(),
		DisplayName:     resp.GetDisplayName(),
		AuthType:        authTypeFromProto(resp.GetAuthType()),
		ConfigSchema:    resp.GetConfigSchema(),
		SupportsWebhook: resp.GetSupportsWebhook(),
	}
	return c, nil
}

// Close releases the owned connection when Dial created it.
func (c *Client) Close() error {
	if c.closer == nil {
		return nil
	}
	return c.closer.Close()
}

// Spec returns the cached Spec captured at Dial time.
func (c *Client) Spec() sdk.Spec { return c.spec }

// Validate runs the remote connector's validation. A failed validation surfaces
// as a status the hub shows the user; the message is reconstructed verbatim.
func (c *Client) Validate(ctx context.Context, cfg sdk.Config) error {
	_, err := c.rpc.Validate(ctx, &pluginv1.ValidateRequest{Config: configToProto(cfg)})
	if err != nil {
		return validateError(err)
	}
	return nil
}

// validateError unwraps a Validate status into a plain error carrying the
// connector's user-facing message, so the hub surfaces it verbatim.
func validateError(err error) error {
	if st, ok := status.FromError(err); ok {
		return errors.New(st.Message())
	}
	return err
}

// FullSync opens the server stream and routes its events: a document is emitted,
// a checkpoint is persisted via cfg.Checkpoint, and Done returns the final
// cursor. An emit/checkpoint error cancels the stream so the server's Send
// fails, aborting the plugin pass — preserving the terminal-error contract.
func (c *Client) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.rpc.FullSync(streamCtx, &pluginv1.FullSyncRequest{Config: configToProto(cfg)})
	if err != nil {
		return "", err
	}
	return drainSync(streamCtx, cancel, stream, cfg, emit)
}

// IncrementalSync mirrors FullSync for the poll path and reconstructs
// sdk.ErrCursorExpired from the server's FailedPrecondition sentinel.
func (c *Client) IncrementalSync(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.rpc.IncrementalSync(streamCtx, &pluginv1.IncrementalSyncRequest{
		Config: configToProto(cfg),
		Cursor: string(cur),
	})
	if err != nil {
		return "", err
	}
	return drainSync(streamCtx, cancel, stream, cfg, emit)
}

// HandleWebhook serializes r into an HttpRequest, opens the stream, and routes
// emitted documents. An Unimplemented status maps to sdk.ErrWebhookUnsupported
// so the hub falls back to polling.
func (c *Client) HandleWebhook(ctx context.Context, cfg sdk.Config, r *http.Request, emit sdk.Emit) error {
	pr, err := httpRequestToProto(r)
	if err != nil {
		return err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.rpc.HandleWebhook(streamCtx, &pluginv1.HandleWebhookRequest{
		Config:  configToProto(cfg),
		Request: pr,
	})
	if err != nil {
		return webhookError(err)
	}
	_, err = drainSync(streamCtx, cancel, stream, cfg, emit)
	return webhookError(err)
}

// syncStream is the subset of the generated server-streaming client the drain
// loop needs; it lets tests exercise drainSync without a live stream.
type syncStream interface {
	Recv() (*pluginv1.SyncEvent, error)
}

// drainSync consumes a sync stream until Done or an error. On the first
// emit/checkpoint error it calls cancel to tear down the stream context, which
// surfaces to the server as a Send failure — aborting the plugin pass and
// preserving the in-process rule that an Emit/Checkpoint error is terminal. ctx
// is the (already cancelable) stream context handed to emit/Checkpoint.
func drainSync(ctx context.Context, cancel context.CancelFunc, stream syncStream, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	for {
		ev, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Stream closed without a Done event: treat as an empty,
				// cursor-less completion rather than a hang. The server always
				// sends Done on success, so this is defensive.
				return "", nil
			}
			return "", syncError(err)
		}
		switch e := ev.GetEvent().(type) {
		case *pluginv1.SyncEvent_Document:
			if emitErr := emit(ctx, e.Document); emitErr != nil {
				cancel()
				return "", emitErr
			}
		case *pluginv1.SyncEvent_Checkpoint:
			if cpErr := cfg.Checkpoint(ctx, sdk.Cursor(e.Checkpoint)); cpErr != nil {
				cancel()
				return "", cpErr
			}
		case *pluginv1.SyncEvent_Done:
			return sdk.Cursor(e.Done.GetCursor()), nil
		default:
			return "", fmt.Errorf("plugin: unexpected sync event %T", ev.GetEvent())
		}
	}
}

// syncError reconstructs a transport sentinel from a stream error. A
// FailedPrecondition carrying the cursor-expired sentinel becomes a wrapped
// sdk.ErrCursorExpired so errors.Is(err, sdk.ErrCursorExpired) holds.
func syncError(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	if st.Code() == codes.FailedPrecondition && st.Message() == cursorExpiredSentinel {
		return fmt.Errorf("plugin: %s: %w", st.Message(), sdk.ErrCursorExpired)
	}
	return err
}

// webhookError reconstructs sdk.ErrWebhookUnsupported from an Unimplemented
// status; other errors pass through.
func webhookError(err error) error {
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
		return sdk.ErrWebhookUnsupported
	}
	return err
}

// configToProto serializes the wire-relevant parts of an sdk.Config. The
// Checkpoint callback is not on the wire (the stream carries checkpoints) and
// the tenant travels as its header value, re-validated server-side.
func configToProto(cfg sdk.Config) *pluginv1.Config {
	return &pluginv1.Config{
		TenantId:   tenancy.HeaderValue(cfg.Tenant),
		InstanceId: cfg.InstanceID,
		ConfigJson: cfg.ConfigJSON,
		Token:      cfg.Token,
	}
}

// httpRequestToProto serializes r for the wire: method, url (path+query as the
// plugin expects), headers, and a fully-buffered body. The caller's r is left
// usable — its Body is replaced with a fresh reader over the buffered bytes.
func httpRequestToProto(r *http.Request) (*pluginv1.HttpRequest, error) {
	if r == nil {
		return nil, errors.New("plugin: nil webhook request")
	}
	var body []byte
	if r.Body != nil {
		buf, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
		_ = r.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("plugin: read webhook body: %w", err)
		}
		body = buf
		// Re-buffer so the caller can still read r after serialization.
		r.Body = io.NopCloser(bytes.NewReader(buf))
	}
	headers := make(map[string]*pluginv1.HeaderValues, len(r.Header))
	for key, vals := range r.Header {
		vs := make([]string, len(vals))
		copy(vs, vals)
		headers[key] = &pluginv1.HeaderValues{Values: vs}
	}
	return &pluginv1.HttpRequest{
		Method:  r.Method,
		Url:     requestURL(r),
		Headers: headers,
		Body:    body,
	}, nil
}

// requestURL returns the path+query the plugin should see. When r.URL is
// populated it is used verbatim; otherwise the empty string.
func requestURL(r *http.Request) string {
	if r.URL == nil {
		return ""
	}
	return r.URL.String()
}
