package main

import "github.com/asker/asker/platform/config"

// controlPlaneConfig is loaded from the environment. Names and defaults are
// the pinned M1 service contract (gRPC :9100, HTTP health :9101).
type controlPlaneConfig struct {
	Addr        string `env:"CONTROL_PLANE_ADDR" envDefault:":9100"`
	HealthAddr  string `env:"CONTROL_PLANE_HEALTH_ADDR" envDefault:":9101"`
	DatabaseURL string `env:"DATABASE_URL" envDefault:"postgres://asker:asker@postgres:5432/asker"`
	// KEKFile is the dev-shim key-encryption key (platform/crypto FileKEK);
	// the Vault-backed KEKProvider replaces it when VaultAddr is set (ADR-015).
	KEKFile string `env:"KEK_FILE" envDefault:"/keys/kek.bin"`

	// Vault KEK selection (ADR-015 §3): when VaultAddr is set, the control plane
	// wraps/unwraps tenant DEKs via Vault Transit (the KEK never enters service
	// memory); otherwise it uses the dev FileKEK. VaultKEKKeyName defaults to
	// "asker-kek" — control-plane and connector-hub MUST agree on it so a DEK
	// wrapped by one unwraps in the other.
	VaultAddr       string `env:"VAULT_ADDR" envDefault:""`
	VaultToken      string `env:"VAULT_TOKEN" envDefault:""`
	VaultKEKKeyName string `env:"VAULT_KEK_KEY_NAME" envDefault:"asker-kek"`
	// Env is the deployment marker. When it is "production" (loud, explicit),
	// the file-KEK dev shim is REFUSED at startup unless VaultAddr is set — a
	// prod deployment must never silently mint an ephemeral dev KEK (ADR-015).
	Env string `env:"ASKER_ENV" envDefault:""`

	// GDPR delete cascade targets (M6). Empty disables that store's purge (the
	// cascade still purges Postgres + crypto-shreds the DEK). The Vespa/Redis/
	// MinIO defaults match the rest of the stack's compose service names.
	VespaURL     string `env:"VESPA_URL" envDefault:"http://vespa:8080"`
	VespaCluster string `env:"VESPA_CONTENT_CLUSTER" envDefault:"asker"`
	RedisAddr    string `env:"REDIS_ADDR" envDefault:"redis:6379"`
	// MinIO settings for the blob-prefix purge (env names match platform/blob).
	MinIOEndpoint  string `env:"MINIO_ENDPOINT" envDefault:"minio:9000"`
	MinIOAccessKey string `env:"MINIO_ACCESS_KEY" envDefault:"asker-minio"`
	MinIOSecretKey string `env:"MINIO_SECRET_KEY" envDefault:"asker-minio-secret"`
	MinIOBucket    string `env:"BLOB_BUCKET" envDefault:"asker-blobs"`
	MinIOUseSSL    bool   `env:"MINIO_USE_SSL" envDefault:"false"`

	// MaxConnectorInstancesPerTenant caps how many connector instances one
	// tenant may create (abuse control; bounds scheduler goroutines). <= 0
	// disables the cap.
	MaxConnectorInstancesPerTenant int `env:"MAX_CONNECTOR_INSTANCES_PER_TENANT" envDefault:"50"`

	// Empty endpoint means telemetry is a no-op.
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:""`
}

// isProd reports whether the deployment is marked production (the KEK
// fail-closed guard).
func (c controlPlaneConfig) isProd() bool {
	return c.Env == "production" || c.Env == "prod"
}

func loadConfig() (controlPlaneConfig, error) {
	var cfg controlPlaneConfig
	if err := config.Load("", &cfg); err != nil {
		return controlPlaneConfig{}, err
	}
	return cfg, nil
}
