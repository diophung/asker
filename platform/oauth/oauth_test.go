package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --- Provider / ConnectorOAuth ------------------------------------------------

func TestProvidersAndParse(t *testing.T) {
	got := Providers()
	want := []Provider{Google, Microsoft, Slack, Atlassian}
	if len(got) != len(want) {
		t.Fatalf("Providers() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Providers()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Mutating the returned slice must not affect a later call.
	got[0] = "tampered"
	if Providers()[0] != Google {
		t.Error("Providers() returned a shared backing slice")
	}

	for _, p := range want {
		gp, ok := ParseProvider(string(p))
		if !ok || gp != p {
			t.Errorf("ParseProvider(%q) = %q,%v", p, gp, ok)
		}
		if !p.Valid() {
			t.Errorf("%q.Valid() = false", p)
		}
		if p.String() != string(p) {
			t.Errorf("%q.String() mismatch", p)
		}
	}
	if _, ok := ParseProvider("nope"); ok {
		t.Error("ParseProvider(nope) ok = true")
	}
	if Provider("nope").Valid() {
		t.Error("Provider(nope).Valid() = true")
	}
}

func TestConnectorOAuth(t *testing.T) {
	cases := []struct {
		connector string
		provider  Provider
		scopes    []string
	}{
		{"gmail", Google, []string{"https://www.googleapis.com/auth/gmail.readonly"}},
		{"gcal", Google, []string{"https://www.googleapis.com/auth/calendar.readonly"}},
		{"gdrive", Google, []string{"https://www.googleapis.com/auth/drive.readonly"}},
		{"outlook-mail", Microsoft, []string{"Mail.Read", "offline_access"}},
		{"outlook-cal", Microsoft, []string{"Calendars.Read", "offline_access"}},
		{"msteams", Microsoft, []string{"Chat.Read", "ChannelMessage.Read.All", "offline_access"}},
		{"slack", Slack, []string{"channels:history", "channels:read", "users:read"}},
		{"jira", Atlassian, []string{"read:jira-work", "offline_access"}},
		{"confluence", Atlassian, []string{"read:confluence-content.all", "offline_access"}},
	}
	for _, c := range cases {
		t.Run(c.connector, func(t *testing.T) {
			p, scopes, ok := ConnectorOAuth(c.connector)
			if !ok {
				t.Fatalf("ConnectorOAuth(%q) ok = false", c.connector)
			}
			if p != c.provider {
				t.Errorf("provider = %q, want %q", p, c.provider)
			}
			if strings.Join(scopes, " ") != strings.Join(c.scopes, " ") {
				t.Errorf("scopes = %v, want %v", scopes, c.scopes)
			}
			// Returned slice must be a copy.
			if len(scopes) > 0 {
				scopes[0] = "tampered"
				_, again, _ := ConnectorOAuth(c.connector)
				if again[0] == "tampered" {
					t.Error("ConnectorOAuth returned shared scope slice")
				}
			}
		})
	}

	for _, bad := range []string{"upload", "", "unknown-connector"} {
		if p, scopes, ok := ConnectorOAuth(bad); ok || p != "" || scopes != nil {
			t.Errorf("ConnectorOAuth(%q) = %q,%v,%v; want ok=false", bad, p, scopes, ok)
		}
	}
}

// --- Token: Marshal/Parse/NeedsRefresh ---------------------------------------

func TestMarshalParseRoundTrip(t *testing.T) {
	tok := Token{
		Provider:     Google,
		ConnectorID:  "gmail",
		AccessToken:  "access-xyz",
		RefreshToken: "refresh-abc",
		TokenType:    "Bearer",
		Expiry:       time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		Scope:        "https://www.googleapis.com/auth/gmail.readonly",
	}
	b, err := Marshal(tok)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// The marker must be present and compact (no indentation).
	if !strings.Contains(string(b), `"asker_oauth":"v1"`) {
		t.Errorf("marshaled blob lacks marker: %s", b)
	}
	if strings.Contains(string(b), "\n") {
		t.Errorf("Marshal not compact: %s", b)
	}

	got, ok := Parse(b)
	if !ok {
		t.Fatal("Parse ok = false on our own blob")
	}
	got.AskerOAuth = "" // not part of the logical value
	want := tok
	if got != want {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestMarshalSetsMarkerWhenUnset(t *testing.T) {
	b, err := Marshal(Token{AccessToken: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Parse(b); !ok {
		t.Error("Marshal did not set the version marker")
	}
}

func TestParseRejectsLegacyAndUnmarked(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"legacy non-json", []byte("fake-gmail-token:alice:12345")},
		{"empty", []byte("")},
		{"json missing marker", mustJSON(t, map[string]string{"access_token": "x"})},
		{"json wrong marker", mustJSON(t, map[string]string{"asker_oauth": "v0", "access_token": "x"})},
		{"json array", []byte("[1,2,3]")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if tok, ok := Parse(c.in); ok {
				t.Errorf("Parse(%q) ok = true, tok=%+v; want false", c.in, tok)
			}
		})
	}
}

func TestNeedsRefresh(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	skew := time.Minute
	cases := []struct {
		name string
		tok  Token
		want bool
	}{
		{"expired with refresh", Token{RefreshToken: "r", Expiry: now.Add(-time.Hour)}, true},
		{"within skew with refresh", Token{RefreshToken: "r", Expiry: now.Add(30 * time.Second)}, true},
		{"exactly at skew boundary", Token{RefreshToken: "r", Expiry: now.Add(skew)}, true},
		{"fresh with refresh", Token{RefreshToken: "r", Expiry: now.Add(time.Hour)}, false},
		{"expired no refresh", Token{Expiry: now.Add(-time.Hour)}, false},
		{"no expiry with refresh", Token{RefreshToken: "r"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.tok.NeedsRefresh(now, skew); got != c.want {
				t.Errorf("NeedsRefresh = %v, want %v", got, c.want)
			}
		})
	}
}

func TestOAuth2TokenConversion(t *testing.T) {
	tok := Token{AccessToken: "a", RefreshToken: "r", TokenType: "Bearer", Expiry: time.Unix(1000, 0)}
	o := tok.OAuth2Token()
	if o.AccessToken != "a" || o.RefreshToken != "r" || o.TokenType != "Bearer" || !o.Expiry.Equal(time.Unix(1000, 0)) {
		t.Errorf("OAuth2Token mismatch: %+v", o)
	}
}

// --- Config / LoadConfig / IsConfigured --------------------------------------

func TestLoadConfigDefaults(t *testing.T) {
	// Ensure a clean environment for every relevant var.
	clearOAuthEnv(t)

	cfg := LoadConfig()
	if cfg.Google.AuthURL != defaultGoogleAuthURL || cfg.Google.TokenURL != defaultGoogleTokenURL {
		t.Errorf("google defaults wrong: %+v", cfg.Google)
	}
	if cfg.Microsoft.Tenant != "common" {
		t.Errorf("microsoft tenant default = %q", cfg.Microsoft.Tenant)
	}
	if !strings.Contains(cfg.Microsoft.AuthURL, "/common/") {
		t.Errorf("microsoft auth url missing tenant: %q", cfg.Microsoft.AuthURL)
	}
	if cfg.Atlassian.Audience != "api.atlassian.com" {
		t.Errorf("atlassian audience default = %q", cfg.Atlassian.Audience)
	}
	if cfg.Slack.TokenURL != defaultSlackTokenURL {
		t.Errorf("slack token url default = %q", cfg.Slack.TokenURL)
	}
	// No creds set -> nothing configured.
	for _, p := range Providers() {
		if cfg.IsConfigured(p) {
			t.Errorf("%q reported configured with no creds", p)
		}
	}
	if cfg.IsConfigured(Provider("bogus")) {
		t.Error("bogus provider reported configured")
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	clearOAuthEnv(t)
	t.Setenv("ASKER_OAUTH_GOOGLE_CLIENT_ID", "gid")
	t.Setenv("ASKER_OAUTH_GOOGLE_CLIENT_SECRET", "gsecret")
	t.Setenv("ASKER_OAUTH_GOOGLE_AUTH_URL", "http://localhost/auth")
	t.Setenv("ASKER_OAUTH_GOOGLE_TOKEN_URL", "http://localhost/token")
	t.Setenv("ASKER_OAUTH_MICROSOFT_TENANT", "my-tenant")
	t.Setenv("ASKER_OAUTH_ATLASSIAN_AUDIENCE", "custom.audience")
	t.Setenv("ASKER_OAUTH_SLACK_CLIENT_ID", "sid")
	t.Setenv("ASKER_OAUTH_SLACK_CLIENT_SECRET", "ssecret")

	cfg := LoadConfig()
	if cfg.Google.ClientID != "gid" || cfg.Google.ClientSecret != "gsecret" {
		t.Errorf("google creds not loaded: %+v", cfg.Google)
	}
	if cfg.Google.AuthURL != "http://localhost/auth" || cfg.Google.TokenURL != "http://localhost/token" {
		t.Errorf("google url overrides not applied: %+v", cfg.Google)
	}
	if cfg.Microsoft.Tenant != "my-tenant" || !strings.Contains(cfg.Microsoft.AuthURL, "/my-tenant/") {
		t.Errorf("microsoft tenant override not applied: %+v", cfg.Microsoft)
	}
	if cfg.Atlassian.Audience != "custom.audience" {
		t.Errorf("atlassian audience override not applied: %q", cfg.Atlassian.Audience)
	}
	if !cfg.IsConfigured(Google) || !cfg.IsConfigured(Slack) {
		t.Error("configured providers reported unconfigured")
	}
	if cfg.IsConfigured(Microsoft) {
		t.Error("microsoft reported configured without creds")
	}
}

// --- Service: AuthCodeURL ----------------------------------------------------

func testConfig(authURL, tokenURL string) Config {
	cfg := Config{
		Google:    ProviderConfig{ClientID: "g-id", ClientSecret: "g-sec", AuthURL: authURL, TokenURL: tokenURL},
		Microsoft: ProviderConfig{ClientID: "m-id", ClientSecret: "m-sec", AuthURL: authURL, TokenURL: tokenURL, Tenant: "common"},
		Slack:     ProviderConfig{ClientID: "s-id", ClientSecret: "s-sec", AuthURL: authURL, TokenURL: tokenURL},
		Atlassian: ProviderConfig{ClientID: "a-id", ClientSecret: "a-sec", AuthURL: authURL, TokenURL: tokenURL, Audience: "api.atlassian.com"},
	}
	return cfg
}

func TestAuthCodeURL(t *testing.T) {
	svc := New(testConfig("https://provider.example/authorize", "https://provider.example/token"), http.DefaultClient)
	const (
		redirect  = "https://asker.example/callback"
		state     = "state-123"
		challenge = "challenge-s256-value"
	)

	cases := []struct {
		provider  Provider
		scopes    []string
		wantQuery map[string]string // exact param == value
		wantHas   []string          // params that must be present (non-empty)
		scopeKey  string            // which param carries scopes
		scopeVal  string
	}{
		{
			provider: Google,
			scopes:   []string{"https://www.googleapis.com/auth/gmail.readonly"},
			wantQuery: map[string]string{
				"response_type":         "code",
				"access_type":           "offline",
				"prompt":                "consent",
				"code_challenge":        challenge,
				"code_challenge_method": "S256",
				"state":                 state,
			},
			scopeKey: "scope",
			scopeVal: "https://www.googleapis.com/auth/gmail.readonly",
		},
		{
			provider: Atlassian,
			scopes:   []string{"read:jira-work", "offline_access"},
			wantQuery: map[string]string{
				"response_type":  "code",
				"audience":       "api.atlassian.com",
				"prompt":         "consent",
				"code_challenge": challenge,
			},
			scopeKey: "scope",
			scopeVal: "read:jira-work offline_access",
		},
		{
			provider: Microsoft,
			scopes:   []string{"Mail.Read", "offline_access"},
			wantQuery: map[string]string{
				"response_type":         "code",
				"code_challenge_method": "S256",
			},
			scopeKey: "scope",
			scopeVal: "Mail.Read offline_access",
		},
		{
			provider: Slack,
			scopes:   []string{"channels:history", "channels:read"},
			wantQuery: map[string]string{
				"response_type":  "code",
				"code_challenge": challenge,
			},
			scopeKey: "user_scope",
			scopeVal: "channels:history,channels:read",
		},
	}

	for _, c := range cases {
		t.Run(string(c.provider), func(t *testing.T) {
			raw, err := svc.AuthCodeURL(c.provider, c.scopes, redirect, state, challenge)
			if err != nil {
				t.Fatalf("AuthCodeURL: %v", err)
			}
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parse url: %v", err)
			}
			q := u.Query()
			for k, v := range c.wantQuery {
				if q.Get(k) != v {
					t.Errorf("param %q = %q, want %q (url=%s)", k, q.Get(k), v, raw)
				}
			}
			if got := q.Get(c.scopeKey); got != c.scopeVal {
				t.Errorf("scope param %q = %q, want %q", c.scopeKey, got, c.scopeVal)
			}
			if q.Get("client_id") == "" {
				t.Error("client_id missing")
			}
			if q.Get("redirect_uri") != redirect {
				t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
			}
			// Slack must NOT carry a standard scope param.
			if c.provider == Slack && q.Get("scope") != "" {
				t.Errorf("slack emitted standard scope param: %q", q.Get("scope"))
			}
		})
	}
}

func TestAuthCodeURLErrors(t *testing.T) {
	// Unknown provider.
	svc := New(Config{}, nil)
	if _, err := svc.AuthCodeURL(Provider("nope"), nil, "r", "s", "c"); err == nil {
		t.Error("expected error for unknown provider")
	}
	// Configured-but-missing-creds: empty Config -> google unconfigured.
	if _, err := svc.AuthCodeURL(Google, nil, "r", "s", "c"); err == nil {
		t.Error("expected error for unconfigured provider")
	}
}

// --- Service: Exchange (standard + Slack) ------------------------------------

func TestExchangeStandard(t *testing.T) {
	srv := standardTokenServer(t, tokenReply{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		Scope:        "Mail.Read offline_access",
	})
	defer srv.Close()

	svc := New(testConfig(srv.URL+"/authorize", srv.URL+"/token"), srv.Client())
	tok, err := svc.Exchange(context.Background(), Microsoft, "the-code", "https://asker.example/cb", "verifier-123")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != "new-access" || tok.RefreshToken != "new-refresh" {
		t.Errorf("tokens wrong: %+v", tok)
	}
	if tok.Provider != Microsoft || tok.AskerOAuth != "v1" {
		t.Errorf("metadata wrong: %+v", tok)
	}
	if tok.Expiry.IsZero() || time.Until(tok.Expiry) <= 0 {
		t.Errorf("expiry not set in the future: %v", tok.Expiry)
	}
}

func TestExchangeSendsPKCEVerifier(t *testing.T) {
	var gotVerifier string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotVerifier = r.Form.Get("code_verifier")
		writeJSON(t, w, tokenReply{AccessToken: "a", TokenType: "Bearer", ExpiresIn: 60})
	}))
	defer srv.Close()

	svc := New(testConfig(srv.URL, srv.URL), srv.Client())
	if _, err := svc.Exchange(context.Background(), Google, "code", "cb", "my-verifier"); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if gotVerifier != "my-verifier" {
		t.Errorf("code_verifier = %q, want my-verifier", gotVerifier)
	}
}

