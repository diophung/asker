package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"

	"github.com/asker/asker/platform/oauth"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy"
)

// oauthStateTTL bounds how long a started OAuth flow may stay pending before
// the user must restart. Short enough that an abandoned/leaked state is useless
// quickly, long enough that a human consent screen does not race it.
const oauthStateTTL = 10 * time.Minute

// oauthStateBytes is the entropy of the random `state`/key. 32 bytes (256 bits)
// is overwhelmingly unguessable, which is what prevents CSRF on the public,
// unauthenticated callback (the callback trusts ONLY state it issued).
const oauthStateBytes = 32

// oauthService is the slice of platform/oauth.Service the handlers use, named
// so tests can substitute a fake without standing up a real provider.
type oauthService interface {
	AuthCodeURL(p oauth.Provider, scopes []string, redirectURI, state, pkceChallengeS256 string) (string, error)
	Exchange(ctx context.Context, p oauth.Provider, code, redirectURI, pkceVerifier string) (oauth.Token, error)
}

// oauthStateStore persists the short-lived, single-use flow state keyed by the
// random `state`. Set writes with a TTL; GetDel atomically reads-and-deletes
// (single use), returning errStateNotFound when absent. redisCounter implements
// it in production; tests inject a fake.
type oauthStateStore interface {
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	GetDel(ctx context.Context, key string) (string, error)
}

// oauthFlowState is the SERVER-SIDE state created by /oauth/start and consumed
// by /oauth/callback. The tenant + connector instance the token belongs to come
// from HERE, never from the unauthenticated callback request — that is the
// crux of keeping tenant isolation intact across the public redirect.
type oauthFlowState struct {
	// Tenant is the verified-token tenant captured at /oauth/start. The callback
	// builds its tenancy context from this trusted string (never from the request).
	Tenant string `json:"tenant"`
	// InstanceID is the connector instance the token is stored under.
	InstanceID string `json:"instance_id"`
	// ConnectorID is the connector type (e.g. "gmail"); set on the stored Token.
	ConnectorID string `json:"connector_id"`
	// Provider is the OAuth provider driving this flow.
	Provider oauth.Provider `json:"provider"`
	// Verifier is the PKCE code_verifier; the matching S256 challenge went to the
	// provider in /oauth/start, and the verifier proves possession at exchange.
	Verifier string `json:"verifier"`
	// CreatedAt is for diagnostics only; expiry is enforced by the store TTL.
	CreatedAt time.Time `json:"created_at"`
}

// statePrefix namespaces the OAuth state keys in Redis so they never collide
// with the rate-limiter's "rl:" keys.
const statePrefix = "oauthstate:"

// oauthStartResponse is the GET /v1/connectors/{id}/oauth/start success body.
type oauthStartResponse struct {
	AuthorizeURL string `json:"authorize_url"`
}

// handleOAuthStart begins the authorization-code flow for an OAuth connector.
// AUTHED: the tenant comes from the verified token (rate-limiter/auth chain),
// so the flow state it persists is bound to the real caller's tenant.
func (d *deps) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if d.oauth == nil || d.oauthState == nil {
		// OAuth not configured on this gateway (no provider creds / no store).
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "oauth not configured"})
		return
	}
	tc, err := tenancy.FromContext(r.Context())
	if err != nil {
		// Unreachable behind the auth middleware; fail closed regardless.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	instanceID := r.PathValue("id")
	// Load the instance to learn its connector_id (and any user_email hint). This
	// RPC carries the tenant via the client interceptor, so a cross-tenant id
	// 404s here exactly like the other connector routes.
	resp, err := d.control.GetConnectorInstance(r.Context(), &controlplanev1.GetConnectorInstanceRequest{Id: instanceID})
	if err != nil {
		d.upstreamError(w, r, "ControlPlane.GetConnectorInstance", err)
		return
	}
	inst := resp.GetInstance()
	connectorID := inst.GetConnectorId()

	provider, scopes, ok := oauth.ConnectorOAuth(connectorID)
	if !ok {
		// Known connector instance, but its type is not an OAuth connector
		// (e.g. "upload"): a client error, nothing to authorize.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connector is not an OAuth connector"})
		return
	}
	if !d.oauthConfigured(provider) {
		// The provider has no client credentials configured on this gateway.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "oauth provider not configured"})
		return
	}

	verifier := oauth2.GenerateVerifier()
	challenge := oauth2.S256ChallengeFromVerifier(verifier)
	state, err := randomState()
	if err != nil {
		d.internalError(w, r, "generate oauth state", err)
		return
	}

	flow := oauthFlowState{
		Tenant:      string(tc.TenantID()),
		InstanceID:  instanceID,
		ConnectorID: connectorID,
		Provider:    provider,
		Verifier:    verifier,
		CreatedAt:   time.Now().UTC(),
	}
	blob, err := json.Marshal(flow)
	if err != nil {
		d.internalError(w, r, "marshal oauth state", err)
		return
	}
	if err := d.oauthState.Set(r.Context(), statePrefix+state, string(blob), oauthStateTTL); err != nil {
		d.internalError(w, r, "store oauth state", err)
		return
	}

	redirectURI := d.gatewayPublicURL + "/v1/oauth/callback"
	authURL, err := d.oauth.AuthCodeURL(provider, scopes, redirectURI, state, challenge)
	if err != nil {
		// Misconfiguration (unknown/unconfigured provider) — opaque to the client.
		d.internalError(w, r, "build authorize url", err)
		return
	}
	// For Google, pass the configured account hint so the dev fake-oauth issues
	// the matching fake-gmail token and the real Google pre-selects the account.
	if provider == oauth.Google {
		if hint := userEmailFromConfig(inst.GetConfigJson()); hint != "" {
			authURL = appendQueryParam(authURL, "login_hint", hint)
		}
	}

	writeJSON(w, http.StatusOK, oauthStartResponse{AuthorizeURL: authURL})
}

