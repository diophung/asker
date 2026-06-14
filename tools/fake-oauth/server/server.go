// Package server implements a standards-correct fake OAuth 2.0 authorization
// server for dev and CI (never deployed to production). It lets the whole Asker
// connector OAuth flow run end to end without real Google/Microsoft/Slack/
// Atlassian credentials.
//
// It speaks the same endpoints platform/oauth's [oauth.Service] drives:
//
//	GET  /{provider}/authorize  - auto-consents (dev), mints a one-time code,
//	                              records the PKCE challenge + redirect_uri, and
//	                              302-redirects back to redirect_uri?code&state.
//	POST /{provider}/token      - authorization_code: validates the code, the
//	                              PKCE verifier (S256), and redirect_uri, then
//	                              issues access+refresh tokens. refresh_token:
//	                              rotates the access token. Standard providers
//	                              return RFC 6749 JSON; Slack returns its
//	                              non-standard {"ok":...,"authed_user":{...}}.
//
// Providers are distinguished by URL path so a single instance backs all four:
// dev sets ASKER_OAUTH_GOOGLE_AUTH_URL=http://fake-oauth:PORT/google/authorize,
// ASKER_OAUTH_GOOGLE_TOKEN_URL=http://fake-oauth:PORT/google/token, etc.
//
// For Google the issued access token is "fake-gmail-token:<subject>" so the
// Gmail connector's call to the dev fake-gmail succeeds; other providers get a
// distinct opaque "fakeoauth:<provider>:<subject>". Tokens and secrets are
// never logged.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// defaultSubject is the resource-owner identity auto-consent uses when the
	// authorize request carries no login_hint.
	defaultSubject = "alice@example.com"

	// defaultCodeTTL bounds how long a minted authorization code is valid.
	defaultCodeTTL = 5 * time.Minute

	// accessTokenTTL is the lifetime of every issued access token (expires_in).
	accessTokenTTL = time.Hour

	// gmailTokenPrefix matches the dev shim the fake-gmail server accepts:
	// "fake-gmail-token:<email>". Google access tokens use it so the Gmail
	// connector's downstream call works end to end.
	gmailTokenPrefix = "fake-gmail-token:"

	// maxFormBytes bounds a token-request body so malformed input cannot
	// exhaust memory.
	maxFormBytes = 1 << 16 // 64 KiB

	// codeBytes / tokenBytes size the random identifiers (raw bytes before
	// base64url encoding).
	codeBytes  = 24
	tokenBytes = 24
)

// providers is the set of provider path segments this fake serves. It mirrors
// platform/oauth.Providers() but is kept local so the tool does not constrain
// the import graph; the cross-package interop test proves they agree.
var providers = map[string]bool{
	"google":    true,
	"microsoft": true,
	"slack":     true,
	"atlassian": true,
}

// authCode is the server-side record created at /authorize and consumed
// (single-use) at /token. It binds the PKCE challenge, redirect_uri, provider,
// and subject so the token exchange can validate them.
type authCode struct {
	provider      string
	codeChallenge string
	redirectURI   string
	subject       string
	expiresAt     time.Time
}

// Server is the fake OAuth HTTP server. It implements [http.Handler] and is
// safe for concurrent use.
type Server struct {
	log            *slog.Logger
	now            func() time.Time
	defaultSubject string
	codeTTL        time.Duration
	mux            *http.ServeMux

	mu    sync.Mutex
	codes map[string]authCode // pending authorization codes
}

// Option customizes a [Server].
type Option func(*Server)

// WithLogger sets the logger (default: [slog.Default]).
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.log = l
		}
	}
}

// WithDefaultSubject sets the subject auto-consent uses absent a login_hint.
func WithDefaultSubject(subject string) Option {
	return func(s *Server) {
		if subject != "" {
			s.defaultSubject = subject
		}
	}
}

// WithCodeTTL sets the authorization-code lifetime.
func WithCodeTTL(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.codeTTL = d
		}
	}
}

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option {
	return func(s *Server) {
		if now != nil {
			s.now = now
		}
	}
}

