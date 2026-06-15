package main

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/asker/asker/platform/personalization"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	"github.com/asker/asker/platform/tenancy"
)

// server implements queryv1.QueryService. The tenant arrives exclusively via
// the tenancygrpc server interceptor; Search re-checks it and fails closed.
type server struct {
	queryv1.UnimplementedQueryServiceServer

	embed   embedder
	clip    clipEmbedder
	vespa   vespaSearcher
	cache   resultCache
	logger  *slog.Logger
	metrics *queryMetrics

	// recencyWeight / recencyHalfLife drive the "most recent, most relevant
	// first" re-rank applied to the retrieved page (rerank.go). Zero weight
	// (the newServer default) leaves the pure relevance order; main.go sets
	// these from config so production blends in freshness.
	recencyWeight   float64
	recencyHalfLife time.Duration

	// profiles, when non-nil, turns ON v3 personalization (per-tenant profile +
	// learned-model re-rank, query understanding, attention scoring). nil — the
	// newServer default used by every existing test — runs the non-personalized
	// pipeline byte-for-byte unchanged. main.go wires the Redis loader.
	profiles profileLoader
	// rrfEnabled fuses a keyword arm and a vector arm with RRF on the
	// personalized path; candidateCap is how many candidates that path retrieves
	// before re-ranking to the page. Both set from config in main.go.
	rrfEnabled   bool
	candidateCap int32

	// cacheWarnOnce gates the loud log for a down Redis: the contract is
	// "skip silently (log once)" — first failure warns, the rest are debug.
	cacheWarnOnce sync.Once
	// clipWarnOnce gates the loud log for a down clip service: degradation is
	// "log once" (ADR-006) — first failure warns, the rest are debug.
	clipWarnOnce sync.Once
}

