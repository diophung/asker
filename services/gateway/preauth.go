package main

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// The pre-auth throttle sits IN FRONT of the auth middleware so an
// UNAUTHENTICATED flood cannot hammer JWT verification + JWKS refetch unbounded
// (the M6 DoS finding). It is intentionally cheap and tenant-agnostic: there is
// no tenant yet at this layer (the tenant is derived only AFTER token
// verification, ADR-002), so it limits by source IP plus a global ceiling.
// The existing per-tenant limiter (ratelimit.go) still runs POST-auth.
//
// limit <= 0 disables a dimension (pass-through); when enabled, excess fails
// CLOSED with 429, because the point is to shed unauthenticated load before it
// reaches the expensive crypto path. The limiter is a hand-rolled token bucket
// (no new module dependency; go.mod stays frozen).

// tokenBucket is a minimal monotonic token-bucket rate limiter: capacity tokens
// max, refilled at refillPerSec tokens/second. Safe for concurrent use.
type tokenBucket struct {
	mu           sync.Mutex
	capacity     float64
	tokens       float64
	refillPerSec float64
	last         time.Time
	now          func() time.Time
}

func newTokenBucket(capacity, refillPerSec float64, now func() time.Time) *tokenBucket {
	if now == nil {
		now = time.Now
	}
	return &tokenBucket{
		capacity:     capacity,
		tokens:       capacity,
		refillPerSec: refillPerSec,
		last:         now(),
		now:          now,
	}
}

// allow consumes one token, returning false when the bucket is empty.
func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = min(b.capacity, b.tokens+elapsed*b.refillPerSec)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// preAuthLimiter throttles requests before authentication by source IP, with a
// global ceiling as a backstop against a distributed flood. Per-IP buckets live
// in a bounded map swept periodically so a churn of source IPs cannot grow
// memory without bound.
type preAuthLimiter struct {
	perIPCap    float64
	perIPRefill float64
	global      *tokenBucket
	trustProxy  bool
	now         func() time.Time

	mu       sync.Mutex
	ips      map[string]*ipEntry
	lastWeep time.Time
}

type ipEntry struct {
	bucket *tokenBucket
	seen   time.Time
}

const (
	ipEntryTTL    = 10 * time.Minute
	ipSweepEvery  = 1 * time.Minute
	maxTrackedIPs = 50_000
)

// newPreAuthLimiter builds the throttle. perIPPerMinute/globalPerSecond <= 0
// disable that dimension. trustProxy uses X-Forwarded-For's left-most hop as
// the client IP (only safe behind a trusted proxy that sets it).
func newPreAuthLimiter(perIPPerMinute, globalPerSecond, globalBurst int, trustProxy bool) *preAuthLimiter {
	return newPreAuthLimiterClock(perIPPerMinute, globalPerSecond, globalBurst, trustProxy, time.Now)
}

func newPreAuthLimiterClock(perIPPerMinute, globalPerSecond, globalBurst int, trustProxy bool, now func() time.Time) *preAuthLimiter {
	l := &preAuthLimiter{
		trustProxy: trustProxy,
		now:        now,
		ips:        make(map[string]*ipEntry),
	}
	if perIPPerMinute > 0 {
		l.perIPCap = float64(perIPPerMinute) // allow a one-minute burst
		l.perIPRefill = float64(perIPPerMinute) / 60.0
	}
	if globalPerSecond > 0 {
		burst := globalBurst
		if burst <= 0 {
			burst = globalPerSecond
		}
		l.global = newTokenBucket(float64(burst), float64(globalPerSecond), now)
	}
	return l
}

// middleware enforces the pre-auth throttle. When disabled (both dimensions
// off) it is a pass-through.
func (l *preAuthLimiter) middleware(next http.Handler) http.Handler {
	if l.perIPRefill <= 0 && l.global == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Global ceiling first (cheapest, protects against a distributed flood).
		if l.global != nil && !l.global.allow() {
			tooMany(w)
			return
		}
		if l.perIPRefill > 0 {
			if !l.allowIP(l.clientIP(r)) {
				tooMany(w)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (l *preAuthLimiter) allowIP(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	e, ok := l.ips[ip]
	if !ok {
		if len(l.ips) >= maxTrackedIPs {
			// Table full even after a sweep: do not track new IPs (still bounded
			// by the global ceiling). Fail open per-IP, not global.
			return true
		}
		e = &ipEntry{bucket: newTokenBucket(l.perIPCap, l.perIPRefill, l.now)}
		l.ips[ip] = e
	}
	e.seen = now
	return e.bucket.allow()
}

// sweepLocked evicts stale per-IP entries at most once per ipSweepEvery.
func (l *preAuthLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastWeep) < ipSweepEvery {
		return
	}
	l.lastWeep = now
	for ip, e := range l.ips {
		if now.Sub(e.seen) > ipEntryTTL {
			delete(l.ips, ip)
		}
	}
}

// clientIP extracts the source IP: the TCP peer by default, or the left-most
// X-Forwarded-For hop when trustProxy is set (only behind a trusted proxy).
func (l *preAuthLimiter) clientIP(r *http.Request) string {
	if l.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := indexByte(xff, ','); i >= 0 {
				return trimSpace(xff[:i])
			}
			return trimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func tooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many requests"})
}

// small dependency-free helpers (avoid importing strings just for two calls).
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
