package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/asker/asker/platform/oauth"
	"golang.org/x/oauth2"
)

// newTestServer returns a fake-oauth server fronted by httptest. The default
// subject is fixed so assertions are deterministic.
func newTestServer(t *testing.T, opts ...Option) *httptest.Server {
	t.Helper()
	base := []Option{
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithDefaultSubject("alice@example.com"),
	}
	srv := httptest.NewServer(New(append(base, opts...)...))
	t.Cleanup(srv.Close)
	return srv
}

// pkcePair returns a PKCE verifier and its S256 base64url challenge, generated
// exactly the way the gateway caller will (x/oauth2 helpers).
func pkcePair() (verifier, challenge string) {
	verifier = oauth2.GenerateVerifier()
	return verifier, oauth2.S256ChallengeFromVerifier(verifier)
}

// s256 computes the raw-base64url S256 challenge for v (independent of x/oauth2
// so the test does not assume the library's encoding).
func s256(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizeNoRedirectClient is an HTTP client that does NOT follow redirects, so
// a test can inspect the 302 the authorize endpoint emits.
func authorizeNoRedirectClient(srv *httptest.Server) *http.Client {
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// doAuthorize hits /{provider}/authorize and returns the code+state parsed from
// the Location header (or fails the test).
func doAuthorize(t *testing.T, srv *httptest.Server, provider, challenge, redirectURI, state, loginHint string) (code, gotState string) {
	t.Helper()
	q := url.Values{
		"client_id":             {provider + "-id"},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"read"},
	}
	if loginHint != "" {
		q.Set("login_hint", loginHint)
	}
	resp, err := authorizeNoRedirectClient(srv).Get(srv.URL + "/" + provider + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return loc.Query().Get("code"), loc.Query().Get("state")
}

// postToken posts a form to /{provider}/token and returns the status + decoded
// JSON body.
func postToken(t *testing.T, srv *httptest.Server, provider string, form url.Values) (int, map[string]any) {
	t.Helper()
	resp, err := srv.Client().PostForm(srv.URL+"/"+provider+"/token", form)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode token body %q: %v", body, err)
		}
	}
	return resp.StatusCode, out
}

func TestAuthorizeAndTokenStandardHappyPath(t *testing.T) {
	srv := newTestServer(t)
	verifier, challenge := pkcePair()
	const redirect = "https://asker.example/callback"

	code, state := doAuthorize(t, srv, "microsoft", challenge, redirect, "xyz-state", "")
	if code == "" {
		t.Fatal("no code in redirect")
	}
	if state != "xyz-state" {
		t.Errorf("state = %q, want xyz-state", state)
	}

	status, body := postToken(t, srv, "microsoft", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
		"client_id":     {"microsoft-id"},
	})
	if status != http.StatusOK {
		t.Fatalf("token status = %d, want 200 (body=%v)", status, body)
	}
	if body["access_token"] != "fakeoauth:microsoft:alice@example.com" {
		t.Errorf("access_token = %v", body["access_token"])
	}
	if body["refresh_token"] == nil || body["refresh_token"] == "" {
		t.Error("missing refresh_token")
	}
	if body["token_type"] != "Bearer" {
		t.Errorf("token_type = %v, want Bearer", body["token_type"])
	}
	if body["expires_in"] == nil {
		t.Error("missing expires_in")
	}
}

func TestGoogleIssuesFakeGmailToken(t *testing.T) {
	srv := newTestServer(t)
	verifier, challenge := pkcePair()
	const redirect = "https://asker.example/cb"

	code, _ := doAuthorize(t, srv, "google", challenge, redirect, "s", "bob@example.com")
	status, body := postToken(t, srv, "google", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	// login_hint overrode the default subject and the token uses the gmail shim.
	if body["access_token"] != "fake-gmail-token:bob@example.com" {
		t.Errorf("google access_token = %v, want fake-gmail-token:bob@example.com", body["access_token"])
	}
}

func TestSlackHappyPathShape(t *testing.T) {
	srv := newTestServer(t)
	verifier, challenge := pkcePair()
	const redirect = "https://asker.example/cb"

	code, _ := doAuthorize(t, srv, "slack", challenge, redirect, "s", "")
	status, body := postToken(t, srv, "slack", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	})
	if status != http.StatusOK {
		t.Fatalf("slack status = %d, want 200", status)
	}
	if body["ok"] != true {
		t.Fatalf("slack ok = %v, want true (body=%v)", body["ok"], body)
	}
	au, ok := body["authed_user"].(map[string]any)
	if !ok {
		t.Fatalf("authed_user not an object: %v", body["authed_user"])
	}
	if au["access_token"] != "fakeoauth:slack:alice@example.com" {
		t.Errorf("slack access_token = %v", au["access_token"])
	}
	if au["token_type"] != "bearer" {
		t.Errorf("slack token_type = %v, want bearer", au["token_type"])
	}
}

