package main

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisSeen implements seenStore on top of github.com/redis/go-redis/v9.
// Seen maps to EXISTS (a read), MarkSeen to "SET key 1 EX <ttl>" (write after
// a successful produce); the library owns connection pooling, reconnects, and
// protocol negotiation. Any error surfaces to the handler, which fails OPEN
// (the document is processed anyway with a warning).
type redisSeen struct {
	client *redis.Client
}

// redisTimeout bounds dial/read/write per command so a hung Redis cannot
// stall the pipeline beyond a beat (the handler proceeds without dedupe).
const redisTimeout = 2 * time.Second

func newRedisSeen(addr string) *redisSeen {
	return &redisSeen{client: redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  redisTimeout,
		ReadTimeout:  redisTimeout,
		WriteTimeout: redisTimeout,
	})}
}

// Seen implements seenStore: true when the key is already recorded.
func (r *redisSeen) Seen(ctx context.Context, key string) (bool, error) {
	n, err := r.client.Exists(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("redis exists: %w", err)
	}
	return n > 0, nil
}

// MarkSeen implements seenStore: record key with ttl. Called only after the
// document has been durably produced to docs.chunked.
func (r *redisSeen) MarkSeen(ctx context.Context, key string, ttl time.Duration) error {
	if err := r.client.Set(ctx, key, "1", ttl).Err(); err != nil {
		return fmt.Errorf("redis set: %w", err)
	}
	return nil
}

// Close implements seenStore.
func (r *redisSeen) Close() error {
	return r.client.Close()
}
