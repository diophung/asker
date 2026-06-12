package main

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisSeen implements seenStore on top of github.com/redis/go-redis/v9.
// SetNX maps to Redis "SET key value NX EX <ttl>" (atomic SETNX + TTL); the
// library owns connection pooling, reconnects, and protocol negotiation.
// Any remaining error surfaces to the handler, which fails OPEN (the
// document is processed anyway with a warning).
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

// SetNX implements seenStore. It reports whether THIS call set the key
// (false = already seen).
func (r *redisSeen) SetNX(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	fresh, err := r.client.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis setnx: %w", err)
	}
	return fresh, nil
}

// Close implements seenStore.
func (r *redisSeen) Close() error {
	return r.client.Close()
}
