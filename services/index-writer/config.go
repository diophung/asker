package main

import (
	"fmt"
	"strings"

	"github.com/asker/asker/platform/config"
	"github.com/asker/asker/platform/kafkautil"
)

// indexWriterConfig is loaded from the environment. Defaults match the M1
// compose topology; EMBEDDING_DIM is deploy-time configuration (ADR-005) and
// must equal the dimension templated into the Vespa schema.
type indexWriterConfig struct {
	// Kafka carries KAFKA_BROKERS / KAFKA_CLIENT_ID (dev default redpanda:9092).
	Kafka kafkautil.Config
	// VespaURL is the document/query API base of the Vespa container.
	VespaURL string `env:"VESPA_URL" envDefault:"http://vespa:8080"`
	// EmbeddingDim is the exact length every chunk vector must have. A
	// mismatch is a feed error, never a silent index (ADR-005).
	EmbeddingDim int `env:"EMBEDDING_DIM" envDefault:"1024"`
	// HealthAddr serves /healthz and /readyz.
	HealthAddr string `env:"INDEX_WRITER_HEALTH_ADDR" envDefault:":9701"`
	// OTLPEndpoint empty means telemetry is a no-op.
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:""`
}

func loadConfig() (indexWriterConfig, error) {
	var cfg indexWriterConfig
	if err := config.Load("", &cfg); err != nil {
		return indexWriterConfig{}, err
	}
	if strings.TrimSpace(cfg.VespaURL) == "" {
		return indexWriterConfig{}, fmt.Errorf("index-writer: VESPA_URL must not be empty")
	}
	if cfg.EmbeddingDim <= 0 {
		return indexWriterConfig{}, fmt.Errorf("index-writer: EMBEDDING_DIM must be positive, got %d", cfg.EmbeddingDim)
	}
	return cfg, nil
}
