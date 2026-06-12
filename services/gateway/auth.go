package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/asker/asker/platform/tenancy"
)

type ctxKey int

const claimsContextKey ctxKey = iota

// claimsFromContext returns the verified JWT claims stored by the auth
// middleware, or nil if the request did not pass through it.
func claimsFromContext(ctx context.Context) map[string]any {
	claims, _ := ctx.Value(claimsContextKey).(map[string]any)
	return claims
}

type authenticator struct {
	verifier *oidc.IDTokenVerifier
	logger   *slog.Logger
}

// newAuthenticator builds the OIDC token verifier. oidc.NewRemoteKeySet does
// no network I/O at construction time — JWKS is fetched lazily on the first
// verification — so the gateway starts even if Keycloak is not up yet.
func newAuthenticator(ctx context.Context, issuer, jwksURL, audience string, logger *slog.Logger) *authenticator {
	keySet := oidc.NewRemoteKeySet(ctx, jwksURL)
	verifier := oidc.NewVerifier(issuer, keySet, &oidc.Config{ClientID: audience})
	return &authenticator{verifier: verifier, logger: logger}
}

// middleware enforces a valid Bearer token and derives the tenant identity
// exclusively from the verified token claims — never from request data.
func (a *authenticator) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			a.unauthorized(w, r, "missing or malformed Authorization header", nil)
			return
		}
		token, err := a.verifier.Verify(r.Context(), raw)
		if err != nil {
			a.unauthorized(w, r, "token verification failed", err)
			return
		}
		var claims map[string]any
		if err := token.Claims(&claims); err != nil {
			a.unauthorized(w, r, "decoding token claims failed", err)
			return
		}
		tc, err := tenancy.FromClaims(claims)
		if err != nil {
			a.unauthorized(w, r, "no tenant in token claims", err)
			return
		}
		ctx := tenancy.WithContext(r.Context(), tc)
		ctx = context.WithValue(ctx, claimsContextKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// unauthorized logs the real failure reason server-side and returns an opaque
// 401 to the client.
func (a *authenticator) unauthorized(w http.ResponseWriter, r *http.Request, reason string, err error) {
	attrs := []any{"path", r.URL.Path, "reason", reason}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	a.logger.Warn("unauthorized request", attrs...)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
