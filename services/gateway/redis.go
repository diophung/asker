package main

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// errStateNotFound is returned by oauthStateStore.GetDel when the key is
// absent — either it expired (TTL) or it was already consumed (single use).
// Callers must NOT distinguish the two: both map to the same opaque OAuth
// error so a probe cannot learn whether a state value ever existed.
var errStateNotFound = errors.New("oauth state not found")

// redisCounter backs the rate limiter's counter interface with Redis via
// go-redis. The client dials lazily, so constructing it performs no I/O.
type redisCounter struct {
	client *redis.Client
}

func newRedisCounter(addr string) *redisCounter {
	return &redisCounter{
		client: redis.NewClient(&redis.Options{
			Addr: addr,
			// A slow Redis must not stall requests; the limiter fails open
			// fast on error, so cap every network op and never retry.
			DialTimeout:  2 * time.Second,
			ReadTimeout:  2 * time.Second,
			WriteTimeout: 2 * time.Second,
			MaxRetries:   -1,
		}),
	}
}

// Incr pipelines INCR key + EXPIRE key ttl in one round trip and returns the
// post-increment count. Refreshing the TTL on every hit is harmless because
// the key embeds the window start.
func (rc *redisCounter) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	pipe := rc.client.Pipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}

// Set stores value under key with the given TTL (SET key value EX ttl). It is
// used by the OAuth state store to persist the short-lived, single-use flow
// state the unauthenticated callback later consumes.
func (rc *redisCounter) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return rc.client.Set(ctx, key, value, ttl).Err()
}

// GetDel atomically reads and deletes key (Redis GETDEL), giving the OAuth
// state its single-use guarantee at the server: a replayed callback finds the
// key already gone. It returns errStateNotFound when the key is absent
// (expired OR already consumed); the two are deliberately indistinguishable.
func (rc *redisCounter) GetDel(ctx context.Context, key string) (string, error) {
	v, err := rc.client.GetDel(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", errStateNotFound
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// --- Recent-search history (recentSearchStore). -----------------------------
//
// A per-tenant, most-recent-first, deduplicated, capped list of search queries
// stored as a Redis list. The key is built from the verified-token tenant
// (recent.go), never request input.

// RecordRecent moves value to the front of the list at key: it removes any
// existing exact occurrence, prepends value, caps the list to maxN entries, and
// refreshes the TTL — all in one pipeline (most-recent-first, deduped, bounded).
func (rc *redisCounter) RecordRecent(ctx context.Context, key, value string, maxN int, ttl time.Duration) error {
	pipe := rc.client.Pipeline()
	pipe.LRem(ctx, key, 0, value) // drop existing exact occurrences
	pipe.LPush(ctx, key, value)   // newest at the front
	pipe.LTrim(ctx, key, 0, int64(maxN-1))
	pipe.Expire(ctx, key, ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// RecentList returns up to n entries from the front (newest first).
func (rc *redisCounter) RecentList(ctx context.Context, key string, n int) ([]string, error) {
	return rc.client.LRange(ctx, key, 0, int64(n-1)).Result()
}

// RemoveRecent deletes every occurrence of value from the list at key.
func (rc *redisCounter) RemoveRecent(ctx context.Context, key, value string) error {
	return rc.client.LRem(ctx, key, 0, value).Err()
}

// ClearRecent removes the whole list at key.
func (rc *redisCounter) ClearRecent(ctx context.Context, key string) error {
	return rc.client.Del(ctx, key).Err()
}

// Close releases the client's connection pool.
func (rc *redisCounter) Close() error { return rc.client.Close() }
