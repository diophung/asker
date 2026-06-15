package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/asker/asker/platform/blob"
	"github.com/asker/asker/platform/crypto"
	"github.com/asker/asker/platform/tenancy"
)

// The GDPR delete cascade fans erasure out to four external stores in addition
// to Postgres (handled by Store.PurgeTenant) and the DEK crypto-shred (handled
// by crypto.DEKStore.Delete + TenantCipher.Forget). These small seams keep each
// store's purge testable in isolation against a fake, and let main.go wire the
// real clients. Each is tenant-scoped from the verified caller context — never
// a request field — and fails closed on a blank tenant.

// vespaPurger deletes a tenant's entire Vespa streaming group (the group name
// IS the tenant id; see services/index-writer/writer.go documentURL and
// vespa/services.xml: streaming content cluster "asker", document type "doc").
type vespaPurger interface {
	PurgeGroup(ctx context.Context, tenantID tenancy.TenantID) error
}

// blobPurger deletes a tenant's blob prefix (originals + thumbnails/keyframes).
type blobPurger interface {
	DeletePrefix(ctx context.Context, tc tenancy.Context) (int, error)
}

// cachePurger best-effort removes a tenant's Redis keys (query result cache +
// rate-limit counters). A cache purge failure never blocks the erasure (the
// keys are derived, TTL'd, and carry no authoritative data) but is reported.
type cachePurger interface {
	PurgeTenant(ctx context.Context, tenantID tenancy.TenantID) error
}

var (
	_ blobPurger = (*blob.Store)(nil)
)

// cascadePurgers bundles the external-store purgers for the delete cascade.
type cascadePurgers struct {
	vespa vespaPurger
	blobs blobPurger
	cache cachePurger
}

// buildPurgers wires the GDPR delete-cascade purgers from config. The blob
// store reuses the SAME TenantCipher as the token vault (so the prefix delete
// runs under the same crypto wiring). The Redis purger is returned separately
// so main can Close it on shutdown. A purger whose backing store is misconfigured
// is left nil rather than failing startup — the cascade purges Postgres + the
// DEK regardless, and a nil purger is a deliberate "skip this store".
func buildPurgers(ctx context.Context, cfg controlPlaneConfig, cipher *crypto.TenantCipher, logger *slog.Logger) (cascadePurgers, *redisCachePurger, error) {
	var p cascadePurgers

	if cfg.VespaURL != "" {
		p.vespa = newHTTPVespaPurger(cfg.VespaURL, cfg.VespaCluster)
	}

	if cfg.MinIOEndpoint != "" && cfg.MinIOAccessKey != "" && cfg.MinIOSecretKey != "" {
		blobs, err := blob.New(ctx, blob.Config{
			Endpoint:  cfg.MinIOEndpoint,
			AccessKey: cfg.MinIOAccessKey,
			SecretKey: cfg.MinIOSecretKey,
			Bucket:    cfg.MinIOBucket,
			UseSSL:    cfg.MinIOUseSSL,
		}, cipher)
		if err != nil {
			// Blob is on the GDPR critical path; refuse to start half-wired.
			return cascadePurgers{}, nil, fmt.Errorf("blob purger: %w", err)
		}
		p.blobs = blobs
	} else {
		logger.Warn("GDPR delete: MinIO not configured — blob prefix purge disabled")
	}

	var cache *redisCachePurger
	if cfg.RedisAddr != "" {
		cache = newRedisCachePurger(cfg.RedisAddr)
		p.cache = cache
	}

	return p, cache, nil
}

// httpVespaPurger issues the Vespa document/v1 group delete. Vespa's
// streaming-mode group delete is:
//
//	DELETE {VESPA}/document/v1/asker/doc/group/<tenant>?selection=true&cluster=asker
//
// which removes every document in the tenant's group in one server-side
// operation. The tenant id is path-escaped (it is already restricted to a safe
// alphabet by platform/tenancy, but escaped like any untrusted segment). A 200
// with a continuation token means more remains; we follow continuations until
// the group is empty so the delete is complete, not partial.
type httpVespaPurger struct {
	baseURL string // no trailing slash
	cluster string
	client  *http.Client
}

