package main

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/asker/asker/platform/telemetry"
	"github.com/asker/asker/platform/tenancy"
)

const readyzProbeTimeout = 2 * time.Second

// newHandler assembles the middleware chain. Outermost to innermost:
// CORS (answers preflights pre-auth) -> pre-auth throttle (per-IP + global,
// IN FRONT of auth so unauth floods can't hammer JWKS/crypto) -> telemetry ->
// mux -> auth -> per-tenant rate limit -> handler. Admin routes add a distinct
// admin-claim authorization check after auth.
func newHandler(cfg gatewayConfig, auth *authenticator, d *deps) http.Handler {
	limiter := newRateLimiter(d.counter, cfg.RateLimitPerMinute, d.logger)
	cors := newCORSPolicy(cfg.CORSAllowedOrigins)
	preAuth := newPreAuthLimiter(cfg.PreAuthPerIPPerMinute, cfg.PreAuthGlobalPerSec, cfg.PreAuthGlobalBurst, cfg.TrustProxyHeaders)

	// authed routes: tenant from the verified token ONLY, then rate limit.
	authed := func(h http.Handler) http.Handler {
		return auth.middleware(limiter.middleware(h))
	}
	// adminAuthed: authed PLUS a distinct admin-role check (not mere authn).
	adminAuthed := func(h http.Handler) http.Handler {
		return authed(d.requireAdmin(h))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", getOnly(handleHealthz))
	mux.HandleFunc("/readyz", getOnly(handleReadyz(cfg.OIDCJWKSURL)))
	// Prometheus scrape endpoint. The RED metrics (http_server_requests_total,
	// http_server_request_duration_seconds) the telemetry HTTP middleware
	// records are exported here too; works without an OTLP collector. It sits
	// outside the auth chain (operator/Prometheus surface, not a tenant API)
	// and carries no tenant context, so no isolation concern.
	mux.Handle("GET /metrics", telemetry.MetricsHandler())
	mux.Handle("/v1/me", authed(getOnly(handleMe)))
	// GDPR self-service erasure: the user deletes THEIR OWN tenant. The tenant
	// is from the verified token only; there is no body field that selects one.
	mux.Handle("/v1/me/data", authed(methods(map[string]http.HandlerFunc{
		http.MethodDelete: d.handleDeleteMyData,
	})))
	mux.Handle("/v1/search", authed(getOnly(d.handleSearch)))
	// Per-source search (the v2 source tabs navigate here on a full page load):
	// /v1/search/email, /files, /messages, /calendar, /photos, /people. The path
	// segment selects the DocType filter (search.go) or the people aggregation.
	mux.Handle("/v1/search/{source}", authed(getOnly(d.handleSourceSearch)))
	// Recent-search history (per tenant, from the verified token). GET lists the
	// caller's recent queries; POST records one; DELETE removes one (?q=) or all.
	mux.Handle("/v1/searches/recent", authed(getOnly(d.handleListRecent)))
	mux.Handle("/v1/searches", authed(methods(map[string]http.HandlerFunc{
		http.MethodPost:   d.handlePostRecent,
		http.MethodDelete: d.handleDeleteRecent,
	})))
	// Personalization (v3.2): per-tenant preferences + behavioral feedback. All
	// authed; the tenant is the verified-token tenant only. Settings GET/PUT, a
	// feedback capture endpoint, and the mandatory reset/export data-rights
	// controls.
	mux.Handle("/v1/preferences", authed(methods(map[string]http.HandlerFunc{
		http.MethodGet: d.handleGetPreferences,
		http.MethodPut: d.handlePutPreferences,
	})))
	mux.Handle("/v1/preferences/reset", authed(methods(map[string]http.HandlerFunc{
		http.MethodPost: d.handleResetLearning,
	})))
	mux.Handle("/v1/preferences/export", authed(getOnly(d.handleExportPersonalization)))
	mux.Handle("/v1/feedback", authed(methods(map[string]http.HandlerFunc{
		http.MethodPost: d.handleFeedback,
	})))
	mux.Handle("/v1/media", authed(getOnly(d.handleMedia)))
	mux.Handle("/v1/connectors", authed(methods(map[string]http.HandlerFunc{
		http.MethodGet:  d.handleListConnectors,
		http.MethodPost: d.handleCreateConnector,
	})))
	mux.Handle("/v1/connectors/{id}", authed(methods(map[string]http.HandlerFunc{
		http.MethodDelete: d.handleDeleteConnector,
	})))
	mux.Handle("/v1/connectors/{id}/token", authed(methods(map[string]http.HandlerFunc{
		http.MethodPut: d.handlePutToken,
	})))
	// Live indexing progress (indexed vs. emitted + per-connector sync state) and
	// re-index (reset a connector's sync cursor so the hub re-runs a full sync).
	mux.Handle("/v1/index/status", authed(getOnly(d.handleIndexStatus)))
	mux.Handle("/v1/connectors/{id}/reindex", authed(methods(map[string]http.HandlerFunc{
		http.MethodPost: d.handleReindexConnector,
	})))
	// OAuth connector authorization start (wave 1). AUTHED: the tenant is the
	// verified-token tenant, and the server-side flow state it persists is bound
	// to it. The web fetches this, then navigates the browser to authorize_url.
	mux.Handle("/v1/connectors/{id}/oauth/start", authed(getOnly(d.handleOAuthStart)))
	mux.Handle("/v1/upload", authed(methods(map[string]http.HandlerFunc{
		http.MethodPost: d.handleUpload,
	})))

	// OAuth provider redirect target. PUBLIC: registered OUTSIDE the authed
	// chain (the provider's browser redirect carries no bearer). It still sits
	// behind the pre-auth throttle + telemetry that wrap the whole mux below.
	// Tenant + connector come ONLY from the single-use server-side state keyed by
	// the unguessable `state`, never from this request (see oauth.go).
	mux.Handle("/v1/oauth/callback", getOnly(d.handleOAuthCallback))

	// Admin API (M6): privileged, cross-tenant operator surface, gated by the
	// admin role/scope claim (admin.go). NOT a web UI — an API; the console is a
	// follow-up. Audit-logged at the control plane.
	mux.Handle("/v1/admin/tenants", adminAuthed(getOnly(d.handleAdminListTenants)))
	mux.Handle("/v1/admin/tenants/{tenant}", adminAuthed(methods(map[string]http.HandlerFunc{
		http.MethodGet:    d.handleAdminGetTenant,
		http.MethodDelete: d.handleAdminDeleteTenant,
	})))
	mux.Handle("/v1/admin/tenants/{tenant}/suspend", adminAuthed(methods(map[string]http.HandlerFunc{
		http.MethodPost: d.handleAdminSuspendTenant,
	})))

	mux.HandleFunc("/", handleNotFound)
	return cors.middleware(preAuth.middleware(telemetry.HTTPMiddleware("gateway")(mux)))
}

// methods dispatches by HTTP method with a JSON 405 (and Allow header) for
// anything unhandled, mirroring getOnly for multi-method routes.
func methods(handlers map[string]http.HandlerFunc) http.Handler {
	allow := strings.Join(slices.Sorted(maps.Keys(handlers)), ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := handlers[r.Method]; ok {
			h(w, r)
			return
		}
		w.Header().Set("Allow", allow)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	})
}

// getOnly rejects non-GET methods with a JSON 405 (ServeMux method patterns
// would answer with a plain-text body).
func getOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		h(w, r)
	}
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports ready only when the JWKS endpoint is fetchable, since
// the gateway cannot authenticate anything without it.
func handleReadyz(jwksURL string) http.HandlerFunc {
	client := &http.Client{Timeout: readyzProbeTimeout}
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, jwksURL, nil)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	tc, err := tenancy.FromContext(r.Context())
	if err != nil {
		// Unreachable behind the auth middleware; fail closed regardless.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	email := ""
	if claims := claimsFromContext(r.Context()); claims != nil {
		if e, ok := claims["email"].(string); ok {
			email = e
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"tenant_id": string(tc.TenantID()),
		"subject":   tc.Subject(),
		"email":     email,
	})
}

func handleNotFound(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The bodies passed here (maps, slices, plain structs) cannot fail to
	// encode; a write error means the client went away and there is nothing
	// useful left to do.
	_ = json.NewEncoder(w).Encode(body)
}
