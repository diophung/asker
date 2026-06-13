// Package hub implements the connector hub: the scheduler that runs every
// tenant's connector instances, the single Emit chokepoint that stamps and
// produces canonical Documents onto docs.raw, the webhook receiver, the
// direct-upload endpoint, and the per-tenant sync-status API.
//
// Trust model (ADR-009): the hub sits inside the trusted network. It derives
// each instance's tenant from control-plane records (which the gateway
// created from a VERIFIED JWT), re-validates it via tenancy.FromHeaderValue,
// and propagates it on every hop: tenancygrpc client interceptor toward the
// control plane, kafkautil headers toward the pipeline, and the
// x-asker-tenant HTTP header on the two internal HTTP hops it serves
// (/upload and /v1/sync-status), both fail-closed.
package hub

import (
	"errors"
	"time"

	"github.com/asker/asker/platform/config"
	"github.com/asker/asker/platform/kafkautil"
)

// Config is the connector-hub environment configuration (pinned M1 service
// contract: HTTP :9300, health :9301).
type Config struct {
	// Addr serves /webhooks/*, /upload and /v1/sync-status.
	Addr string `env:"HUB_ADDR" envDefault:":9300"`
	// HealthAddr serves /healthz and /readyz.
	HealthAddr string `env:"HUB_HEALTH_ADDR" envDefault:":9301"`
	// WebhookBase is the externally reachable base URL the hub advertises to
	// connectors as {"webhook_url": WebhookBase + "/webhooks/<cid>/<iid>"}.
	WebhookBase string `env:"HUB_WEBHOOK_BASE" envDefault:"http://connector-hub:9300"`
	// ControlPlaneAddr is the control-plane gRPC target.
	ControlPlaneAddr string `env:"CONTROL_PLANE_GRPC_ADDR" envDefault:"dns:///control-plane:9100"`
	// SyncInterval is the steady-state IncrementalSync cadence per instance.
	SyncInterval time.Duration `env:"CONNECTOR_SYNC_INTERVAL" envDefault:"30s"`
	// SchedulerTick is how often the scheduler reconciles its worker set
	// against SchedulerService.ListAllInstances.
	SchedulerTick time.Duration `env:"SCHEDULER_TICK" envDefault:"10s"`
	// KEKFile is the dev-shim key-encryption key shared with the control
	// plane (same /keys volume). The hub itself never decrypts tokens (the
	// control plane returns them decrypted); it is plumbed through to
	// connector wiring that needs envelope encryption (blob store).
	KEKFile string `env:"KEK_FILE" envDefault:"/keys/kek.bin"`

	// Vault KEK selection (ADR-015 §3), MUST match the control plane's choice
	// and key name so wrapped DEKs interoperate. VAULT_ADDR set -> Vault Transit;
	// else the dev file KEK. In production (ASKER_ENV=production) an empty
	// VAULT_ADDR fails closed at startup (never an ephemeral dev KEK).
	VaultAddr       string `env:"VAULT_ADDR" envDefault:""`
	VaultToken      string `env:"VAULT_TOKEN" envDefault:""`
	VaultKEKKeyName string `env:"VAULT_KEK_KEY_NAME" envDefault:"asker-kek"`
	// Env is the deployment marker (the KEK prod fail-closed guard).
	Env string `env:"ASKER_ENV" envDefault:""`

	// MaxInstancesPerTenant bounds how many sync workers (goroutines) one tenant
	// can occupy in the scheduler — defense in depth alongside the control
	// plane's per-tenant connector-instance create cap, in case stale rows or a
	// misconfigured cap let a tenant accumulate instances. <= 0 disables the cap.
	MaxInstancesPerTenant int `env:"HUB_MAX_INSTANCES_PER_TENANT" envDefault:"50"`

	// MinIO settings are consumed by the upload connector's blob store
	// (platform/blob), wired in package main; the hub only loads and
	// forwards them. The env names match platform/blob.Config so either
	// loading path sees the same configuration.
	MinIOEndpoint  string `env:"MINIO_ENDPOINT" envDefault:"minio:9000"`
	MinIOAccessKey string `env:"MINIO_ACCESS_KEY" envDefault:"asker-minio"`
	MinIOSecretKey string `env:"MINIO_SECRET_KEY" envDefault:"asker-minio-secret"`
	MinIOBucket    string `env:"BLOB_BUCKET" envDefault:"asker-blobs"`
	MinIOUseSSL    bool   `env:"MINIO_USE_SSL" envDefault:"false"`

	// Kafka carries KAFKA_BROKERS / KAFKA_CLIENT_ID.
	Kafka kafkautil.Config

	// OTLPEndpoint empty means telemetry is a no-op.
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:""`
}

// LoadConfig reads Config from the environment.
func LoadConfig() (Config, error) {
	var cfg Config
	if err := config.Load("", &cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	switch {
	case c.SyncInterval <= 0:
		return errors.New("hub: CONNECTOR_SYNC_INTERVAL must be positive")
	case c.SchedulerTick <= 0:
		return errors.New("hub: SCHEDULER_TICK must be positive")
	case c.WebhookBase == "":
		return errors.New("hub: HUB_WEBHOOK_BASE must not be empty")
	}
	return nil
}

// IsProd reports whether the deployment is marked production (the KEK
// fail-closed guard, ADR-015).
func (c Config) IsProd() bool {
	return c.Env == "production" || c.Env == "prod"
}