func TestPKCEMismatchRejected(t *testing.T) {
	srv := newTestServer(t)
	_, challenge := pkcePair()
	const redirect = "https://asker.example/cb"

	code, _ := doAuthorize(t, srv, "google", challenge, redirect, "s", "")
	status, body := postToken(t, srv, "google", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {"the-wrong-verifier"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if body["error"] != "invalid_grant" {
		t.Errorf("error = %v, want invalid_grant", body["error"])
	}
}

func TestSlackPKCEMismatchShape(t *testing.T) {
	srv := newTestServer(t)
	_, challenge := pkcePair()
	const redirect = "https://asker.example/cb"

	code, _ := doAuthorize(t, srv, "slack", challenge, redirect, "s", "")
	status, body := postToken(t, srv, "slack", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {"wrong"},
	})
	// Slack signals failure with HTTP 200 + ok:false.
	if status != http.StatusOK {
		t.Fatalf("slack error status = %d, want 200", status)
	}
	if body["ok"] != false || body["error"] != "invalid_code" {
		t.Errorf("slack error body = %v, want ok:false error:invalid_code", body)
	}
}

func TestCodeIsSingleUse(t *testing.T) {
	srv := newTestServer(t)
	verifier, challenge := pkcePair()
	const redirect = "https://asker.example/cb"

	code, _ := doAuthorize(t, srv, "atlassian", challenge, redirect, "s", "")
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	}
	if status, _ := postToken(t, srv, "atlassian", form); status != http.StatusOK {
		t.Fatalf("first exchange status = %d, want 200", status)
	}
	status, body := postToken(t, srv, "atlassian", form)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("second exchange = %d %v, want 400 invalid_grant", status, body)
	}
}

func TestRefreshIssuesNewToken(t *testing.T) {
	srv := newTestServer(t)
	verifier, challenge := pkcePair()
	const redirect = "https://asker.example/cb"

	code, _ := doAuthorize(t, srv, "microsoft", challenge, redirect, "s", "")
	_, first := postToken(t, srv, "microsoft", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	})
	refresh, _ := first["refresh_token"].(string)
	if refresh == "" {
		t.Fatal("no refresh token issued")
	}

	status, body := postToken(t, srv, "microsoft", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	})
	if status != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200", status)
	}
	if body["access_token"] != "fakeoauth:microsoft:alice@example.com" {
		t.Errorf("refreshed access_token = %v", body["access_token"])
	}
	if body["refresh_token"] == "" || body["refresh_token"] == nil {
		t.Error("refresh did not return a refresh token")
	}
}

func TestRefreshRejectsUnknownToken(t *testing.T) {
	srv := newTestServer(t)
	status, body := postToken(t, srv, "google", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"not-one-we-issued"},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("refresh of bogus token = %d %v, want 400 invalid_grant", status, body)
	}
}

func TestUnknownCodeRejected(t *testing.T) {
	srv := newTestServer(t)
	status, body := postToken(t, srv, "google", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"never-issued"},
		"redirect_uri":  {"https://asker.example/cb"},
		"code_verifier": {"v"},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("unknown code = %d %v, want 400 invalid_grant", status, body)
	}
}

func TestRedirectURIMismatchRejected(t *testing.T) {
	srv := newTestServer(t)
	verifier, challenge := pkcePair()

	code, _ := doAuthorize(t, srv, "google", challenge, "https://asker.example/cb", "s", "")
	status, body := postToken(t, srv, "google", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"https://evil.example/cb"},
		"code_verifier": {verifier},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("redirect mismatch = %d %v, want 400 invalid_grant", status, body)
	}
}

func TestExpiredCodeRejected(t *testing.T) {
	now := time.Now()
	clock := now
	srv := newTestServer(t, WithCodeTTL(time.Minute), WithClock(func() time.Time { return clock }))
	verifier, challenge := pkcePair()
	const redirect = "https://asker.example/cb"

	code, _ := doAuthorize(t, srv, "google", challenge, redirect, "s", "")
	clock = now.Add(2 * time.Minute) // advance past TTL
	status, body := postToken(t, srv, "google", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("expired code = %d %v, want 400 invalid_grant", status, body)
	}
}

