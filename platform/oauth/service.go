package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// defaultHTTPTimeout bounds the token-endpoint calls when the caller does not
// inject its own client.
const defaultHTTPTimeout = 30 * time.Second

// Service drives the OAuth2 authorization-code flow (with PKCE), token
// exchange, and refresh for every supported provider. It is safe for
// concurrent use. Construct it with [New].
type Service struct {
	cfg        Config
	httpClient *http.Client
}

// New builds a [Service] from cfg. httpClient is the caller-injected,
// SSRF-safe / timeout-bounded client used for every outbound call; when it is
// nil a 30-second default client is used.
func New(cfg Config, httpClient *http.Client) *Service {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Service{cfg: cfg, httpClient: httpClient}
}

// oauthConfig builds an [*oauth2.Config] for p with the given scopes and
// redirect URI. It returns an error when p is unknown or unconfigured.
func (s *Service) oauthConfig(p Provider, scopes []string, redirectURI string) (*oauth2.Config, *ProviderConfig, error) {
	pc := s.cfg.providerConfig(p)
	if pc == nil {
		return nil, nil, errUnknownProvider(p)
	}
	if pc.ClientID == "" || pc.ClientSecret == "" {
		return nil, nil, errUnconfigured(p)
	}
	return &oauth2.Config{
		ClientID:     pc.ClientID,
		ClientSecret: pc.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  pc.AuthURL,
			TokenURL: pc.TokenURL,
		},
		RedirectURL: redirectURI,
		Scopes:      scopes,
	}, pc, nil
}

// ctx returns a context carrying the Service's HTTP client so x/oauth2 uses it
// for token-endpoint calls.
func (s *Service) clientCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, s.httpClient)
}

// AuthCodeURL builds the provider authorize URL: response_type=code, the
// scopes, state, the PKCE S256 challenge, and the provider-specific params —
// Google adds access_type=offline&prompt=consent (to obtain a refresh token),
// Atlassian adds audience=<audience>&prompt=consent, Slack encodes its scopes
// as user_scope (user-token style) rather than scope. It errors when the
// provider is unknown or unconfigured.
func (s *Service) AuthCodeURL(p Provider, scopes []string, redirectURI, state, pkceChallengeS256 string) (string, error) {
	conf, pc, err := s.oauthConfig(p, scopes, redirectURI)
	if err != nil {
		return "", err
	}

	opts := []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam("code_challenge", pkceChallengeS256),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	}

	switch p {
	case Google:
		opts = append(opts,
			oauth2.SetAuthURLParam("access_type", "offline"),
			oauth2.SetAuthURLParam("prompt", "consent"),
		)
	case Atlassian:
		opts = append(opts,
			oauth2.SetAuthURLParam("audience", pc.Audience),
			oauth2.SetAuthURLParam("prompt", "consent"),
		)
	case Slack:
		// Slack v2 user tokens use user_scope, not the standard scope param;
		// drop the empty scope param x/oauth2 would otherwise emit.
		conf.Scopes = nil
		opts = append(opts, oauth2.SetAuthURLParam("user_scope", strings.Join(scopes, ",")))
	case Microsoft:
		// Microsoft is standard: offline_access scope yields a refresh token.
	}

	return conf.AuthCodeURL(state, opts...), nil
}

// Exchange swaps an authorization code for a [Token]. It runs PKCE (sending the
// verifier) and handles Slack's non-standard token response, where the user
// token lives under authed_user.access_token and success is signaled by an
// "ok" boolean rather than an HTTP error.
func (s *Service) Exchange(ctx context.Context, p Provider, code, redirectURI, pkceVerifier string) (Token, error) {
	if p == Slack {
		return s.exchangeSlack(ctx, code, redirectURI, pkceVerifier)
	}

	conf, _, err := s.oauthConfig(p, nil, redirectURI)
	if err != nil {
		return Token{}, err
	}

	tok, err := conf.Exchange(s.clientCtx(ctx), code,
		oauth2.VerifierOption(pkceVerifier),
	)
	if err != nil {
		return Token{}, fmt.Errorf("oauth: %s exchange: %w", p, err)
	}
	return tokenFrom(p, "", tok, Token{}), nil
}