// New constructs a ready-to-serve fake OAuth server.
func New(opts ...Option) *Server {
	s := &Server{
		log:            slog.Default(),
		now:            time.Now,
		defaultSubject: defaultSubject,
		codeTTL:        defaultCodeTTL,
		codes:          make(map[string]authCode),
	}
	for _, opt := range opts {
		opt(s)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{provider}/authorize", s.handleAuthorize)
	mux.HandleFunc("POST /{provider}/token", s.handleToken)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, "ok")
	})
	s.mux = mux
	return s
}

// ServeHTTP implements [http.Handler].
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleAuthorize auto-consents (dev): it validates the provider and
// redirect_uri, mints a one-time code bound to the PKCE challenge, and
// 302-redirects the browser back to redirect_uri with code+state. There is no
// real login UI; the subject comes from login_hint or the configured default.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !providers[provider] {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}

	q := r.URL.Query()
	redirectURI := q.Get("redirect_uri")
	// The redirect target must be a valid absolute URL we can append to; an
	// invalid one is a client error (and would otherwise be an open redirect
	// risk if we blindly forwarded it).
	redir, err := url.Parse(redirectURI)
	if err != nil || redirectURI == "" || !redir.IsAbs() {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}

	challenge := q.Get("code_challenge")
	method := q.Get("code_challenge_method")
	// PKCE S256 is mandatory in Asker's flow; reject anything else rather than
	// silently downgrading.
	if challenge == "" || method != "S256" {
		http.Error(w, "code_challenge with S256 required", http.StatusBadRequest)
		return
	}

	subject := s.defaultSubject
	if hint := q.Get("login_hint"); hint != "" {
		subject = hint
	}

	code, err := randToken(codeBytes)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.codes[code] = authCode{
		provider:      provider,
		codeChallenge: challenge,
		redirectURI:   redirectURI,
		subject:       subject,
		expiresAt:     s.now().Add(s.codeTTL),
	}
	s.mu.Unlock()

	rq := redir.Query()
	rq.Set("code", code)
	if state := q.Get("state"); state != "" {
		rq.Set("state", state)
	}
	redir.RawQuery = rq.Encode()

	s.log.Info("fake-oauth authorize: auto-consent",
		"provider", provider, "subject", subject) // never log code/state secrets
	http.Redirect(w, r, redir.String(), http.StatusFound)
}

// handleToken services the token endpoint for both the authorization_code and
// refresh_token grants. Standard providers get RFC 6749 JSON; Slack gets its
// non-standard envelope. The body is size-bounded and never panics on bad input.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !providers[provider] {
		writeTokenError(w, provider, http.StatusNotFound, "invalid_request")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		writeTokenError(w, provider, http.StatusBadRequest, "invalid_request")
		return
	}

	// Slack's oauth.v2.access does NOT send grant_type (platform/oauth's
	// exchangeSlack posts only code+redirect_uri+code_verifier), so infer the
	// authorization-code grant from the presence of a code for Slack.
	grant := r.Form.Get("grant_type")
	if grant == "" && provider == "slack" && r.Form.Get("code") != "" {
		grant = "authorization_code"
	}

	switch grant {
	case "authorization_code":
		s.handleAuthCodeGrant(w, r, provider)
	case "refresh_token":
		s.handleRefreshGrant(w, r, provider)
	default:
		writeTokenError(w, provider, http.StatusBadRequest, "unsupported_grant_type")
	}
}

// handleAuthCodeGrant validates the one-time code, the PKCE verifier (S256),
// and redirect_uri, then issues the token set. The code is deleted on lookup so
// it is strictly single-use.
func (s *Server) handleAuthCodeGrant(w http.ResponseWriter, r *http.Request, provider string) {
	code := r.Form.Get("code")
	verifier := r.Form.Get("code_verifier")
	redirectURI := r.Form.Get("redirect_uri")

	s.mu.Lock()
	rec, ok := s.codes[code]
	if ok {
		delete(s.codes, code) // single-use: consume on lookup regardless of outcome
	}
	s.mu.Unlock()

	if !ok || code == "" {
		writeTokenError(w, provider, http.StatusBadRequest, "invalid_grant")
		return
	}
	if s.now().After(rec.expiresAt) {
		writeTokenError(w, provider, http.StatusBadRequest, "invalid_grant")
		return
	}
	if rec.provider != provider {
		writeTokenError(w, provider, http.StatusBadRequest, "invalid_grant")
		return
	}
	if rec.redirectURI != redirectURI {
		writeTokenError(w, provider, http.StatusBadRequest, "invalid_grant")
		return
	}
	if !verifyPKCE(verifier, rec.codeChallenge) {
		writeTokenError(w, provider, http.StatusBadRequest, "invalid_grant")
		return
	}

	s.issueTokens(w, provider, rec.subject)
}