func TestAuthorizeRejectsBadInput(t *testing.T) {
	srv := newTestServer(t)
	_, challenge := pkcePair()
	client := authorizeNoRedirectClient(srv)

	cases := []struct {
		name  string
		query url.Values
		path  string
		want  int
	}{
		{
			name: "unknown provider",
			path: "/nope/authorize",
			query: url.Values{
				"redirect_uri": {"https://a/cb"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
			},
			want: http.StatusNotFound,
		},
		{
			name: "missing redirect_uri",
			path: "/google/authorize",
			query: url.Values{
				"code_challenge": {challenge}, "code_challenge_method": {"S256"},
			},
			want: http.StatusBadRequest,
		},
		{
			name: "relative redirect_uri",
			path: "/google/authorize",
			query: url.Values{
				"redirect_uri": {"/local/cb"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
			},
			want: http.StatusBadRequest,
		},
		{
			name: "missing challenge",
			path: "/google/authorize",
			query: url.Values{
				"redirect_uri": {"https://a/cb"}, "code_challenge_method": {"S256"},
			},
			want: http.StatusBadRequest,
		},
		{
			name: "plain method rejected",
			path: "/google/authorize",
			query: url.Values{
				"redirect_uri": {"https://a/cb"}, "code_challenge": {challenge}, "code_challenge_method": {"plain"},
			},
			want: http.StatusBadRequest,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := client.Get(srv.URL + c.path + "?" + c.query.Encode())
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, c.want)
			}
		})
	}
}

func TestTokenRejectsUnknownProviderAndGrant(t *testing.T) {
	srv := newTestServer(t)
	status, body := postToken(t, srv, "nope", url.Values{"grant_type": {"authorization_code"}})
	if status != http.StatusNotFound || body["error"] != "invalid_request" {
		t.Errorf("unknown provider token = %d %v", status, body)
	}

	status, body = postToken(t, srv, "google", url.Values{"grant_type": {"client_credentials"}})
	if status != http.StatusBadRequest || body["error"] != "unsupported_grant_type" {
		t.Errorf("bad grant = %d %v", status, body)
	}
}

func TestTokenBodyTooLargeRejected(t *testing.T) {
	srv := newTestServer(t)
	huge := strings.Repeat("a", maxFormBytes+1024)
	resp, err := srv.Client().Post(srv.URL+"/google/token", "application/x-www-form-urlencoded",
		strings.NewReader("grant_type=authorization_code&code="+huge))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized body status = %d, want 400", resp.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "ok" {
		t.Errorf("healthz body = %q", body)
	}
}

func TestVerifyPKCEUnit(t *testing.T) {
	v := "a-verifier-string-that-is-long-enough-1234567890"
	if !verifyPKCE(v, s256(v)) {
		t.Error("verifyPKCE rejected a matching pair")
	}
	if verifyPKCE(v, s256("other")) {
		t.Error("verifyPKCE accepted a mismatched challenge")
	}
	if verifyPKCE("", s256(v)) || verifyPKCE(v, "") {
		t.Error("verifyPKCE accepted an empty input")
	}
}

func TestDefaultSubjectAccessor(t *testing.T) {
	s := New(WithDefaultSubject("custom@example.com"))
	if s.DefaultSubject() != "custom@example.com" {
		t.Errorf("DefaultSubject = %q", s.DefaultSubject())
	}
	// Empty option is ignored (default retained).
	s2 := New(WithDefaultSubject(""))
	if s2.DefaultSubject() != defaultSubject {
		t.Errorf("empty subject override changed default to %q", s2.DefaultSubject())
	}
}

// --- Interop with the REAL platform/oauth.Service ----------------------------

// fakeOAuthConfig builds an oauth.Config whose four providers point at the fake
// server, with dev client creds so IsConfigured is true.
func fakeOAuthConfig(base string) oauth.Config {
	pc := func(p string) oauth.ProviderConfig {
		return oauth.ProviderConfig{
			ClientID:     p + "-id",
			ClientSecret: p + "-secret",
			AuthURL:      base + "/" + p + "/authorize",
			TokenURL:     base + "/" + p + "/token",
		}
	}
	cfg := oauth.Config{
		Google:    pc("google"),
		Microsoft: pc("microsoft"),
		Slack:     pc("slack"),
		Atlassian: pc("atlassian"),
	}
	cfg.Microsoft.Tenant = "common"
	cfg.Atlassian.Audience = "api.atlassian.com"
	return cfg
}

// extractCodeViaService drives oauth.Service.AuthCodeURL, hits the fake's
// authorize endpoint with the resulting URL, and returns the code from the
// 302 — i.e. exactly what the gateway callback would receive.
func extractCodeViaService(t *testing.T, srv *httptest.Server, svc *oauth.Service, p oauth.Provider, scopes []string, redirect, state, challenge string) string {
	t.Helper()
	authURL, err := svc.AuthCodeURL(p, scopes, redirect, state, challenge)
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	resp, err := authorizeNoRedirectClient(srv).Get(authURL)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if got := loc.Query().Get("state"); got != state {
		t.Errorf("state round-trip = %q, want %q", got, state)
	}
	return loc.Query().Get("code")
}