// Refresh renews the access token of t using its refresh token. For the
// standard providers it uses x/oauth2's TokenSource, which performs the refresh
// grant and reuses the existing token when it is still valid. The refresh token
// is carried forward when the provider does not rotate it.
//
// Slack user tokens are typically non-expiring and non-refreshable; Refresh
// returns t unchanged when t has no refresh token, and errors only if a caller
// somehow has a Slack token with a refresh token set (which Slack does not
// issue for user tokens). It also returns t unchanged for any provider when
// there is no refresh token to use.
func (s *Service) Refresh(ctx context.Context, p Provider, t Token) (Token, error) {
	if !s.cfg.IsConfigured(p) {
		if s.cfg.providerConfig(p) == nil {
			return Token{}, errUnknownProvider(p)
		}
		return Token{}, errUnconfigured(p)
	}

	if t.RefreshToken == "" {
		// Nothing to refresh (e.g. a Slack non-refreshable user token); the
		// caller keeps using the existing access token.
		return t, nil
	}

	conf, _, err := s.oauthConfig(p, nil, "")
	if err != nil {
		return Token{}, err
	}

	src := conf.TokenSource(s.clientCtx(ctx), t.OAuth2Token())
	tok, err := src.Token()
	if err != nil {
		return Token{}, fmt.Errorf("oauth: %s refresh: %w", p, err)
	}
	return tokenFrom(p, t.ConnectorID, tok, t), nil
}

// slackTokenResponse models the non-standard Slack oauth.v2.access reply for a
// user-token install.
type slackTokenResponse struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error"`
	Scope      string `json:"scope"`
	TokenType  string `json:"token_type"`
	AuthedUser struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		ExpiresIn    int64  `json:"expires_in"`
	} `json:"authed_user"`
}

// exchangeSlack performs the Slack token exchange by hand because Slack returns
// ok:false (HTTP 200) on failure and nests the user token under authed_user,
// neither of which x/oauth2's default decoder handles.
func (s *Service) exchangeSlack(ctx context.Context, code, redirectURI, pkceVerifier string) (Token, error) {
	conf, _, err := s.oauthConfig(Slack, nil, redirectURI)
	if err != nil {
		return Token{}, err
	}

	form := url.Values{
		"client_id":     {conf.ClientID},
		"client_secret": {conf.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {pkceVerifier},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, conf.Endpoint.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("oauth: slack exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("oauth: slack exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, fmt.Errorf("oauth: slack exchange: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Token{}, fmt.Errorf("oauth: slack exchange: HTTP %d", resp.StatusCode)
	}

	var sr slackTokenResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return Token{}, fmt.Errorf("oauth: slack exchange: decode: %w", err)
	}
	if !sr.OK {
		// sr.Error is a Slack error code, not a secret.
		return Token{}, fmt.Errorf("oauth: slack exchange failed: %s", slackErr(sr.Error))
	}
	if sr.AuthedUser.AccessToken == "" {
		return Token{}, errors.New("oauth: slack exchange: no user access token in response")
	}

	tokenType := firstNonEmpty(sr.AuthedUser.TokenType, sr.TokenType, "Bearer")
	scope := firstNonEmpty(sr.AuthedUser.Scope, sr.Scope)
	var expiry time.Time
	if sr.AuthedUser.ExpiresIn > 0 {
		expiry = time.Now().Add(time.Duration(sr.AuthedUser.ExpiresIn) * time.Second)
	}

	return Token{
		AskerOAuth:   tokenMarker,
		Provider:     Slack,
		AccessToken:  sr.AuthedUser.AccessToken,
		RefreshToken: sr.AuthedUser.RefreshToken,
		TokenType:    tokenType,
		Expiry:       expiry,
		Scope:        scope,
	}, nil
}

// slackErr returns a non-empty, safe error code for Slack failures.
func slackErr(code string) string {
	if code == "" {
		return "unknown_error"
	}
	return code
}
