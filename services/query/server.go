package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	"github.com/asker/asker/platform/tenancy"
)

// server implements queryv1.QueryService. The tenant arrives exclusively via
// the tenancygrpc server interceptor; Search re-checks it and fails closed.
type server struct {
	queryv1.UnimplementedQueryServiceServer

	embed  embedder
	vespa  vespaSearcher
	cache  resultCache
	logger *slog.Logger

	// cacheWarnOnce gates the loud log for a down Redis: the contract is
	// "skip silently (log once)" — first failure warns, the rest are debug.
	cacheWarnOnce sync.Once
}

func newServer(embed embedder, vespa vespaSearcher, cache resultCache, logger *slog.Logger) *server {
	return &server{embed: embed, vespa: vespa, cache: cache, logger: logger}
}

// Search runs the spec §2.6 pipeline: validate/normalize -> understand ->
// cache -> embed -> Vespa (with the degradation ladder) -> respond.
func (s *server) Search(ctx context.Context, req *queryv1.SearchRequest) (*queryv1.SearchResponse, error) {
	start := time.Now()

	tc, err := tenancy.FromContext(ctx)
	if err != nil {
		// The interceptor guarantees a tenant; this is fail-closed defense
		// in depth, not a reachable path on the wired server.
		return nil, status.Error(codes.Unauthenticated, "query: no tenant in request context")
	}
	logger := s.logger.With("tenant", string(tc.TenantID()))

	// Stage 1+2: normalize, then query understanding.
	stage := time.Now()
	norm := normalizeRequest(req)
	plan := understand(norm)
	logger.Debug("stage understand",
		"took", time.Since(stage),
		"residual_chars", len(plan.Text),
		"doc_types", len(plan.DocTypes),
		"has_participant", plan.Participant != "",
		"mode", norm.GetMode().String())

	if plan.Text == "" && !plan.hasFilters() {
		return nil, status.Error(codes.InvalidArgument, "query: empty query with no filters")
	}

	// Stage 3: result cache.
	stage = time.Now()
	key := cacheKey(tc.TenantID(), norm)
	if cached := s.cacheGet(ctx, logger, key); cached != nil {
		cached.Cached = true
		cached.TookMs = time.Since(start).Milliseconds()
		logger.Debug("stage cache", "took", time.Since(stage), "hit", true)
		return cached, nil
	}
	logger.Debug("stage cache", "took", time.Since(stage), "hit", false)

	// Stage 4: embed the residual text (HYBRID/VECTOR only). Ladder rung 1:
	// a TEI failure in HYBRID degrades to keyword-only; VECTOR mode errors.
	mode := norm.GetMode()
	degraded := ""
	var vector []float32
	if plan.Text != "" && (mode == queryv1.SearchMode_HYBRID || mode == queryv1.SearchMode_VECTOR) {
		stage = time.Now()
		v, embedErr := s.embed.Embed(ctx, plan.Text)
		logger.Debug("stage embed", "took", time.Since(stage), "error", embedErr != nil)
		switch {
		case embedErr == nil:
			vector = v
		case mode == queryv1.SearchMode_VECTOR:
			return nil, status.Errorf(codes.Unavailable, "query: embedding unavailable in VECTOR mode: %v", embedErr)
		default:
			if errors.Is(embedErr, errEmbedDim) {
				logger.Error("EMBEDDING_DIM mismatch — fix the deployment (ADR-005)", "error", embedErr)
			} else {
				logger.Warn("query embedding failed; degrading to keyword-only", "error", embedErr)
			}
			degraded = degradedKeywordOnly
		}
	}

	// Stage 5+6: Vespa retrieval with the degradation ladder.
	vq := vespaQuery{
		Tenant:      tc.TenantID(), // from verified ctx — NEVER from the request
		Kind:        retrievalPlan(plan, mode, vector),
		Text:        plan.Text,
		Vector:      vector,
		DocTypes:    plan.DocTypes,
		From:        plan.From,
		To:          plan.To,
		Participant: plan.Participant,
		Hits:        norm.GetLimit(),
		Offset:      norm.GetOffset(),
	}
	stage = time.Now()
	result, keywordFallback, err := s.searchWithDegradation(ctx, vq)
	logger.Debug("stage vespa", "took", time.Since(stage), "profile", vq.Kind.profile(), "error", err != nil)
	if keywordFallback {
		degraded = degradedKeywordOnly
	}
	if err != nil {
		if errors.Is(err, errInvalidFilterValue) {
			return nil, status.Errorf(codes.InvalidArgument, "query: %v", err)
		}
		return nil, status.Errorf(codes.Unavailable, "query: search backend: %v", err)
	}

	// Stage 7: respond; cache full-fidelity (non-degraded) results only, so
	// a 60s TTL never pins keyword-only results past a TEI/Vespa blip.
	resp := &queryv1.SearchResponse{
		Hits:     result.Hits,
		Total:    result.Total,
		Degraded: degraded,
		TookMs:   time.Since(start).Milliseconds(),
	}
	if degraded == "" {
		s.cacheSet(ctx, logger, key, resp)
	}
	logger.Debug("search complete",
		"took", time.Since(start), "hits", len(resp.Hits), "total", resp.Total, "degraded", degraded)
	return resp, nil
}

// retrievalPlan picks the retrieval kind from the understanding output, the
// requested mode, and whether a query vector is actually available.
func retrievalPlan(plan parsedQuery, mode queryv1.SearchMode, vector []float32) retrievalKind {
	switch {
	case plan.Text == "":
		return retrieveFilterOnly
	case mode == queryv1.SearchMode_KEYWORD, vector == nil:
		// vector == nil with HYBRID is ladder rung 1 (TEI already failed).
		return retrieveKeyword
	case mode == queryv1.SearchMode_VECTOR:
		return retrieveVector
	default:
		return retrieveHybrid
	}
}

// cacheGet returns the cached response for key, or nil on miss or any cache
// failure (Redis down => skip silently, log once).
func (s *server) cacheGet(ctx context.Context, logger *slog.Logger, key string) *queryv1.SearchResponse {
	data, ok, err := s.cache.Get(ctx, key)
	if err != nil {
		s.logCacheError(logger, "get", err)
		return nil
	}
	if !ok {
		return nil
	}
	resp := &queryv1.SearchResponse{}
	if err := proto.Unmarshal(data, resp); err != nil {
		logger.Debug("cache entry undecodable; ignoring", "error", err)
		return nil
	}
	return resp
}

// cacheSet stores resp under key with the contract TTL; failures only log.
func (s *server) cacheSet(ctx context.Context, logger *slog.Logger, key string, resp *queryv1.SearchResponse) {
	data, err := proto.Marshal(resp)
	if err != nil {
		logger.Debug("marshal response for cache", "error", err)
		return
	}
	if err := s.cache.Set(ctx, key, data, cacheTTL); err != nil {
		s.logCacheError(logger, "set", err)
	}
}

func (s *server) logCacheError(logger *slog.Logger, op string, err error) {
	warned := false
	s.cacheWarnOnce.Do(func() {
		warned = true
		logger.Warn("result cache unavailable; continuing without it", "op", op, "error", err)
	})
	if !warned {
		logger.Debug("result cache unavailable", "op", op, "error", err)
	}
}
