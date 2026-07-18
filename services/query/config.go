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
	// contribute little. Only used when RecencyWeight > 0. Also the half-life of
	// the recency bonus in the personalized re-rank (recency-vs-importance slider).
	RecencyHalfLife time.Duration `env:"QUERY_RECENCY_HALFLIFE" envDefault:"720h"`

	// --- Personalization (v3.2) ---------------------------------------------

	// PersonalizationEnabled wires the Redis profile loader (main.go). When false
	// the query path runs the non-personalized pipeline exactly as before — a
	// rollback switch. Per-user profiles still cold-start to sensible defaults.
	PersonalizationEnabled bool `env:"QUERY_PERSONALIZATION_ENABLED" envDefault:"true"`
	// HybridRRF fuses a keyword arm and a vector arm with Reciprocal Rank Fusion
	// on the personalized path (spec §1.4). Off => the personalized path reuses
	// the single-pass hybrid retrieval. Personal corpora are tiny (exact
	// streaming scan), so the extra arm is within budget.
	HybridRRF bool `env:"QUERY_HYBRID_RRF" envDefault:"true"`
	// CandidateCap is how many candidates the personalized path retrieves (at
	// offset 0) before re-ranking down to the requested page. Larger => better
	// recall for the re-ranker, more work. Clamped to [maxLimit, 1000].
	CandidateCap int `env:"QUERY_CANDIDATE_CAP" envDefault:"100"`

	// --- Cross-encoder reranking (Phase 1) ----------------------------------

	// RerankEnabled wires the reranker client (main.go). When false the reranker
	// is not constructed and the SearchRequest.rerank flag is ignored — a clean
	// rollback switch, and the default so existing deployments are unchanged.
	RerankEnabled bool `env:"QUERY_RERANK_ENABLED" envDefault:"false"`
	// RerankURL is the reranker model service (bge-reranker-v2-m3 by default),
	// analogous to TEI/CLIP: it scores (query, candidate) pairs.
	RerankURL string `env:"QUERY_RERANK_URL" envDefault:"http://reranker:9900"`
	// RerankTimeout bounds the reranker /rerank call; on expiry the rerank pass
	// is skipped (degraded="rerank-unavailable") — retrieval is unaffected
	// (never fail closed). Default fits the ~300ms/50-candidate budget with slack.
	RerankTimeout time.Duration `env:"QUERY_RERANK_TIMEOUT" envDefault:"1s"`
	// RerankCandidates is how many of the top fused candidates are handed to the
	// cross-encoder (rerank depth). The reranked set is what the page is drawn
	// from. Larger => better recall for the reranker, more per-query cost.
	RerankCandidates int `env:"QUERY_RERANK_CANDIDATES" envDefault:"50"`
	// RerankDocChars caps how much of each candidate's text is sent to the
	// reranker (cross-encoders truncate long inputs anyway; this bounds payload
	// and latency).
	RerankDocChars int `env:"QUERY_RERANK_DOC_CHARS" envDefault:"1024"`

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
	if cfg.CandidateCap < maxLimit {
		cfg.CandidateCap = maxLimit
	}
	if cfg.CandidateCap > 1000 {
		cfg.CandidateCap = 1000
	}
	if cfg.RerankEnabled {
		if cfg.RerankTimeout <= 0 {
			return queryConfig{}, fmt.Errorf("config: QUERY_RERANK_TIMEOUT must be > 0 when QUERY_RERANK_ENABLED, got %s", cfg.RerankTimeout)
		}
		if cfg.RerankCandidates <= 0 {
			return queryConfig{}, fmt.Errorf("config: QUERY_RERANK_CANDIDATES must be > 0 when QUERY_RERANK_ENABLED, got %d", cfg.RerankCandidates)
		}
		if cfg.RerankDocChars <= 0 {
			return queryConfig{}, fmt.Errorf("config: QUERY_RERANK_DOC_CHARS must be > 0 when QUERY_RERANK_ENABLED, got %d", cfg.RerankDocChars)
		}
		// The reranker reorders the top RerankCandidates of the retrieved
		// candidate pool, so the pool must be at least that deep.
		if cfg.CandidateCap < cfg.RerankCandidates {
			cfg.CandidateCap = cfg.RerankCandidates
		}
	}
	return cfg, nil
}
