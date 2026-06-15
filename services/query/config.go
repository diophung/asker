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
	// ClipURL is the CLIP model service (ADR-013), analogous to TEI: it embeds
	// the query text into CLIP space for the text->image retrieval arm.
	ClipURL string `env:"CLIP_URL" envDefault:"http://clip:9800"`
	// ClipDim is the CLIP query-vector dimensionality (ViT-B/32 => 512); it
	// must equal the served CLIP model AND the deployed Vespa clip_embedding
	// tensor. Like EMBEDDING_DIM it is deploy-time config, validated > 0 below,
	// and a CLIP response of any other length is rejected.
	ClipDim int `env:"CLIP_DIM" envDefault:"512"`
	// ClipTimeout bounds the CLIP /embed/text call; on expiry the CLIP arm is
	// dropped (degraded="clip-unavailable") — text retrieval is unaffected
	// (ADR-006: never fail closed).
	ClipTimeout time.Duration `env:"QUERY_CLIP_TIMEOUT" envDefault:"2s"`

	// RecencyWeight blends freshness into ranking ("most recent, most relevant
	// first"): the retrieved page is re-ordered by
	// (1-w)*normRelevance + w*recency, where recency decays with RecencyHalfLife.
	// w in [0,1]; 0 disables the blend (pure relevance). Default 0.4 keeps
	// relevance leading while fresh results clearly rise (rerank.go).
	RecencyWeight float64 `env:"QUERY_RECENCY_WEIGHT" envDefault:"0.4"`
	// RecencyHalfLife is the age at which a hit's recency contribution halves.
	// 720h = 30 days: items within a month stay strongly boosted, year-old items
	// contribute little. Only used when RecencyWeight > 0.
	RecencyHalfLife time.Duration `env:"QUERY_RECENCY_HALFLIFE" envDefault:"720h"`

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
	if cfg.ClipDim <= 0 {
		return queryConfig{}, fmt.Errorf("config: CLIP_DIM must be > 0, got %d", cfg.ClipDim)
	}
	if cfg.ClipTimeout <= 0 {
		return queryConfig{}, fmt.Errorf("config: QUERY_CLIP_TIMEOUT must be > 0, got %s", cfg.ClipTimeout)
	}
	if cfg.RecencyWeight < 0 || cfg.RecencyWeight > 1 {
		return queryConfig{}, fmt.Errorf("config: QUERY_RECENCY_WEIGHT must be in [0,1], got %v", cfg.RecencyWeight)
	}
	if cfg.RecencyWeight > 0 && cfg.RecencyHalfLife <= 0 {
		return queryConfig{}, fmt.Errorf("config: QUERY_RECENCY_HALFLIFE must be > 0 when QUERY_RECENCY_WEIGHT > 0, got %s", cfg.RecencyHalfLife)
	}
	return cfg, nil
}