func newServer(embed embedder, clip clipEmbedder, vespa vespaSearcher, cache resultCache, logger *slog.Logger) *server {
	return &server{embed: embed, clip: clip, vespa: vespa, cache: cache, logger: logger, metrics: newQueryMetrics()}
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
	now := time.Now()
	stage := now
	norm := normalizeRequest(req)
	plan := understand(norm)
	logger.Debug("stage understand",
		"took", time.Since(stage),
		"residual_chars", len(plan.Text),
		"doc_types", len(plan.DocTypes),
		"has_participant", plan.Participant != "",
		"mode", norm.GetMode().String())

	// Stage 2b (v3): load the tenant's personalization profile + learned model
	// and enrich the plan with intent + temporal scope. A nil loader leaves
	// `personalizing` false and the pipeline below runs exactly as before.
	personalizing := s.profiles != nil
	var profile personalization.Profile
	var model personalization.LearnedModel
	var profileVersion int64
	if personalizing {
		profile, model = s.profiles.Load(ctx, tc.TenantID())
		profileVersion = profile.Version
		plan = scopeQuery(plan, profile, now)
		logger.Debug("stage scope", "intent", plan.Intent.String(),
			"event_window", !plan.EventFrom.IsZero(), "profile_version", profileVersion)
	}

	// A schedule/needs-attention intent legitimately drives retrieval with no
	// query terms (it lists a window / scans for salience), so an empty residual
	// is only an error when no intent and no filters back it.
	if plan.Text == "" && !plan.hasFilters() && (!personalizing || !intentDrivenEmptyText(plan)) {
		return nil, status.Error(codes.InvalidArgument, "query: empty query with no filters")
	}

	// The CLIP text->image arm (ADR-013) is part of the plan for a HYBRID
	// query that has residual text. This is deterministic from the request, so
	// it is folded into the cache key (a CLIP-planning request never collides
	// with a CLIP-less one) and decides whether stage 4b runs.
	mode := norm.GetMode()
	clipPlanned := plan.Text != "" && mode == queryv1.SearchMode_HYBRID

	// Stage 3: result cache. A clean hit serves immediately; a miss (including a
	// Redis outage, which degrades to "no cache") falls through to retrieval.
	// The cache outcome is both a metric label on the search-duration histogram
	// and its own counter so a dashboard can read hit ratio directly.
	stage = time.Now()
	key := cacheKey(tc.TenantID(), norm, clipPlanned, profileVersion)
	if cached := s.cacheGet(ctx, logger, key); cached != nil {
		cached.Cached = true
		cached.TookMs = time.Since(start).Milliseconds()
		logger.Debug("stage cache", "took", time.Since(stage), "hit", true)
		s.metrics.recordCache(ctx, "hit")
		s.metrics.recordSearch(ctx, float64(time.Since(start).Milliseconds()), mode.String(), "", "hit", "ok")
		return cached, nil
	}
	s.metrics.recordCache(ctx, "miss")
	logger.Debug("stage cache", "took", time.Since(stage), "hit", false)

	// Stage 4: embed the residual text (HYBRID/VECTOR only). Ladder rung 1:
	// a TEI failure in HYBRID degrades to keyword-only; VECTOR mode errors.
	var degradedReasons []string
	var vector []float32
	if plan.Text != "" && (mode == queryv1.SearchMode_HYBRID || mode == queryv1.SearchMode_VECTOR) {
		stage = time.Now()
		v, embedErr := s.embed.Embed(ctx, plan.Text)
		logger.Debug("stage embed", "took", time.Since(stage), "error", embedErr != nil)
		switch {
		case embedErr == nil:
			vector = v
		case mode == queryv1.SearchMode_VECTOR:
			// Record the (brownout) latency so the P90 SLO alert sees failed
			// searches, not just successes (M5 review).
			s.metrics.recordSearch(ctx, float64(time.Since(start).Milliseconds()), mode.String(), "none", "miss", "error")
			return nil, status.Errorf(codes.Unavailable, "query: embedding unavailable in VECTOR mode: %v", embedErr)
		default:
			if errors.Is(embedErr, errEmbedDim) {
				logger.Error("EMBEDDING_DIM mismatch — fix the deployment (ADR-005)", "error", embedErr)
			} else {
				logger.Warn("query embedding failed; degrading to keyword-only", "error", embedErr)
			}
			degradedReasons = addDegraded(degradedReasons, degradedKeywordOnly)
		}
	}

	// Stage 4b: CLIP text->image embedding (ADR-013). Only the blended HYBRID
	// mode runs the CLIP arm — KEYWORD/VECTOR are caller-constrained strategies
	// that must behave exactly as before. A clip failure drops the arm
	// (degraded="clip-unavailable"); it never blocks text retrieval.
	clipActive := false
	var clipVector []float32
	if clipPlanned {
		stage = time.Now()
		cv, clipErr := s.clip.EmbedText(ctx, plan.Text)
		logger.Debug("stage clip-embed", "took", time.Since(stage), "error", clipErr != nil)
		if clipErr != nil {
			s.logClipError(logger, clipErr)
			degradedReasons = addDegraded(degradedReasons, degradedClipUnavailable)
		} else {
			clipActive = true
			clipVector = cv
		}
	}

	// Stage 5+6: Vespa retrieval with the degradation ladder.
	base := vespaQuery{
		Tenant:      tc.TenantID(), // from verified ctx — NEVER from the request
		Text:        plan.Text,
		DocTypes:    plan.DocTypes,
		From:        plan.From,
		To:          plan.To,
		Participant: plan.Participant,
		EventFrom:   plan.EventFrom, // event_start window for schedule lookups (v3)
		EventTo:     plan.EventTo,
	}
	limit, offset := norm.GetLimit(), norm.GetOffset()

	var result vespaResult
	stage = time.Now()
	if personalizing {
		// Personalized path: retrieve a wide candidate set (RRF-fused arms when
		// enabled) at offset 0, then re-rank to the page by the combined score.
		var candidates vespaResult
		candidates, err = s.retrievePersonalized(ctx, logger, base, plan, mode, vector, clipActive, clipVector, &degradedReasons)
		if err == nil {
			halfLife := s.recencyHalfLife
			if halfLife <= 0 {
				halfLife = defaultPersonalizationHalfLife
			}
			page, total := personalizeRank(candidates.Hits, personalizeParams{
				profile:  profile,
				model:    model,
				intent:   plan.Intent,
				window:   timeWindow{From: plan.WinFrom, To: plan.WinTo},
				now:      now,
				halfLife: halfLife,
				debug:    norm.GetDebug(),
				limit:    limit,
				offset:   offset,
			})
			result = vespaResult{Hits: page, Total: total}
		}
	} else if clipActive {
		// Two arms merged: each arm must contribute its full prefix up to
		// offset+limit (with offset 0), so the merged ranking is correct
		// before the page is sliced. The text arm still degrades on its own
		// ladder; a CLIP arm failure here drops the arm (never fail closed).
		textQ := base
		textQ.Kind = retrievalPlan(plan, mode, vector)
		textQ.Vector = vector
		clipQ := base
		clipQ.Kind = retrieveCLIP
		clipQ.ClipVector = clipVector
		result, err = s.searchMerged(ctx, logger, textQ, clipQ, limit, offset, &degradedReasons)
	} else {
		textQ := base
		textQ.Kind = retrievalPlan(plan, mode, vector)
		textQ.Vector = vector
		textQ.Hits = limit
		textQ.Offset = offset
		var keywordFallback bool
		result, keywordFallback, err = s.searchWithDegradation(ctx, textQ)
		if keywordFallback {
			degradedReasons = addDegraded(degradedReasons, degradedKeywordOnly)
		}
	}
	logger.Debug("stage vespa", "took", time.Since(stage), "personalized", personalizing, "clip_arm", clipActive, "error", err != nil)
	if err != nil {
		if errors.Is(err, errInvalidFilterValue) {
			return nil, status.Errorf(codes.InvalidArgument, "query: %v", err)
		}
		// A search-backend (Vespa) failure: record the latency it consumed so a
		// slow-error brownout is visible to the P90 SLO alert (M5 review).
		s.metrics.recordSearch(ctx, float64(time.Since(start).Milliseconds()), mode.String(), joinDegraded(degradedReasons), "miss", "error")
		return nil, status.Errorf(codes.Unavailable, "query: search backend: %v", err)
	}

	degraded := joinDegraded(degradedReasons)

	// Count each distinct degradation rung that fired (the reasons slice is
	// already deduped) so a dashboard tracks keyword-only / clip-unavailable
	// rates without parsing the composed marker.
	for _, rung := range degradedReasons {
		s.metrics.recordDegradation(ctx, rung)
	}

	// Stage 6b: re-rank the retrieved page by "most recent, most relevant
	// first" — blend freshness into the relevance order (rerank.go). A no-op
	// when recencyWeight is 0. Applied before caching so a cache hit serves the
	// same blended order (the sub-minute recency drift over the TTL is noise).
	// The personalized path already folds recency into its combined score
	// (recency-vs-importance slider), so this standalone blend is skipped there.
	if !personalizing {
		rerankByRecency(result.Hits, s.recencyWeight, s.recencyHalfLife, now)
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
	// This is a computed (cache-miss) result; the search-duration histogram is
	// labeled cache="miss" here, cache="hit" on the early cached return above.
	s.metrics.recordSearch(ctx, float64(resp.TookMs), mode.String(), degraded, "miss", "ok")
	logger.Debug("search complete",
		"took", time.Since(start), "hits", len(resp.Hits), "total", resp.Total, "degraded", degraded)
	return resp, nil
}

// searchMerged runs the text arm (with its degradation ladder) and the CLIP
// arm side by side, unions the hits by doc_id, blends scores, and returns the
// requested page of the merged ranking. Each arm is fetched from offset 0 up
// to offset+limit so the merge sees every candidate that could land on the
// page. It owns both degradation appends so the composed marker is
// deterministic ("keyword-only" before "clip-unavailable"): the text arm's
// keyword fallback first, then a dropped CLIP arm (the arm is dropped and the
// text results still return — ADR-006).
func (s *server) searchMerged(
	ctx context.Context, logger *slog.Logger,
	textQ, clipQ vespaQuery, limit, offset int32, degradedReasons *[]string,
) (vespaResult, error) {
	// Each arm needs the full prefix [0, offset+limit) so pagination over the
	// merged set is correct. int32 math is bounded by the request validation
	// (limit<=100, offset<=1000), so no overflow.
	prefix := offset + limit

	textQ.Hits = prefix
	textQ.Offset = 0
	textResult, keywordFallback, textErr := s.searchWithDegradation(ctx, textQ)
	if textErr != nil {
		// The text arm is the backbone: its failure is the search's failure.
		return vespaResult{}, textErr
	}
	if keywordFallback {
		*degradedReasons = addDegraded(*degradedReasons, degradedKeywordOnly)
	}

	clipQ.Hits = prefix
	clipQ.Offset = 0
	clipResult, clipErr := s.vespa.Search(ctx, clipQ)
	if clipErr != nil {
		// Drop the CLIP arm; never fail the search on it (ADR-006).
		s.logClipError(logger, clipErr)
		*degradedReasons = addDegraded(*degradedReasons, degradedClipUnavailable)
		clipResult = vespaResult{}
	}

	merged := mergeHits(textResult.Hits, clipResult.Hits)
	total := mergedTotal(textResult, clipResult, len(merged))
	return vespaResult{Hits: pageHits(merged, offset, limit), Total: total}, nil
}

// mergeHits unions two arms' hits by doc_id and orders the result by blended
// score, descending. A doc matched by BOTH arms keeps the higher of the two
// scores (so an image with matching OCR text AND visual similarity ranks at
// least as well as either alone); a purely-visual or purely-textual match
// keeps its single score. The first arm (text) is authoritative for a hit's
// non-score fields (snippet, metadata), but a text hit that lacks media deep-
// link fields inherits them from the CLIP match (the CLIP arm is what knows
// the visually-matched chunk).
func mergeHits(textHits, clipHits []*queryv1.Hit) []*queryv1.Hit {
	order := make([]string, 0, len(textHits)+len(clipHits))
	byDoc := make(map[string]*queryv1.Hit, len(textHits)+len(clipHits))

	add := func(h *queryv1.Hit) {
		existing, ok := byDoc[h.GetDocId()]
		if !ok {
			order = append(order, h.GetDocId())
			byDoc[h.GetDocId()] = h
			return
		}
		// Keep the higher score (the blend for a doc both arms matched).
		if h.GetScore() > existing.GetScore() {
			existing.Score = h.GetScore()
		}
		// Fill media deep-link fields the text arm did not carry from the
		// CLIP match (visual chunk anchoring).
		if existing.GetModality() == "" && h.GetModality() != "" {
			existing.StartMs = h.GetStartMs()
			existing.EndMs = h.GetEndMs()
			existing.Modality = h.GetModality()
		}
		if existing.GetThumbnailKey() == "" && h.GetThumbnailKey() != "" {
			existing.ThumbnailKey = h.GetThumbnailKey()
		}
	}
	for _, h := range textHits {
		add(h)
	}
	for _, h := range clipHits {
		add(h)
	}

	out := make([]*queryv1.Hit, 0, len(order))
	for _, id := range order {
		out = append(out, byDoc[id])
	}
	// Stable sort by score desc so equal scores keep arrival (text-first) order.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].GetScore() > out[j].GetScore()
	})
	return out
}

