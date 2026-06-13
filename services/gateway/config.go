package main

import "github.com/asker/asker/platform/config"

// gatewayConfig is loaded from the environment. Defaults match the M0 auth
// contract: tokens are issued with the host-visible issuer (KC_HOSTNAME) while
// JWKS is fetched over the compose network, hence explicit issuer + JWKS URL
// instead of OIDC discovery.
type gatewayConfig struct {
	Addr         string `env:"GATEWAY_ADDR" envDefault:":8080"`
	OIDCIssuer   string `env:"OIDC_ISSUER" envDefault:"http://localhost:8081/realms/asker"`
	OIDCJWKSURL  string `env:"OIDC_JWKS_URL" envDefault:"http://keycloak:8080/realms/asker/protocol/openid-connect/certs"`
	OIDCAudience string `env:"OIDC_AUDIENCE" envDefault:"asker-web"`
	// Empty endpoint means telemetry is a no-op.
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:""`

	// M1 backends. gRPC targets use the dns resolver so compose service names
	// re-resolve across container restarts.
	QueryGRPCAddr        string `env:"QUERY_GRPC_ADDR" envDefault:"dns:///query:9200"`
	ControlPlaneGRPCAddr string `env:"CONTROL_PLANE_GRPC_ADDR" envDefault:"dns:///control-plane:9100"`
	HubHTTPURL           string `env:"HUB_HTTP_URL" envDefault:"http://connector-hub:9300"`
	RedisAddr            string `env:"REDIS_ADDR" envDefault:"redis:6379"`
	// Per-tenant fixed-window limit; <= 0 disables limiting entirely.
	RateLimitPerMinute int `env:"RATE_LIMIT_PER_MINUTE" envDefault:"600"`

	// Pre-auth throttle (M6 DoS hardening): a per-source-IP limit plus a global
	// ceiling that run IN FRONT of JWT verification, so unauthenticated floods
	// cannot hammer JWKS/crypto. <= 0 disables that dimension.
	PreAuthPerIPPerMinute int `env:"PREAUTH_PER_IP_PER_MINUTE" envDefault:"120"`
	PreAuthGlobalPerSec   int `env:"PREAUTH_GLOBAL_PER_SEC" envDefault:"500"`
	PreAuthGlobalBurst    int `env:"PREAUTH_GLOBAL_BURST" envDefault:"1000"`
	// TrustProxyHeaders uses X-Forwarded-For for the client IP. Only enable
	// behind a trusted proxy that sets it (otherwise a client spoofs its IP).
	TrustProxyHeaders bool `env:"TRUST_PROXY_HEADERS" envDefault:"false"`

	// MaxQueryChars caps the /v1/search q= length so a giant query string
	// cannot drive disproportionate downstream work. <= 0 disables the cap.
	MaxQueryChars int `env:"MAX_QUERY_CHARS" envDefault:"1024"`
	// Comma-separated exact-match origins. Never "*": the allowed origin is
	// echoed back verbatim.
	CORSAllowedOrigins string `env:"CORS_ALLOWED_ORIGINS" envDefault:"http://localhost:3000"`
	MaxUploadMB        int64  `env:"MAX_UPLOAD_MB" envDefault:"32"`
	// Cap on the bytes streamed back from the hub for GET /v1/media — these
	// are thumbnails/keyframes, so the default is small.
	MaxMediaMB int64 `env:"MAX_MEDIA_MB" envDefault:"25"`
}

func loadConfig() (gatewayConfig, error) {
	var cfg gatewayConfig
	if err := config.Load("", &cfg); err != nil {
		return gatewayConfig{}, err
	}
	return cfg, nil
}
