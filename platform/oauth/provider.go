// Package oauth is the shared OAuth2 foundation for Asker's cloud connectors.
//
// It is tenant-agnostic plumbing: callers (the connector hub, the gateway)
// scope every flow by tenant before invoking these helpers. The package owns
// three concerns: (1) the static mapping from a connector ID to its OAuth
// provider and default read scopes, (2) the stored credential type [Token]
// — a versioned JSON blob the control-plane token vault persists as an opaque
// []byte — and (3) the [Service] that drives the authorization-code flow
// (with PKCE), token exchange, and refresh against each provider, including
// the providers (Slack) whose token endpoints do not follow the OAuth2
// standard.
//
// Secrets (client secrets, access and refresh tokens) are NEVER logged by this
// package and must never be logged by callers.
package oauth

import "fmt"

// Provider identifies an OAuth2 identity provider Asker integrates with. It is
// a string for stable serialization in [Token] and in logs.
type Provider string

// The providers Asker supports. The string values are stable wire identifiers.
const (
	// Google backs the Gmail, Google Calendar, and Google Drive connectors.
	Google Provider = "google"
	// Microsoft backs the Outlook mail/calendar and Microsoft Teams connectors.
	Microsoft Provider = "microsoft"
	// Slack backs the Slack connector (user-token / v2 style).
	Slack Provider = "slack"
	// Atlassian backs the Jira and Confluence connectors.
	Atlassian Provider = "atlassian"
)

// Providers returns every supported provider, in a stable order. The returned
// slice is freshly allocated, so callers may modify it.
func Providers() []Provider {
	return []Provider{Google, Microsoft, Slack, Atlassian}
}

// ParseProvider maps a string (case-sensitive, lowercase wire form) to a
// [Provider]. ok is false for an unknown value.
func ParseProvider(s string) (p Provider, ok bool) {
	switch Provider(s) {
	case Google, Microsoft, Slack, Atlassian:
		return Provider(s), true
	default:
		return "", false
	}
}

// Valid reports whether p is one of the supported providers.
func (p Provider) Valid() bool {
	_, ok := ParseProvider(string(p))
	return ok
}

// String returns the provider's stable wire identifier.
func (p Provider) String() string { return string(p) }

// connectorMapping is the static connector-ID -> provider + default scopes
// table. Default scopes are the provider's READ scopes plus the offline/refresh
// scope where the provider expresses refresh as a scope (Microsoft, Atlassian).
// Google's offline access is requested via auth-URL params, not a scope, so it
// is absent here (see [Service.AuthCodeURL]).
var connectorMapping = map[string]struct {
	provider Provider
	scopes   []string
}{
	"gmail":  {Google, []string{"https://www.googleapis.com/auth/gmail.readonly"}},
	"gcal":   {Google, []string{"https://www.googleapis.com/auth/calendar.readonly"}},
	"gdrive": {Google, []string{"https://www.googleapis.com/auth/drive.readonly"}},

	"outlook-mail": {Microsoft, []string{"Mail.Read", "offline_access"}},
	"outlook-cal":  {Microsoft, []string{"Calendars.Read", "offline_access"}},
	"msteams":      {Microsoft, []string{"Chat.Read", "ChannelMessage.Read.All", "offline_access"}},

	"slack": {Slack, []string{"channels:history", "channels:read", "users:read"}},

	"jira":       {Atlassian, []string{"read:jira-work", "offline_access"}},
	"confluence": {Atlassian, []string{"read:confluence-content.all", "offline_access"}},
}

// ConnectorOAuth returns the OAuth [Provider] and the default read scopes for a
// connector. ok is false for a connector that does not use OAuth (or an unknown
// connector ID), so callers can pass non-OAuth credentials through unchanged.
// The returned scope slice is freshly allocated; callers may modify it.
func ConnectorOAuth(connectorID string) (p Provider, scopes []string, ok bool) {
	m, found := connectorMapping[connectorID]
	if !found {
		return "", nil, false
	}
	out := make([]string, len(m.scopes))
	copy(out, m.scopes)
	return m.provider, out, true
}

// errUnconfigured is the sentinel wrapped when a provider lacks client
// credentials. It is wrapped (not returned bare) so the offending provider is
// named without leaking the secret.
func errUnconfigured(p Provider) error {
	return fmt.Errorf("oauth: provider %q is not configured (missing client_id/client_secret)", p)
}

// errUnknownProvider is wrapped when a method receives an unsupported provider.
func errUnknownProvider(p Provider) error {
	return fmt.Errorf("oauth: unknown provider %q", p)
}
