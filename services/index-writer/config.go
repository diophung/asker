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
	// EmbeddingDim is the exact length every bge-m3 (text/ocr/asr/caption)
	// chunk vector must have. A mismatch is a feed error, never a silent
	// index (ADR-005).
	EmbeddingDim int `env:"EMBEDDING_DIM" envDefault:"1024"`
	// CLIPDim is the exact length every CLIP image/keyframe chunk vector must
	// have (ADR-013; dev 512 for ViT-B/32). It must equal the @CLIP_DIM@ token
	// templated into the Vespa schema's clip_embedding tensor. A chunk vector's
	// length decides which Vespa field it feeds (EMBEDDING_DIM -> embedding,
	// CLIP_DIM -> clip_embedding); any other length is a feed error.
	CLIPDim int `env:"CLIP_DIM" envDefault:"512"`
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
	if cfg.CLIPDim <= 0 {
		return indexWriterConfig{}, fmt.Errorf("index-writer: CLIP_DIM must be positive, got %d", cfg.CLIPDim)
	}
	if cfg.CLIPDim == cfg.EmbeddingDim {
		// The writer routes a chunk's vector to embedding vs clip_embedding by
		// its length alone; identical dims make that routing ambiguous.
		return indexWriterConfig{}, fmt.Errorf(
			"index-writer: CLIP_DIM (%d) must differ from EMBEDDING_DIM (%d) so chunk vectors can be routed by length",
			cfg.CLIPDim, cfg.EmbeddingDim)
	}
	return cfg, nil
}