func TestExchangeSlack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code_verifier") != "v" {
			t.Errorf("slack missing code_verifier: %q", r.Form.Get("code_verifier"))
		}
		// Slack-shaped reply: ok + nested authed_user.
		body := map[string]any{
			"ok":         true,
			"token_type": "bot-ignored",
			"authed_user": map[string]any{
				"access_token": "xoxp-user-token",
				"token_type":   "Bearer",
				"scope":        "channels:history,channels:read",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	svc := New(testConfig(srv.URL, srv.URL), srv.Client())
	tok, err := svc.Exchange(context.Background(), Slack, "code", "cb", "v")
	if err != nil {
		t.Fatalf("slack Exchange: %v", err)
	}
	if tok.AccessToken != "xoxp-user-token" {
		t.Errorf("slack access token = %q", tok.AccessToken)
	}
	if tok.Provider != Slack || tok.Scope != "channels:history,channels:read" {
		t.Errorf("slack token metadata wrong: %+v", tok)
	}
	if !tok.Expiry.IsZero() {
		t.Errorf("slack non-expiring token got expiry: %v", tok.Expiry)
	}
}

func TestExchangeSlackError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_code"})
	}))
	defer srv.Close()

	svc := New(testConfig(srv.URL, srv.URL), srv.Client())
	_, err := svc.Exchange(context.Background(), Slack, "bad", "cb", "v")
	if err == nil || !strings.Contains(err.Error(), "invalid_code") {
		t.Fatalf("expected slack error with code, got %v", err)
	}
}

func TestExchangeSlackEmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "authed_user": map[string]any{}})
	}))
	defer srv.Close()
	svc := New(testConfig(srv.URL, srv.URL), srv.Client())
	if _, err := svc.Exchange(context.Background(), Slack, "c", "cb", "v"); err == nil {
		t.Error("expected error for empty slack user token")
	}
}

func TestExchangeUnconfigured(t *testing.T) {
	svc := New(Config{}, nil)
	if _, err := svc.Exchange(context.Background(), Google, "c", "cb", "v"); err == nil {
		t.Error("expected error exchanging on unconfigured provider")
	}
	if _, err := svc.Exchange(context.Background(), Slack, "c", "cb", "v"); err == nil {
		t.Error("expected error exchanging on unconfigured slack")
	}
}

// --- Service: Refresh --------------------------------------------------------

func TestRefreshStandard(t *testing.T) {
	srv := standardTokenServer(t, tokenReply{
		AccessToken: "refreshed-access",
		TokenType:   "Bearer",
		ExpiresIn:   3600,
		// Note: no refresh_token in reply -> must carry forward the old one.
	})
	defer srv.Close()

	svc := New(testConfig(srv.URL, srv.URL), srv.Client())
	old := Token{
		Provider:     Google,
		ConnectorID:  "gmail",
		AccessToken:  "stale",
		RefreshToken: "keep-this-refresh",
		Expiry:       time.Now().Add(-time.Hour), // forces a refresh
	}
	got, err := svc.Refresh(context.Background(), Google, old)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.AccessToken != "refreshed-access" {
		t.Errorf("access token not refreshed: %+v", got)
	}
	if got.RefreshToken != "keep-this-refresh" {
		t.Errorf("refresh token not carried forward: %q", got.RefreshToken)
	}
	if got.ConnectorID != "gmail" {
		t.Errorf("connector id lost: %q", got.ConnectorID)
	}
}