// handleOAuthCallback is the PUBLIC provider redirect target. It is
// UNAUTHENTICATED (no bearer), so EVERYTHING it trusts — the tenant, the
// connector instance, the provider, the PKCE verifier — comes from the
// single-use server-side state keyed by the unguessable `state`, never from the
// request. On any failure it redirects to the fixed web app with ?oauth=error
// and leaks no code/token/internal detail.
func (d *deps) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if d.oauth == nil || d.oauthState == nil {
		d.redirectOAuthError(w, r, "oauth not configured", nil)
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		d.redirectOAuthError(w, r, "missing state", nil)
		return
	}

	// Single-use: GetDel atomically consumes the state. A replay finds it gone.
	blob, err := d.oauthState.GetDel(r.Context(), statePrefix+state)
	if err != nil {
		// Missing/expired/replayed — all indistinguishable on purpose.
		d.redirectOAuthError(w, r, "state not found", err)
		return
	}
	var flow oauthFlowState
	if err := json.Unmarshal([]byte(blob), &flow); err != nil {
		d.redirectOAuthError(w, r, "decode state", err)
		return
	}

	// A provider-side error (user denied consent, etc.) arrives as ?error=...
	// AFTER we have already consumed the state, so a denied flow cannot be
	// replayed either.
	if provErr := q.Get("error"); provErr != "" {
		d.redirectOAuthError(w, r, "provider error: "+provErr, nil)
		return
	}
	code := q.Get("code")
	if code == "" {
		d.redirectOAuthError(w, r, "missing code", nil)
		return
	}

	// Reconstruct the tenancy context from the TRUSTED state string (the same
	// path internal hops use), so the outbound PutToken carries x-asker-tenant =
	// state.Tenant. The callback request itself never names a tenant.
	tc, err := tenancy.FromHeaderValue(flow.Tenant)
	if err != nil {
		d.redirectOAuthError(w, r, "invalid tenant in state", err)
		return
	}
	ctx := tenancy.WithContext(r.Context(), tc)

	redirectURI := d.gatewayPublicURL + "/v1/oauth/callback"
	token, err := d.oauth.Exchange(ctx, flow.Provider, code, redirectURI, flow.Verifier)
	if err != nil {
		// Never log the code; Exchange's error is already secret-free, but keep
		// it server-side only.
		d.redirectOAuthError(w, r, "token exchange failed", err)
		return
	}
	// Stamp the connector identity onto the stored token so the hub can map it
	// back to a provider/connector without re-deriving.
	token.Provider = flow.Provider
	token.ConnectorID = flow.ConnectorID

	blobOut, err := oauth.Marshal(token)
	if err != nil {
		d.redirectOAuthError(w, r, "marshal token", err)
		return
	}
	// The client interceptor reads the tenant from ctx and sets x-asker-tenant —
	// the SAME encrypted vault used for manual PutToken, now keyed by the state's
	// tenant, NOT anything the unauthenticated request supplied.
	if _, err := d.control.PutToken(ctx, &controlplanev1.PutTokenRequest{
		ConnectorInstanceId: flow.InstanceID,
		Token:               blobOut,
	}); err != nil {
		d.redirectOAuthError(w, r, "store token failed", err)
		return
	}

	http.Redirect(w, r, d.webAppURL+"/connectors?oauth=connected", http.StatusFound)
}

// redirectOAuthError logs the real reason server-side (without secrets) and
// redirects the browser to the fixed web app with an opaque ?oauth=error.
func (d *deps) redirectOAuthError(w http.ResponseWriter, r *http.Request, reason string, err error) {
	attrs := []any{"path", r.URL.Path, "reason", reason}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	d.logger.Warn("oauth callback failed", attrs...)
	http.Redirect(w, r, d.webAppURL+"/connectors?oauth=error", http.StatusFound)
}

// oauthConfigured reports whether provider p has client credentials on this
// gateway. It is nil-safe so tests that omit oauthCfg still work.
func (d *deps) oauthConfigured(p oauth.Provider) bool {
	return d.oauthCfg.IsConfigured(p)
}

// randomState returns a URL-safe, unguessable random string used both as the
// CSRF `state` and as the Redis key for the flow state.
func randomState() (string, error) {
	b := make([]byte, oauthStateBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// userEmailFromConfig extracts a "user_email" string from a connector
// instance's config JSON, or "" when absent/unparsable. Used only as a
// login_hint for Google; never affects tenant or token storage.
func userEmailFromConfig(configJSON []byte) string {
	if len(configJSON) == 0 {
		return ""
	}
	var cfg struct {
		UserEmail string `json:"user_email"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return ""
	}
	return cfg.UserEmail
}

// appendQueryParam adds key=value to a URL's query string, preserving existing
// params. On a parse failure it returns the input unchanged (the hint is
// best-effort).
func appendQueryParam(rawURL, key, value string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}