// handleRefreshGrant issues a fresh access token for a refresh grant. The fake
// does not persist refresh state (the subject is encoded in the supplied
// refresh token), so any well-formed refresh token rotates the access token.
func (s *Server) handleRefreshGrant(w http.ResponseWriter, r *http.Request, provider string) {
	refresh := r.Form.Get("refresh_token")
	subject := subjectFromRefresh(refresh)
	if subject == "" {
		writeTokenError(w, provider, http.StatusBadRequest, "invalid_grant")
		return
	}
	s.issueTokens(w, provider, subject)
}

// issueTokens mints and writes the token response for provider+subject in the
// shape platform/oauth expects: Slack's non-standard envelope, everyone else's
// standard JSON. A new refresh token is always returned so a subsequent refresh
// keeps working.
func (s *Server) issueTokens(w http.ResponseWriter, provider, subject string) {
	access := accessToken(provider, subject)
	refresh := refreshToken(provider, subject)
	expiresIn := int64(accessTokenTTL / time.Second)

	if provider == "slack" {
		// Slack v2 user-token install: ok + nested authed_user.
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true,
			"authed_user": map[string]any{
				"access_token":  access,
				"refresh_token": refresh,
				"token_type":    "bearer",
				"expires_in":    expiresIn,
				"scope":         "channels:history,channels:read,users:read",
			},
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		"token_type":    "Bearer",
		"expires_in":    expiresIn,
		"scope":         "openid",
	})
}

// accessToken builds the issued access token. Google uses the fake-gmail dev
// shim form so the Gmail connector's downstream call works; others get a
// distinct opaque token.
func accessToken(provider, subject string) string {
	if provider == "google" {
		return gmailTokenPrefix + subject
	}
	return "fakeoauth:" + provider + ":" + subject
}

// refreshToken builds an opaque refresh token that encodes the subject so a
// later refresh grant can re-mint the access token without server-side state.
func refreshToken(provider, subject string) string {
	return "fakeoauth-refresh:" + provider + ":" + subject
}

// subjectFromRefresh extracts the subject from a refresh token this server
// minted. It returns "" for anything it did not issue.
func subjectFromRefresh(refresh string) string {
	const prefix = "fakeoauth-refresh:"
	rest, ok := strings.CutPrefix(refresh, prefix)
	if !ok {
		return ""
	}
	// rest is "<provider>:<subject>"; the subject may itself contain ':'
	// only via an email, which it does not, but split on the first ':'.
	_, subject, ok := strings.Cut(rest, ":")
	if !ok || subject == "" {
		return ""
	}
	return subject
}

// verifyPKCE reports whether verifier hashes (S256, base64url no padding) to
// the recorded challenge. An empty verifier or challenge never matches.
func verifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtleEqual(got, challenge)
}

// subtleEqual is a length-checked constant-time string compare.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// randToken returns n random bytes base64url-encoded (no padding).
func randToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeTokenError writes a grant error in the shape the provider expects:
// Slack signals failure with HTTP 200 + {"ok":false,...}; everyone else uses
// the RFC 6749 4xx + {"error":...} form. The error code is a fixed, non-secret
// token.
func writeTokenError(w http.ResponseWriter, provider string, status int, code string) {
	if provider == "slack" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": slackErrorCode(code)})
		return
	}
	writeJSON(w, status, map[string]any{"error": code})
}

// slackErrorCode maps a standard OAuth error code to Slack's vocabulary for the
// cases this fake produces.
func slackErrorCode(code string) string {
	if code == "invalid_grant" {
		return "invalid_code"
	}
	return code
}

// DefaultSubject reports the subject auto-consent will use absent a login_hint.
// Exposed so the command can log the effective configuration at startup.
func (s *Server) DefaultSubject() string { return s.defaultSubject }