func TestRefreshRotatesRefreshToken(t *testing.T) {
	srv := standardTokenServer(t, tokenReply{
		AccessToken:  "refreshed-access",
		RefreshToken: "rotated-refresh",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
	})
	defer srv.Close()

	svc := New(testConfig(srv.URL, srv.URL), srv.Client())
	old := Token{Provider: Microsoft, AccessToken: "stale", RefreshToken: "old", Expiry: time.Now().Add(-time.Hour)}
	got, err := svc.Refresh(context.Background(), Microsoft, old)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.RefreshToken != "rotated-refresh" {
		t.Errorf("rotated refresh token not applied: %q", got.RefreshToken)
	}
}

func TestRefreshNoRefreshTokenIsNoop(t *testing.T) {
	// Slack user token: non-refreshable, no refresh token -> returned unchanged.
	svc := New(testConfig("https://x/authorize", "https://x/token"), http.DefaultClient)
	tok := Token{Provider: Slack, AccessToken: "xoxp-token"}
	got, err := svc.Refresh(context.Background(), Slack, tok)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.AccessToken != "xoxp-token" {
		t.Errorf("noop refresh changed token: %+v", got)
	}
}

// TestRefreshSlackRotates covers a rotation-enabled Slack user token: the
// refresh grant returns Slack's non-standard authed_user envelope (which
// x/oauth2 cannot parse), so Refresh must use the hand-rolled Slack branch.
func TestRefreshSlackRotates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("slack refresh grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("refresh_token") != "old-refresh" {
			t.Errorf("slack refresh_token = %q", r.Form.Get("refresh_token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"authed_user": map[string]any{
				"access_token":  "xoxp-rotated",
				"refresh_token": "rotated-refresh",
				"token_type":    "Bearer",
				"expires_in":    3600,
				"scope":         "channels:history",
			},
		})
	}))
	defer srv.Close()

	svc := New(testConfig(srv.URL, srv.URL), srv.Client())
	old := Token{
		Provider:     Slack,
		ConnectorID:  "slack",
		AccessToken:  "stale",
		RefreshToken: "old-refresh",
		Expiry:       time.Now().Add(-time.Hour), // forces a refresh
	}
	got, err := svc.Refresh(context.Background(), Slack, old)
	if err != nil {
		t.Fatalf("slack Refresh: %v", err)
	}
	if got.AccessToken != "xoxp-rotated" {
		t.Errorf("slack access token not refreshed: %+v", got)
	}
	if got.RefreshToken != "rotated-refresh" {
		t.Errorf("slack rotated refresh token not applied: %q", got.RefreshToken)
	}
	if got.ConnectorID != "slack" {
		t.Errorf("connector id lost on slack refresh: %q", got.ConnectorID)
	}
	if got.Expiry.IsZero() {
		t.Error("slack refresh dropped expiry")
	}
}