func newHTTPVespaPurger(vespaURL, cluster string) *httpVespaPurger {
	return &httpVespaPurger{
		baseURL: strings.TrimRight(vespaURL, "/"),
		cluster: cluster,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *httpVespaPurger) PurgeGroup(ctx context.Context, tenantID tenancy.TenantID) error {
	if tenantID == "" {
		return tenancy.ErrNoTenant
	}
	base := p.baseURL + "/document/v1/asker/doc/group/" + url.PathEscape(string(tenantID))
	// Follow document/v1 continuation tokens so a large group is fully purged.
	var continuation string
	for i := 0; i < 1000; i++ { // bound: each round deletes a chunk; 1000 rounds is a huge corpus
		if err := ctx.Err(); err != nil {
			return err
		}
		q := url.Values{}
		q.Set("selection", "true")
		if p.cluster != "" {
			q.Set("cluster", p.cluster)
		}
		if continuation != "" {
			q.Set("continuation", continuation)
		}
		reqURL := base + "?" + q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
		if err != nil {
			return fmt.Errorf("vespa purge: build request: %w", err)
		}
		next, err := p.doDelete(req)
		if err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		continuation = next
	}
	return fmt.Errorf("vespa purge: group %q did not drain within the continuation bound", tenantID)
}

// vespaDeleteResponse is the slice of the document/v1 visit/delete response we
// consume: a continuation token when more documents remain.
type vespaDeleteResponse struct {
	Continuation string `json:"continuation"`
	Message      string `json:"message"`
}

func (p *httpVespaPurger) doDelete(req *http.Request) (continuation string, err error) {
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("vespa purge: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("vespa purge: status %d: %s", resp.StatusCode, truncate(body, 512))
	}
	var vr vespaDeleteResponse
	// A non-JSON 2xx body is fine (treated as a completed delete with no
	// continuation); only a JSON continuation token drives another round.
	_ = json.Unmarshal(body, &vr)
	return vr.Continuation, nil
}

// redisCachePurger removes a tenant's Redis keys by scanning the known
// key-prefix patterns the query cache and rate limiter use. SCAN (not KEYS)
// keeps it non-blocking on the Redis side.
type redisCachePurger struct {
	rdb *redis.Client
}

func newRedisCachePurger(addr string) *redisCachePurger {
	return &redisCachePurger{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

func (p *redisCachePurger) Close() error { return p.rdb.Close() }

func (p *redisCachePurger) PurgeTenant(ctx context.Context, tenantID tenancy.TenantID) error {
	if tenantID == "" {
		return tenancy.ErrNoTenant
	}
	// The query result-cache keys are "q:<tenant>:..." and the gateway
	// rate-limit keys are "rl:<tenant>:..." (see services/query/cache.go
	// cacheKey and services/gateway/ratelimit.go). The tenant alphabet
	// ([A-Za-z0-9._-]) excludes Redis glob metacharacters (*?[]\), so the match
	// pattern cannot widen to another tenant.
	patterns := []string{
		"q:" + string(tenantID) + ":*",
		"rl:" + string(tenantID) + ":*",
		// Personalization read-cache + recent-search history (v3.2): the resolved
		// profile, the learned model, and the recent-search list, all keyed by the
		// tenant. The durable copies live in Postgres and are erased by the
		// cascade; these are the Redis read-through/history keys.
		"asker:pref:" + string(tenantID),
		"asker:weights:" + string(tenantID),
		"asker:recent:" + string(tenantID),
	}
	for _, pat := range patterns {
		if err := p.scanDel(ctx, pat); err != nil {
			return err
		}
	}
	return nil
}

func (p *redisCachePurger) scanDel(ctx context.Context, match string) error {
	var cursor uint64
	for {
		keys, next, err := p.rdb.Scan(ctx, cursor, match, 256).Result()
		if err != nil {
			return fmt.Errorf("redis purge scan %q: %w", match, err)
		}
		if len(keys) > 0 {
			if err := p.rdb.Del(ctx, keys...).Err(); err != nil {
				return fmt.Errorf("redis purge del: %w", err)
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
