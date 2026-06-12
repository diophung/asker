package main

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

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

// Close releases the client's connection pool.
func (rc *redisCounter) Close() error { return rc.client.Close() }
