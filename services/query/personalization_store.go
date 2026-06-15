package main

import (
	"context"
	"encoding/json"

	"github.com/redis/go-redis/v9"

	"github.com/asker/asker/platform/personalization"
	"github.com/asker/asker/platform/tenancy"
)

// profileLoader supplies the per-tenant ranking inputs (resolved preference
// profile + learned model) to the query path. A nil loader on the server means
// personalization is OFF (the non-personalized path, used by every existing
// test); a wired loader turns it ON (main.go).
//
// Load NEVER fails the search: on a miss or any backend error it returns
// cold-start defaults (DefaultProfile + empty model), so a Redis outage degrades
// to sensible non-personalized ranking, never an error (spec §3 cold start,
// DECISIONS D4). The returned profile.Version (0 at cold start) is folded into
// the result-cache key so a preference change invalidates cached orders.
type profileLoader interface {
	Load(ctx context.Context, tenant tenancy.TenantID) (personalization.Profile, personalization.LearnedModel)
}

// redisProfileLoader reads the profile/model the gateway write-through-caches at
// personalization.RedisProfileKey / RedisWeightsKey.
type redisProfileLoader struct {
	client *redis.Client
}

func newRedisProfileLoader(addr string) *redisProfileLoader {
	return &redisProfileLoader{client: redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  redisOpTimeout,
		ReadTimeout:  redisOpTimeout,
		WriteTimeout: redisOpTimeout,
		// A sick Redis must not eat the search latency budget; a missing/slow
		// read degrades to defaults at the caller, so never retry.
		MaxRetries: -1,
	})}
}

// Load implements profileLoader.
func (l *redisProfileLoader) Load(ctx context.Context, tenant tenancy.TenantID) (personalization.Profile, personalization.LearnedModel) {
	profile := personalization.DefaultProfile()
	if data, err := l.client.Get(ctx, personalization.RedisProfileKey(string(tenant))).Bytes(); err == nil {
		var p personalization.Profile
		if json.Unmarshal(data, &p) == nil {
			profile = personalization.Clamp(p) // defend against a stale/garbled blob
		}
	}

	var model personalization.LearnedModel
	if data, err := l.client.Get(ctx, personalization.RedisWeightsKey(string(tenant))).Bytes(); err == nil {
		_ = json.Unmarshal(data, &model)
	}
	return profile, model
}

// Close releases the loader's Redis connection pool (process shutdown).
func (l *redisProfileLoader) Close() {
	_ = l.client.Close()
}