// TestRefreshSlackCarriesForward covers a Slack refresh reply that omits the
// rotated refresh token / expiry / scope: the prior values must be preserved so
// the connector keeps refreshing instead of breaking permanently.
func TestRefreshSlackCarriesForward(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"authed_user": map[string]any{
				"access_token": "xoxp-rotated",
				// no refresh_token, expires_in, or scope in this reply
			},
		})
	}))
	defer srv.Close()

	svc := New(testConfig(srv.URL, srv.URL), srv.Client())
	expiry := time.Now().Add(-time.Hour)
	old := Token{
		Provider:     Slack,
		ConnectorID:  "slack",
		AccessToken:  "stale",
		RefreshToken: "keep-refresh",
		Expiry:       expiry,
		Scope:        "channels:history",
	}
	got, err := svc.Refresh(context.Background(), Slack, old)
	if err != nil {
		t.Fatalf("slack Refresh: %v", err)
	}
	if got.AccessToken != "xoxp-rotated" {
		t.Errorf("slack access token not refreshed: %+v", got)
	}
	if got.RefreshToken != "keep-refresh" {
		t.Errorf("slack refresh token not carried forward: %q", got.RefreshToken)
	}
	if !got.Expiry.Equal(expiry) {
		t.Errorf("slack expiry not carried forward: got %v want %v", got.Expiry, expiry)
	}
	if got.Scope != "channels:history" {
		t.Errorf("slack scope not carried forward: %q", got.Scope)
	}
}

