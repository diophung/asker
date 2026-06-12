package main

import (
	"fmt"
	"time"

	"github.com/asker/asker/platform/config"
)

// queryConfig is loaded from the environment. Defaults match the M1 compose
// topology in the build contract; EMBEDDING_DIM must equal the dimension of
// the model TEI is serving AND the deployed Vespa schema (ADR-005) — the dev
// stack runs 384, prod-shaped stacks 1024. It is never hardcoded.
type queryConfig struct {
	Addr       string `env:"QUERY_ADDR" envDefault:":9200"`
	HealthAddr string `env:"QUERY_HEALTH_ADDR" envDefault:":9201"`
	TEIURL     string `env:"TEI_URL" envDefault:"http://tei:80"`
	VespaURL   string `env:"VESPA_URL" envDefault:"http://vespa:8080"`
	RedisAddr  string `env:"REDIS_ADDR" envDefault:"redis:6379"`
	// EmbeddingDim is the query-vector dimensionality; a TEI response of any
	// other length is rejected (ADR-005: loud operator error, never a
	// silently wrong-size vector).
	EmbeddingDim int `env:"EMBEDDING_DIM" envDefault:"1024"`
	// EmbedTimeout bounds the TEI /embed call; on expiry HYBRID degrades to
	// keyword-only (spec §2.6 ladder).
	EmbedTimeout time.Duration `env:"QUERY_EMBED_TIMEOUT" envDefault:"2s"`
	// Empty endpoint means telemetry is a no-op.
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:""`
}

func loadConfig() (queryConfig, error) {
	var cfg queryConfig
	if err := config.Load("", &cfg); err != nil {
		return queryConfig{}, err
	}
	if cfg.EmbeddingDim <= 0 {
		return queryConfig{}, fmt.Errorf("config: EMBEDDING_DIM must be > 0, got %d", cfg.EmbeddingDim)
	}
	if cfg.EmbedTimeout <= 0 {
		return queryConfig{}, fmt.Errorf("config: QUERY_EMBED_TIMEOUT must be > 0, got %s", cfg.EmbedTimeout)
	}
	return cfg, nil
}
