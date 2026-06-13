package oauth

import (
	"os"
	"strings"
)

// Default authorize/token endpoints for each provider. They are the real
// production URLs so a deployment only needs to set client_id/client_secret;
// dev points the *_URL overrides at the fake provider instead.
const (
	defaultGoogleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultGoogleTokenURL = "https://oauth2.googleapis.com/token"

	// Microsoft URLs embed the tenant; %s is replaced with ProviderConfig.Tenant.
	defaultMicrosoftAuthTmpl  = "https://login.microsoftonline.com/%s/oauth2/v2.0/authorize"
	defaultMicrosoftTokenTmpl = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"

	defaultSlackAuthURL  = "https://slack.com/oauth/v2/authorize"
	defaultSlackTokenURL = "https://slack.com/api/oauth.v2.access"

	defaultAtlassianAuthURL  = "https://auth.atlassian.com/authorize"
	defaultAtlassianTokenURL = "https://auth.atlassian.com/oauth/token"

	// defaultMicrosoftTenant is the multi-tenant Azure AD endpoint segment.
	defaultMicrosoftTenant = "common"
	// defaultAtlassianAudience is the resource audience for Atlassian Cloud.
	defaultAtlassianAudience = "api.atlassian.com"
)

// ProviderConfig is the per-provider client configuration. ClientID and
// ClientSecret are required for a provider to be usable; AuthURL and TokenURL
// default to the real production endpoints and are overridden in dev to point
// at the fake provider. Tenant applies only to Microsoft and Audience only to
// Atlassian.
type ProviderConfig struct {
	// ClientID is the OAuth client (application) ID.
	ClientID string
	// ClientSecret is the OAuth client secret. Never log it.
	ClientSecret string
	// AuthURL is the provider authorize endpoint.
	AuthURL string
	// TokenURL is the provider token endpoint.
	TokenURL string
	// Tenant is the Microsoft (Azure AD) tenant segment; default "common".
	Tenant string
	// Audience is the Atlassian resource audience; default "api.atlassian.com".
	Audience string
}

// Config holds the client configuration for every provider. Construct it with
// [LoadConfig] (reads the environment) or build one by hand in tests.
type Config struct {
	Google    ProviderConfig
	Microsoft ProviderConfig
	Slack     ProviderConfig
	Atlassian ProviderConfig
}

// providerConfig returns a pointer to the [ProviderConfig] for p, or nil for an
// unknown provider, so callers can mutate or inspect it in place.
func (c *Config) providerConfig(p Provider) *ProviderConfig {
	switch p {
	case Google:
		return &c.Google
	case Microsoft:
		return &c.Microsoft
	case Slack:
		return &c.Slack
	case Atlassian:
		return &c.Atlassian
	default:
		return nil
	}
}

// IsConfigured reports whether provider p has both a client ID and secret, i.e.
// flows for it can run.
func (c Config) IsConfigured(p Provider) bool {
	pc := c.providerConfig(p)
	return pc != nil && pc.ClientID != "" && pc.ClientSecret != ""
}

// envPrefix is the common prefix for every OAuth environment variable.
const envPrefix = "ASKER_OAUTH_"

// getenv reads ASKER_OAUTH_<provider>_<suffix> (provider upper-cased), e.g.
// getenv(Google, "CLIENT_ID") reads ASKER_OAUTH_GOOGLE_CLIENT_ID.
func getenv(p Provider, suffix string) string {
	return os.Getenv(envPrefix + strings.ToUpper(string(p)) + "_" + suffix)
}

// firstNonEmpty returns the first non-empty argument, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// LoadConfig builds a [Config] from the environment. For each provider it reads
// ASKER_OAUTH_<PROVIDER>_CLIENT_ID, _CLIENT_SECRET, _AUTH_URL, _TOKEN_URL;
// plus ASKER_OAUTH_MICROSOFT_TENANT and ASKER_OAUTH_ATLASSIAN_AUDIENCE. The
// authorize/token URLs default to the real production endpoints (Microsoft's
// embed the resolved tenant) so only client_id/secret need setting in prod;
// dev overrides the URLs to reach the fake provider.
func LoadConfig() Config {
	tenant := firstNonEmpty(os.Getenv(envPrefix+"MICROSOFT_TENANT"), defaultMicrosoftTenant)
	audience := firstNonEmpty(os.Getenv(envPrefix+"ATLASSIAN_AUDIENCE"), defaultAtlassianAudience)

	return Config{
		Google: ProviderConfig{
			ClientID:     getenv(Google, "CLIENT_ID"),
			ClientSecret: getenv(Google, "CLIENT_SECRET"),
			AuthURL:      firstNonEmpty(getenv(Google, "AUTH_URL"), defaultGoogleAuthURL),
			TokenURL:     firstNonEmpty(getenv(Google, "TOKEN_URL"), defaultGoogleTokenURL),
		},
		Microsoft: ProviderConfig{
			ClientID:     getenv(Microsoft, "CLIENT_ID"),
			ClientSecret: getenv(Microsoft, "CLIENT_SECRET"),
			AuthURL:      firstNonEmpty(getenv(Microsoft, "AUTH_URL"), msEndpoint(defaultMicrosoftAuthTmpl, tenant)),
			TokenURL:     firstNonEmpty(getenv(Microsoft, "TOKEN_URL"), msEndpoint(defaultMicrosoftTokenTmpl, tenant)),
			Tenant:       tenant,
		},
		Slack: ProviderConfig{
			ClientID:     getenv(Slack, "CLIENT_ID"),
			ClientSecret: getenv(Slack, "CLIENT_SECRET"),
			AuthURL:      firstNonEmpty(getenv(Slack, "AUTH_URL"), defaultSlackAuthURL),
			TokenURL:     firstNonEmpty(getenv(Slack, "TOKEN_URL"), defaultSlackTokenURL),
		},
		Atlassian: ProviderConfig{
			ClientID:     getenv(Atlassian, "CLIENT_ID"),
			ClientSecret: getenv(Atlassian, "CLIENT_SECRET"),
			AuthURL:      firstNonEmpty(getenv(Atlassian, "AUTH_URL"), defaultAtlassianAuthURL),
			TokenURL:     firstNonEmpty(getenv(Atlassian, "TOKEN_URL"), defaultAtlassianTokenURL),
			Audience:     audience,
		},
	}
}

// msEndpoint fills a Microsoft endpoint template with the tenant segment.
func msEndpoint(tmpl, tenant string) string {
	return strings.Replace(tmpl, "%s", tenant, 1)
}
