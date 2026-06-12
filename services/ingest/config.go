package main

import (
	"github.com/asker/asker/platform/config"
	"github.com/asker/asker/platform/kafkautil"
)

// ingestConfig is loaded from the environment. Names and defaults follow the
// pinned M1 service contract (health on :9501, Kafka via KAFKA_BROKERS,
// Redis via REDIS_ADDR).
type ingestConfig struct {
	// HealthAddr serves /healthz and /readyz; ingest has no other listener.
	HealthAddr string `env:"INGEST_HEALTH_ADDR" envDefault:":9501"`
	// RedisAddr is the dedupe store. Redis being down never blocks the
	// pipeline (availability first; downstream upserts are idempotent).
	RedisAddr string `env:"REDIS_ADDR" envDefault:"redis:6379"`
	// OTLPEndpoint empty means telemetry is a no-op.
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:""`
	// Kafka reads KAFKA_BROKERS / KAFKA_CLIENT_ID.
	Kafka kafkautil.Config
}

func loadConfig() (ingestConfig, error) {
	var cfg ingestConfig
	if err := config.Load("", &cfg); err != nil {
		return ingestConfig{}, err
	}
	if cfg.Kafka.ClientID == "" {
		cfg.Kafka.ClientID = serviceName
	}
	return cfg, nil
}
