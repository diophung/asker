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
}

func loadConfig() (gatewayConfig, error) {
	var cfg gatewayConfig
	if err := config.Load("", &cfg); err != nil {
		return gatewayConfig{}, err
	}
	return cfg, nil
}
