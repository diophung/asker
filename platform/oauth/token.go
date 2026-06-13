package oauth

import (
	"encoding/json"
	"time"

	"golang.org/x/oauth2"
)

// tokenMarker is the value of [Token.AskerOAuth] in every credential this
// package mints. Parse uses its presence to tell an Asker OAuth blob apart from
// a legacy, manually-pasted opaque token (e.g. "fake-gmail-token:...").
const tokenMarker = "v1"

// Token is the stored OAuth credential for one connector instance. The control-
// plane token vault persists it as an opaque, encrypted []byte (its
// PutToken/GetToken take a []byte), so a Token is serialized to compact JSON by
// [Marshal] and read back by [Parse] — no protobuf change is required. When the
// connector hub runs an instance it refreshes the access token and delivers
// [Token.AccessToken] in sdk.Config.Token.
//
// AskerOAuth marks the blob as an Asker OAuth credential (value [tokenMarker]).
// Legacy tokens lack it, so [Parse] returns ok=false for them and the hub
// passes them through unchanged.
type Token struct {
	// AskerOAuth is the version marker that distinguishes this blob from a
	// legacy opaque token. It is "v1" in every Token this package mints.
	AskerOAuth string `json:"asker_oauth"`
	// Provider is the issuing provider.
	Provider Provider `json:"provider"`
	// ConnectorID is the connector this credential belongs to (e.g. "gmail").
	ConnectorID string `json:"connector_id"`
	// AccessToken is the bearer the connector uses. Never log it.
	AccessToken string `json:"access_token"`
	// RefreshToken renews the access token; empty when the provider issues
	// non-refreshable tokens (e.g. Slack user tokens). Never log it.
	RefreshToken string `json:"refresh_token,omitempty"`
	// TokenType is the token type, typically "Bearer".
	TokenType string `json:"token_type,omitempty"`
	// Expiry is when AccessToken expires; zero means it does not expire.
	Expiry time.Time `json:"expiry,omitempty"`
	// Scope is the space-separated scopes the provider actually granted.
	Scope string `json:"scope,omitempty"`
}

// Marshal serializes t to compact JSON for storage in the token vault. It never
// returns an error for a valid Token, but the signature carries one for forward
// compatibility and to satisfy callers that handle marshaling failures.
func Marshal(t Token) ([]byte, error) {
	if t.AskerOAuth == "" {
		t.AskerOAuth = tokenMarker
	}
	return json.Marshal(t)
}

// Parse decodes an Asker OAuth [Token] from its stored bytes. ok is false when
// b is NOT an Asker OAuth blob — either it is not JSON at all (a legacy pasted
// token) or it is JSON that lacks the [tokenMarker] marker — so the caller can
// treat b as an opaque legacy credential and pass it through unchanged.
func Parse(b []byte) (t Token, ok bool) {
	if err := json.Unmarshal(b, &t); err != nil {
		return Token{}, false
	}
	if t.AskerOAuth != tokenMarker {
		return Token{}, false
	}
	return t, true
}

// NeedsRefresh reports whether t should be refreshed at time now: it is true
// only when t has a refresh token AND the access token is already expired or
// expires within skew. A token with no Expiry (non-expiring) never needs a
// refresh. skew should be a small positive margin (e.g. a minute) so callers
// refresh before, not after, the token goes stale.
func (t Token) NeedsRefresh(now time.Time, skew time.Duration) bool {
	if t.RefreshToken == "" {
		return false
	}
	if t.Expiry.IsZero() {
		return false
	}
	return !now.Before(t.Expiry.Add(-skew))
}

// OAuth2Token converts t to an [*oauth2.Token] for use with x/oauth2's
// TokenSource (auto-refresh) and Exchange plumbing.
func (t Token) OAuth2Token() *oauth2.Token {
	return &oauth2.Token{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		TokenType:    t.TokenType,
		Expiry:       t.Expiry,
	}
}

// tokenFrom builds a stored [Token] from an [*oauth2.Token] and the provider /
// connector context. It carries the refresh token forward from prev when the
// provider did not return a new one (non-rotating refresh), so a refresh never
// drops a still-valid refresh token. scope falls back to prev.Scope likewise.
// Expiry also falls back to prev: a refresh response that omits expires_in
// leaves oauth2.Token.Expiry zero, which NeedsRefresh would read as
// "non-expiring" and never refresh again — so an access token that actually
// expires would silently stop self-healing. Carrying prev.Expiry keeps the hub
// refreshing on the original cadence (worst case: one extra refresh).
func tokenFrom(provider Provider, connectorID string, tok *oauth2.Token, prev Token) Token {
	out := Token{
		AskerOAuth:   tokenMarker,
		Provider:     provider,
		ConnectorID:  connectorID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    tok.TokenType,
		Expiry:       tok.Expiry,
	}
	if out.RefreshToken == "" {
		out.RefreshToken = prev.RefreshToken
	}
	if out.Expiry.IsZero() {
		out.Expiry = prev.Expiry
	}
	if scope, _ := tok.Extra("scope").(string); scope != "" {
		out.Scope = scope
	} else {
		out.Scope = prev.Scope
	}
	return out
}
