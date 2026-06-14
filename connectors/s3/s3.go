// Package s3 implements the Asker connector for S3-compatible object stores
// (AWS S3, MinIO, Cloudflare R2, Backblaze B2, ...). It speaks the S3 REST API
// through the minio-go/v7 client, which addresses any S3-compatible endpoint
// uniformly, and maps every object to a canonical FILE Document.
//
// # Auth
//
// The source uses static key-pair credentials (sdk.AuthToken). The hub vault
// stores an S3 credential as the single string "<accessKeyID>:<secretAccessKey>"
// and delivers it, already decrypted, in sdk.Config.Token. The connector splits
// that pair to build a minio-go static-V4 credential; it never logs or persists
// either half.
//
// # Config
//
//	{
//	  "endpoint":  "s3.amazonaws.com" | "127.0.0.1:9000" | "http://host:port",
//	  "bucket":    "my-bucket",          // required
//	  "prefix":    "docs/",              // optional: list only this key prefix
//	  "region":    "us-east-1",          // optional: skips a bucket-location lookup
//	  "use_ssl":   true                  // optional: HTTPS to the endpoint
//	}
//
// endpoint is a host[:port] (the form minio-go wants) and use_ssl selects the
// scheme. For dev/CI the contract harness injects the replay server's full
// http://host:port URL into endpoint; the connector tolerates a scheme there,
// stripping it and inferring use_ssl from it, so a single injected field drives
// the whole client.
//
// # Cursor format
//
// S3 has no change feed, so the cursor is a small JSON blob the connector both
// produces and parses, carrying everything IncrementalSync needs to reconcile a
// re-list against the prior pass:
//
//		{"k":"<last object key listed>", "m":"<RFC3339 max LastModified seen>",
//		 "keys":["a","b",...]}
//
//	  - "k" is the lexicographically-last key listed in a (possibly interrupted)
//	    FullSync; a mid-backfill checkpoint replays it as ListObjects StartAfter so
//	    an interrupted backfill resumes instead of restarting (S3 lists keys
//	    sorted).
//	  - "m" is the maximum LastModified observed across the whole keyset. The next
//	    IncrementalSync re-lists and re-emits any object whose LastModified is
//	    strictly newer than "m" (a create or overwrite).
//	  - "keys" is the compact sorted set of keys present at the end of the pass.
//	    The next IncrementalSync diffs it against the freshly-listed keyset and
//	    emits a tombstone for every key that has since vanished.
//
// This bounded-keyset approach detects deletions on every incremental re-list
// (not only on a periodic full re-list) at the cost of carrying the keyset in
// the cursor; it is the honest M2 trade-off for a source with no delete feed.
package s3

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/platform/safehttp"
)

const (
	// connectorID is the stable connector identifier baked into every doc_id
	// via sdk.DocID. It must never change once documents exist.
	connectorID = "s3"

	// bodyCap bounds how many bytes of an object body the connector pulls into
	// body_text. Larger or binary objects are left body-empty for M3 extraction.
	bodyCap = 1 << 20 // 1 MiB

	// listPageSize is the ListObjects batch size; minio-go pages transparently.
	listPageSize = 1000

	// clientTimeout bounds a single object body download.
	clientTimeout = 30 * time.Second
)

// configSchema is the JSONSchema for instance configuration.
const configSchema = `{
  "type": "object",
  "properties": {
    "endpoint": {"type": "string"},
    "bucket":   {"type": "string"},
    "prefix":   {"type": "string"},
    "region":   {"type": "string"},
    "use_ssl":  {"type": "boolean"}
  },
  "required": ["endpoint", "bucket"]
}`

// Connector implements sdk.Connector for S3-compatible object stores.
type Connector struct {
	log *slog.Logger
	now func() time.Time
}

// Option customizes a Connector (logger / clock injection for tests).
type Option func(*Connector)

// WithLogger sets the structured logger (default slog.Default()). It matches
// the wave-1 connectors' shape so the hub registry wires every connector
// uniformly.
func WithLogger(l *slog.Logger) Option {
	return func(c *Connector) {
		if l != nil {
			c.log = l
		}
	}
}

// withClock overrides the deletion-time clock (tests only).
func withClock(now func() time.Time) Option {
	return func(c *Connector) {
		if now != nil {
			c.now = now
		}
	}
}

