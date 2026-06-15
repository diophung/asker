package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/asker/asker/platform/tenancy"
)

const (
	// maxRecentSearches bounds the stored history per tenant (Google shows a
	// short list; the dropdown filters it client-side).
	maxRecentSearches = 20
	// recentSearchTTL expires a tenant's history if they stop searching.
	recentSearchTTL = 90 * 24 * time.Hour
	// maxRecentQueryLen caps a stored query (defense against junk/oversized q).
	maxRecentQueryLen = 256
	// maxRecentBodyBytes bounds the POST body read.
	maxRecentBodyBytes = 4 << 10
)

// recentSearchStore persists a per-tenant, most-recent-first, deduplicated list
// of search queries (Redis in production; an in-memory fake in tests). Every
// key is derived from the verified-token tenant (recentKey) — never request
// input — so one tenant's history is never visible to another.
type recentSearchStore interface {
	RecordRecent(ctx context.Context, key, value string, maxN int, ttl time.Duration) error
	RecentList(ctx context.Context, key string, n int) ([]string, error)
	RemoveRecent(ctx context.Context, key, value string) error
	ClearRecent(ctx context.Context, key string) error
}

func recentKey(tenant tenancy.TenantID) string {
	return "asker:recent:" + string(tenant)
}

// normalizeRecentQuery trims, collapses internal whitespace, and rune-caps a
// query so storage and dedup operate on a canonical form. Returns "" for an
// effectively empty query (which is never recorded).
func normalizeRecentQuery(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if q == "" {
		return ""
	}
	if r := []rune(q); len(r) > maxRecentQueryLen {
		q = strings.TrimSpace(string(r[:maxRecentQueryLen]))
	}
	return q
}

// recordRecent stores a query in the caller's recent-search history. Best
// effort: a nil store (tests not exercising history) or a Redis error never
// affects the search response. The tenant comes from the verified context only.
func (d *deps) recordRecent(ctx context.Context, q string) {
	if d.recent == nil {
		return
	}
	tc, err := tenancy.FromContext(ctx)
	if err != nil {
		return
	}
	norm := normalizeRecentQuery(q)
	if norm == "" {
		return
	}
	if err := d.recent.RecordRecent(ctx, recentKey(tc.TenantID()), norm, maxRecentSearches, recentSearchTTL); err != nil {
		d.logger.Debug("record recent search failed", "error", err)
	}
}

// handleListRecent serves GET /v1/searches/recent: the caller's recent queries,
// newest first. Recent history is non-critical, so a store outage degrades to an
// empty list rather than failing the request.
func (d *deps) handleListRecent(w http.ResponseWriter, r *http.Request) {
	tc, err := tenancy.FromContext(r.Context())
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	searches := []string{}
	if d.recent != nil {
		list, lErr := d.recent.RecentList(r.Context(), recentKey(tc.TenantID()), maxRecentSearches)
		if lErr != nil {
			d.logger.Debug("list recent searches failed", "error", lErr)
		} else if list != nil {
			searches = list
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"searches": searches})
}

// handlePostRecent serves POST /v1/searches {"q":"..."}: explicitly record a
// query. Searches are also auto-recorded by the search handlers, so this is an
// optional convenience; it always returns 204.
func (d *deps) handlePostRecent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Q string `json:"q"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRecentBodyBytes)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	d.recordRecent(r.Context(), body.Q)
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteRecent serves DELETE /v1/searches: remove one entry (?q=<query>)
// or clear the whole history (no q). Like the list path it operates only on the
// caller's own tenant key.
func (d *deps) handleDeleteRecent(w http.ResponseWriter, r *http.Request) {
	tc, err := tenancy.FromContext(r.Context())
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if d.recent == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	key := recentKey(tc.TenantID())
	if q := normalizeRecentQuery(r.URL.Query().Get("q")); q != "" {
		if err := d.recent.RemoveRecent(r.Context(), key, q); err != nil {
			d.logger.Debug("remove recent search failed", "error", err)
		}
	} else if err := d.recent.ClearRecent(r.Context(), key); err != nil {
		d.logger.Debug("clear recent searches failed", "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}