// mergedTotal estimates the tenant-scoped match count across both arms. The
// arms' per-arm totalCounts overlap (a doc matched by both is counted twice),
// so the merged distinct count is at least max(arm totals are not additive);
// the count actually observed after dedupe is the most honest lower bound we
// have without a second pass. We report max(distinct observed, each arm's
// total) so the figure never undercounts the dominant (text) arm.
func mergedTotal(textResult, clipResult vespaResult, distinct int) int64 {
	total := int64(distinct)
	if textResult.Total > total {
		total = textResult.Total
	}
	if clipResult.Total > total {
		total = clipResult.Total
	}
	return total
}

// pageHits applies offset/limit to the merged ranking.
func pageHits(hits []*queryv1.Hit, offset, limit int32) []*queryv1.Hit {
	if offset >= int32(len(hits)) {
		return nil
	}
	end := offset + limit
	if end > int32(len(hits)) {
		end = int32(len(hits))
	}
	return hits[offset:end]
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

// logClipError reports a dropped CLIP arm: loud once (a CLIP_DIM mismatch is
// an operator error per ADR-013; any other failure means the clip service is
// unavailable), quiet thereafter.
func (s *server) logClipError(logger *slog.Logger, err error) {
	warned := false
	s.clipWarnOnce.Do(func() {
		warned = true
		if errors.Is(err, errClipDim) {
			logger.Error("CLIP_DIM mismatch — fix the deployment (ADR-013)", "error", err)
		} else {
			logger.Warn("clip text->image arm unavailable; dropping it", "error", err)
		}
	})
	if !warned {
		logger.Debug("clip text->image arm unavailable", "error", err)
	}
}
