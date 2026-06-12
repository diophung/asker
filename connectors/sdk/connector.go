package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// Emit hands one canonical document to the connector hub. The hub stamps
// ts.ingested and routes the document to Kafka — connectors never touch Kafka
// themselves. Emitted documents must satisfy the contract in the package
// documentation: tenant_id matching cfg.Tenant, doc_id from DocID,
// version_etag set, and tombstones expressed as Documents with
// tombstone.deleted=true and no body. An Emit error is terminal for the
// current sync pass: return it (wrapped if useful) so the hub can retry from
// the last checkpoint.
type Emit func(ctx context.Context, doc *askerv1.Document) error

// Cursor is an opaque, connector-defined resumption point (e.g. a Gmail
// historyId or a page token). The hub persists cursors verbatim and replays
// them into IncrementalSync; only the connector that produced a cursor
// interprets it. The empty Cursor means "no position yet".
type Cursor string

// Checkpoint persists an intermediate cursor during a (possibly long)
// FullSync so an interrupted backfill resumes instead of restarting. The hub
// guarantees the Checkpoint in a Config it passes to a connector is never
// nil, so connectors may call cfg.Checkpoint without a nil check; tests
// constructing a Config by hand should use NopCheckpoint. A Checkpoint error
// is terminal for the current sync pass, like an Emit error.
type Checkpoint func(ctx context.Context, cur Cursor) error

// NopCheckpoint is a Checkpoint that discards the cursor. It exists so tests
// (and callers that do not need resumability) can build a Config whose
// Checkpoint field honors the never-nil guarantee.
func NopCheckpoint(_ context.Context, _ Cursor) error { return nil }

// Config is everything the hub supplies for one connector instance run.
type Config struct {
	// Tenant is the tenant this instance syncs for. It is always a valid
	// tenancy.Context (the hub derives it from a verified JWT or its own
	// instance records); every emitted document's tenant_id must equal
	// Tenant.TenantID().
	Tenant tenancy.Context

	// InstanceID identifies this configured instance of the connector (a
	// tenant may connect the same source twice, e.g. two Gmail accounts).
	InstanceID string

	// ConfigJSON is the instance configuration, valid against the JSONSchema
	// published in Spec.ConfigSchema.
	ConfigJSON []byte

	// Token is the credential material for the source (OAuth2 access token,
	// API key, ...), already decrypted by the hub's token vault. Empty for
	// AuthNone connectors. Connectors must not log or persist it.
	Token []byte

	// Checkpoint persists intermediate cursors during FullSync. The hub
	// guarantees it is never nil; tests should set NopCheckpoint.
	Checkpoint Checkpoint
}

// AuthType declares how a connector authenticates to its source. The hub
// uses it to drive the right credential flow before any sync runs.
type AuthType int

const (
	// AuthNone means the source needs no credentials (e.g. direct upload,
	// public iCal feeds).
	AuthNone AuthType = iota
	// AuthOAuth2 means the hub runs an OAuth2 authorization-code flow and
	// delivers a refreshed access token in Config.Token.
	AuthOAuth2
	// AuthToken means the user supplies a static secret (API key, PAT)
	// delivered in Config.Token.
	AuthToken
)

// String returns a stable lowercase name for logging and APIs.
func (a AuthType) String() string {
	switch a {
	case AuthNone:
		return "none"
	case AuthOAuth2:
		return "oauth2"
	case AuthToken:
		return "token"
	default:
		return fmt.Sprintf("authtype(%d)", int(a))
	}
}

// Spec describes a connector to the hub and the management UI.
type Spec struct {
	// ID is the stable connector identifier ("gmail", "upload", ...). It is
	// embedded in every doc_id via DocID, so it must never change once
	// documents exist. Lowercase [a-z0-9-]+ (connectortest.RunSpecChecks
	// enforces this); in particular it can never contain the ":" DocID
	// separator.
	ID string
	// DisplayName is the human-readable name shown in the UI.
	DisplayName string
	// AuthType declares the credential flow the hub must run.
	AuthType AuthType
	// ConfigSchema is a JSONSchema document describing valid ConfigJSON.
	// Connectors with no configuration should publish an empty object
	// schema such as {"type":"object"} rather than leaving this nil.
	ConfigSchema json.RawMessage
	// SupportsWebhook tells the hub whether HandleWebhook is implemented;
	// when false the hub schedules polling only and never routes requests
	// to HandleWebhook.
	SupportsWebhook bool
}

// ErrWebhookUnsupported is returned by HandleWebhook on connectors that have
// no push path. The hub treats it as "use polling", not as a failure.
var ErrWebhookUnsupported = errors.New("sdk: connector does not support webhooks")

// ErrCursorExpired is returned (possibly wrapped) by IncrementalSync when the
// source reports the cursor is no longer replayable (e.g. Gmail history.list
// 404 on a stale startHistoryId). The hub responds by restarting FullSync for
// the instance; connectors must NOT silently full-sync themselves.
var ErrCursorExpired = errors.New("sdk: sync cursor expired at the source")

// Connector is the interface every data-source connector implements. M1
// connectors run in-process; the M2 gRPC plugin transport adapts the same
// interface, so implementations must not assume shared memory with the hub
// beyond the arguments they are given.
//
// All methods must honor ctx cancellation: the hub cancels syncs on
// shutdown, tenant disconnect, and GDPR delete.
type Connector interface {
	// Spec returns the connector's static description. It must be cheap,
	// side-effect free, and identical on every call.
	Spec() Spec

	// Validate checks that cfg is usable — ConfigJSON parses against the
	// schema, the token works, the source is reachable — without emitting
	// any documents. The hub calls it when an instance is created or
	// reconfigured and surfaces the error to the user verbatim, so messages
	// must not leak credentials.
	Validate(ctx context.Context, cfg Config) error

	// FullSync performs the initial backfill, emitting every visible
	// document. Long backfills must call cfg.Checkpoint periodically so an
	// interrupted run resumes rather than restarts. It returns the Cursor
	// from which IncrementalSync continues (typically "now" at the source).
	FullSync(ctx context.Context, cfg Config, emit Emit) (Cursor, error)

	// IncrementalSync emits every document created, changed, or deleted
	// (as a tombstone) since cur, returning the advanced cursor. Returning
	// the same cursor with no emissions means "no changes". This is the
	// polling path that backs the 30-minute freshness SLA when webhooks are
	// unavailable.
	IncrementalSync(ctx context.Context, cfg Config, cur Cursor, emit Emit) (Cursor, error)

	// HandleWebhook is the push path: the hub routes a source-originated
	// HTTP request (already matched to this tenant+instance) here, and the
	// connector verifies it and emits the affected documents — fetching
	// from the source if the payload is only a notification. Connectors
	// without a push path return ErrWebhookUnsupported.
	HandleWebhook(ctx context.Context, cfg Config, r *http.Request, emit Emit) error
}