// TestRefreshSlackError surfaces Slack's ok:false envelope (HTTP 200) as an
// error so a refresh failure fails the run instead of yielding an empty bearer.
func TestRefreshSlackError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_refresh_token"})
	}))
	defer srv.Close()

	svc := New(testConfig(srv.URL, srv.URL), srv.Client())
	old := Token{Provider: Slack, AccessToken: "stale", RefreshToken: "bad", Expiry: time.Now().Add(-time.Hour)}
	_, err := svc.Refresh(context.Background(), Slack, old)
	if err == nil || !strings.Contains(err.Error(), "invalid_refresh_token") {
		t.Fatalf("expected slack refresh error with code, got %v", err)
	}
}

func TestRefreshUnconfigured(t *testing.T) {
	svc := New(Config{}, nil)
	if _, err := svc.Refresh(context.Background(), Google, Token{RefreshToken: "r"}); err == nil {
		t.Error("expected error refreshing on unconfigured provider")
	}
	if _, err := svc.Refresh(context.Background(), Provider("nope"), Token{RefreshToken: "r"}); err == nil {
		t.Error("expected error refreshing on unknown provider")
	}
}

func TestNewDefaultsHTTPClient(t *testing.T) {
	svc := New(Config{}, nil)
	if svc.httpClient == nil || svc.httpClient.Timeout != defaultHTTPTimeout {
		t.Errorf("New(nil) did not set a default client: %+v", svc.httpClient)
	}
}

// --- helpers -----------------------------------------------------------------

type tokenReply struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// standardTokenServer returns an httptest server whose /token (or any path)
// replies with the given standard OAuth2 token JSON.
func standardTokenServer(t *testing.T, reply tokenReply) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, reply)
	}))
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode reply: %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// clearOAuthEnv unsets every OAuth env var this package reads, restoring them
// after the test via t.Setenv semantics (Setenv records the prior value).
func clearOAuthEnv(t *testing.T) {
	t.Helper()
	vars := []string{
		"ASKER_OAUTH_MICROSOFT_TENANT",
		"ASKER_OAUTH_ATLASSIAN_AUDIENCE",
	}
	for _, p := range Providers() {
		up := strings.ToUpper(string(p))
		vars = append(vars,
			"ASKER_OAUTH_"+up+"_CLIENT_ID",
			"ASKER_OAUTH_"+up+"_CLIENT_SECRET",
			"ASKER_OAUTH_"+up+"_AUTH_URL",
			"ASKER_OAUTH_"+up+"_TOKEN_URL",
		)
	}
	for _, v := range vars {
		t.Setenv(v, "")
	}
}