// New returns a ready-to-register S3 connector as the sdk.Connector interface,
// so the hub registry can hold it directly.
func New(opts ...Option) sdk.Connector {
	c := &Connector{log: slog.Default(), now: time.Now}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Spec implements sdk.Connector. SupportsWebhook is false: S3 event
// notifications need source-side wiring (SQS/SNS/Lambda) and a publicly
// reachable endpoint, which is hub infrastructure deferred past M2 — the hub
// polls via IncrementalSync.
func (c *Connector) Spec() sdk.Spec {
	return sdk.Spec{
		ID:              connectorID,
		DisplayName:     "S3-Compatible Storage",
		AuthType:        sdk.AuthToken,
		ConfigSchema:    json.RawMessage(configSchema),
		SupportsWebhook: false,
	}
}

// instanceConfig is the parsed ConfigJSON.
type instanceConfig struct {
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
	Region   string `json:"region"`
	UseSSL   bool   `json:"use_ssl"`
}

// parseConfig decodes and sanity-checks ConfigJSON. endpoint and bucket are
// required. endpoint may carry an http(s):// scheme (the dev/CI base_url
// override the contract harness injects); when it does, the scheme is stripped
// to the host[:port] minio-go wants and use_ssl is inferred from it.
func parseConfig(raw []byte) (instanceConfig, error) {
	var conf instanceConfig
	if len(raw) == 0 {
		return conf, fmt.Errorf("s3: config is required (endpoint and bucket)")
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return conf, fmt.Errorf("s3: config is not valid JSON: %w", err)
	}
	conf.Endpoint = strings.TrimSpace(conf.Endpoint)
	conf.Bucket = strings.TrimSpace(conf.Bucket)
	conf.Prefix = strings.TrimSpace(conf.Prefix)
	conf.Region = strings.TrimSpace(conf.Region)

	if conf.Endpoint == "" {
		return conf, fmt.Errorf("s3: config field endpoint is required")
	}
	if conf.Bucket == "" {
		return conf, fmt.Errorf("s3: config field bucket is required")
	}

	// Tolerate a scheme on endpoint (the injected base_url override). Strip it
	// to host[:port] and let it dictate use_ssl.
	if strings.Contains(conf.Endpoint, "://") {
		u, err := url.Parse(conf.Endpoint)
		if err != nil || u.Host == "" {
			return conf, fmt.Errorf("s3: config field endpoint %q is not a valid host or URL", conf.Endpoint)
		}
		conf.Endpoint = u.Host
		conf.UseSSL = u.Scheme == "https"
	}
	return conf, nil
}

// splitToken splits the "<accessKeyID>:<secretAccessKey>" credential the hub
// vault delivers in cfg.Token. The error never echoes the token.
func splitToken(token []byte) (accessKey, secretKey string, err error) {
	id, secret, ok := strings.Cut(string(token), ":")
	if !ok || id == "" || secret == "" {
		return "", "", fmt.Errorf("s3: credential must be %q", "<accessKeyID>:<secretAccessKey>")
	}
	return id, secret, nil
}

// newClient builds the S3 client for conf using the split key pair. Setting
// Region avoids a bucket-location round-trip (minio-go skips GetBucketLocation
// when a region is configured), which keeps cassettes to exactly the calls the
// connector intends.
func (c *Connector) newClient(conf instanceConfig, token []byte) (*client, error) {
	accessKey, secretKey, err := splitToken(token)
	if err != nil {
		return nil, err
	}
	region := conf.Region
	if region == "" {
		// A non-empty region short-circuits minio-go's location probe; default
		// to the canonical S3 region so a replay/fake endpoint needs no extra
		// location interaction. Real callers should set their bucket's region.
		region = "us-east-1"
	}
	// endpoint is tenant-supplied and dialed server-side; route minio-go through
	// the SSRF-guarded transport so a hostile endpoint cannot reach loopback,
	// the cloud metadata IP, RFC1918, or cluster-internal services. The guard
	// runs at connect time (defeating DNS rebinding); dev/CI loopback is allowed
	// via the safehttp env/test seam.
	guarded, err := safehttp.NewTransport()
	if err != nil {
		return nil, fmt.Errorf("s3: build guarded transport: %w", err)
	}
	mc, err := minio.New(conf.Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:    conf.UseSSL,
		Region:    region,
		Transport: guarded,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: build client for endpoint: %w", err)
	}
	return &client{mc: mc, bucket: conf.Bucket, prefix: conf.Prefix}, nil
}

// Validate implements sdk.Connector: the config must parse, the credential must
// have the expected shape, and — when a token is present — a cheap one-object
// ListObjects must reach the bucket. Error text surfaces to users verbatim and
// never contains the credential.
func (c *Connector) Validate(ctx context.Context, cfg sdk.Config) error {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return err
	}
	if len(cfg.Token) == 0 {
		// No credential yet (instance created before its secret is supplied):
		// config-only validation.
		return nil
	}
	cl, err := c.newClient(conf, cfg.Token)
	if err != nil {
		return err
	}
	if err := cl.probe(ctx); err != nil {
		return fmt.Errorf("s3: credential or bucket check failed: %w", err)
	}
	return nil
}

// HandleWebhook implements sdk.Connector. S3 push (event notifications) is not
// wired in M2 (Spec.SupportsWebhook is false), so the hub never routes here;
// return the sentinel so a misconfiguration falls back to polling.
func (c *Connector) HandleWebhook(_ context.Context, _ sdk.Config, _ *http.Request, _ sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}