// TestInteropStandardWithRealService proves the real oauth.Service can run the
// full AuthCodeURL -> Exchange -> Refresh flow against this fake for a standard
// provider, with PKCE generated by x/oauth2 (as the gateway will do).
func TestInteropStandardWithRealService(t *testing.T) {
	srv := newTestServer(t)
	svc := oauth.New(fakeOAuthConfig(srv.URL), srv.Client())

	verifier := oauth2.GenerateVerifier()
	challenge := oauth2.S256ChallengeFromVerifier(verifier)
	const redirect = "https://asker.example/v1/oauth/callback"

	code := extractCodeViaService(t, srv, svc, oauth.Google,
		[]string{"https://www.googleapis.com/auth/gmail.readonly"}, redirect, "state-abc", challenge)
	if code == "" {
		t.Fatal("no code returned")
	}

	tok, err := svc.Exchange(context.Background(), oauth.Google, code, redirect, verifier)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != "fake-gmail-token:alice@example.com" {
		t.Errorf("google access token = %q", tok.AccessToken)
	}
	if tok.RefreshToken == "" {
		t.Fatal("no refresh token from exchange")
	}
	if tok.Provider != oauth.Google || tok.AskerOAuth != "v1" {
		t.Errorf("token metadata wrong: %+v", tok)
	}
	if tok.Expiry.IsZero() || time.Until(tok.Expiry) <= 0 {
		t.Errorf("expiry not in the future: %v", tok.Expiry)
	}

	// The marshaled token must round-trip through the storage contract.
	blob, err := oauth.Marshal(tok)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, ok := oauth.Parse(blob); !ok {
		t.Fatal("Parse rejected our own marshaled token")
	}

	// Refresh via the real service rotates the access token.
	refreshed, err := svc.Refresh(context.Background(), oauth.Google, tok)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.AccessToken == "" {
		t.Fatal("refresh produced empty access token")
	}
	if refreshed.RefreshToken == "" {
		t.Error("refresh dropped the refresh token")
	}
}

// TestInteropSlackWithRealService proves the real oauth.Service handles the
// fake's non-standard Slack envelope end to end.
func TestInteropSlackWithRealService(t *testing.T) {
	srv := newTestServer(t)
	svc := oauth.New(fakeOAuthConfig(srv.URL), srv.Client())

	verifier := oauth2.GenerateVerifier()
	challenge := oauth2.S256ChallengeFromVerifier(verifier)
	const redirect = "https://asker.example/v1/oauth/callback"

	code := extractCodeViaService(t, srv, svc, oauth.Slack,
		[]string{"channels:history", "channels:read"}, redirect, "slack-state", challenge)
	if code == "" {
		t.Fatal("no code returned for slack")
	}

	tok, err := svc.Exchange(context.Background(), oauth.Slack, code, redirect, verifier)
	if err != nil {
		t.Fatalf("slack Exchange: %v", err)
	}
	if tok.AccessToken != "fakeoauth:slack:alice@example.com" {
		t.Errorf("slack access token = %q", tok.AccessToken)
	}
	if tok.Provider != oauth.Slack {
		t.Errorf("provider = %q, want slack", tok.Provider)
	}

	// Slack tokens here carry a refresh token + expiry, so a Refresh succeeds.
	refreshed, err := svc.Refresh(context.Background(), oauth.Slack, tok)
	if err != nil {
		t.Fatalf("slack Refresh: %v", err)
	}
	if refreshed.AccessToken == "" {
		t.Error("slack refresh produced empty token")
	}
}

// TestInteropAllStandardProviders exercises Microsoft and Atlassian through the
// real service too, so every standard provider path is covered.
func TestInteropAllStandardProviders(t *testing.T) {
	srv := newTestServer(t)
	svc := oauth.New(fakeOAuthConfig(srv.URL), srv.Client())
	const redirect = "https://asker.example/v1/oauth/callback"

	for _, p := range []oauth.Provider{oauth.Microsoft, oauth.Atlassian} {
		t.Run(string(p), func(t *testing.T) {
			verifier := oauth2.GenerateVerifier()
			challenge := oauth2.S256ChallengeFromVerifier(verifier)
			code := extractCodeViaService(t, srv, svc, p, []string{"read", "offline_access"}, redirect, "st", challenge)
			tok, err := svc.Exchange(context.Background(), p, code, redirect, verifier)
			if err != nil {
				t.Fatalf("Exchange(%s): %v", p, err)
			}
			want := "fakeoauth:" + string(p) + ":alice@example.com"
			if tok.AccessToken != want {
				t.Errorf("access token = %q, want %q", tok.AccessToken, want)
			}
			if _, err := svc.Refresh(context.Background(), p, tok); err != nil {
				t.Errorf("Refresh(%s): %v", p, err)
			}
		})
	}
}
