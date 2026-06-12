package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/asker/asker/platform/tenancy"
)

// rateCounter is the tiny slice of Redis the limiter needs: atomically
// increment key and ensure it expires after ttl, returning the new count.
// Backed by redisCounter in production and a fake in tests.
type rateCounter interface {
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
}

// rateWindowTTL is the expiry set on each fixed-window counter key. Longer
// than the 60s window so a key never vanishes mid-window, short enough that
// stale windows do not accumulate in Redis.
const rateWindowTTL = 90 * time.Second

// rateLimiter is a per-tenant fixed-window limiter. M1 policy: availability
// over limiting — when Redis is unreachable it fails OPEN (requests pass) and
// warns loudly, at most once per minute.
type rateLimiter struct {
	counter rateCounter
	limit   int
	logger  *slog.Logger
	now     func() time.Time // injectable clock for tests

	mu       sync.Mutex
	lastWarn time.Time
}

func newRateLimiter(counter rateCounter, limit int, logger *slog.Logger) *rateLimiter {
	return &rateLimiter{counter: counter, limit: limit, logger: logger, now: time.Now}
}

// middleware enforces the limit for the tenant established by the auth
// middleware. It MUST run after auth: with no tenant in context it fails
// closed with a 401 rather than guessing a bucket.
func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	if rl.limit <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tc, err := tenancy.FromContext(r.Context())
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		now := rl.now()
		key := fmt.Sprintf("rl:%s:%d", tc.TenantID(), now.Unix()/60)
		n, err := rl.counter.Incr(r.Context(), key, rateWindowTTL)
		if err != nil {
			rl.warnFailOpen(err)
			next.ServeHTTP(w, r) // fail OPEN: availability over limiting in M1
			return
		}
		if n > int64(rl.limit) {
			retryAfter := 60 - now.Unix()%60
			w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// warnFailOpen logs the fail-open condition at most once per minute so a dead
// Redis does not flood the logs at request rate.
func (rl *rateLimiter) warnFailOpen(err error) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	if now.Sub(rl.lastWarn) < time.Minute {
		return
	}
	rl.lastWarn = now
	rl.logger.Warn("rate limiter failing OPEN: redis unreachable, requests are NOT being limited", "error", err)
}
