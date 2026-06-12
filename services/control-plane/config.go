package main

import "github.com/asker/asker/platform/config"

// controlPlaneConfig is loaded from the environment. Names and defaults are
// the pinned M1 service contract (gRPC :9100, HTTP health :9101).
type controlPlaneConfig struct {
	Addr        string `env:"CONTROL_PLANE_ADDR" envDefault:":9100"`
	HealthAddr  string `env:"CONTROL_PLANE_HEALTH_ADDR" envDefault:":9101"`
	DatabaseURL string `env:"DATABASE_URL" envDefault:"postgres://asker:asker@postgres:5432/asker"`
	// KEKFile is the dev-shim key-encryption key (platform/crypto FileKEK);
	// the Vault-backed KEKProvider replaces it in M4 behind the same interface.
	KEKFile string `env:"KEK_FILE" envDefault:"/keys/kek.bin"`
	// Empty endpoint means telemetry is a no-op.
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:""`
}

func loadConfig() (controlPlaneConfig, error) {
	var cfg controlPlaneConfig
	if err := config.Load("", &cfg); err != nil {
		return controlPlaneConfig{}, err
	}
	return cfg, nil
}
