package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	"github.com/asker/asker/platform/tenancy"
)

// cacheTTL is the result-cache lifetime (build contract: 60s).
const cacheTTL = 60 * time.Second

// resultCache abstracts the exactly two Redis operations the query path uses
// so tests can substitute a fake and a Redis outage degrades to "no cache"
// instead of failing searches.
type resultCache interface {
	// Get returns the cached value and whether the key was present.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores value under key with the given TTL.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// cacheKey derives the result-cache key "q:<tenant>:<sha256 of normalized
// request>". The hash input is a canonical, field-separated rendering of the
// normalized request (proto wire marshaling is not guaranteed deterministic,
// so it is not hashed directly). Doc types are sorted so order-insensitive
// equivalent requests share an entry.
//
// clipArm records whether the CLIP text->image arm is part of this query's
// plan (ADR-013) — it is folded into the key so a result computed without the
// CLIP arm (e.g. an M2-era entry, or a deployment where CLIP was not yet in
// the plan) is never served for a request that now plans the CLIP arm, and
// vice versa. Only non-degraded results are cached, so a CLIP-down result is
// never stored; this key bit is the belt-and-suspenders namespace separation.
// profileVersion is the caller's personalization profile version (0 on the
// non-personalized path). Folding it into the key means a preference change
// (which bumps the version) invalidates this tenant's cached orders immediately,
// rather than serving the old ranking until the 60s TTL expires (DECISIONS D4).
func cacheKey(tenant tenancy.TenantID, req *queryv1.SearchRequest, clipArm bool, profileVersion int64) string {
	types := make([]int32, 0, len(req.GetDocTypes()))
	for _, t := range req.GetDocTypes() {
		types = append(types, int32(t))
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })

	var b strings.Builder
	b.WriteString(req.GetQuery())
	b.WriteByte(0x1f)
	for _, t := range types {
		fmt.Fprintf(&b, "%d,", t)
	}
	b.WriteByte(0x1f)
	writeTimestampField := func(set bool, sec int64, nanos int32) {
		if set {
			fmt.Fprintf(&b, "%d.%d", sec, nanos)
		} else {
			b.WriteByte('-')
		}
		b.WriteByte(0x1f)
	}
	writeTimestampField(req.GetFromDate() != nil, req.GetFromDate().GetSeconds(), req.GetFromDate().GetNanos())
	writeTimestampField(req.GetToDate() != nil, req.GetToDate().GetSeconds(), req.GetToDate().GetNanos())
	b.WriteString(req.GetParticipant())
	fmt.Fprintf(&b, "\x1f%d\x1f%d\x1f%d\x1f%t\x1f%d", req.GetLimit(), req.GetOffset(), req.GetMode(), clipArm, profileVersion)

	sum := sha256.Sum256([]byte(b.String()))
	return "q:" + string(tenant) + ":" + hex.EncodeToString(sum[:])
}

// redisOpTimeout bounds each phase of a cache operation so a sick Redis
// cannot eat the query latency budget (cache check is a 5ms line item).
const redisOpTimeout = 250 * time.Millisecond

// redisCache implements resultCache on go-redis/v9 with only the two
// commands the query path needs (GET, SET with expiry).
type redisCache struct {
	client *redis.Client
}

func newRedisCache(addr string) *redisCache {
	return &redisCache{client: redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  redisOpTimeout,
		ReadTimeout:  redisOpTimeout,
		WriteTimeout: redisOpTimeout,
		// No retries: a failed cache op degrades to "no cache" at the caller;
		// retry backoff would only burn the search latency budget.
		MaxRetries: -1,
	})}
}

// Get implements resultCache.
func (c *redisCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	val, err := c.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return val, true, nil
}

// Set implements resultCache.
func (c *redisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return c.client.Set(ctx, key, value, ttl).Err()
}

// Close releases the client's connection pool (process shutdown).
func (c *redisCache) Close() {
	_ = c.client.Close()
}
