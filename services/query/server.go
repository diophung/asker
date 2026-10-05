package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/asker/asker/platform/personalization"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	documentv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// server implements queryv1.QueryService. The tenant arrives exclusively via
// the tenancygrpc server interceptor; Search re-checks it and fails closed.
type server struct {
	queryv1.UnimplementedQueryServiceServer

	embed              embedder
	clip               clipEmbedder
	vespa              vespaSearcher
	cache              resultCache
	logger             *slog.Logger
	metrics            *queryMetrics
	searchTimeout      time.Duration
	clipEnabled        bool
	cacheNamespace     string
	resultCacheEnabled bool

	// recencyWeight / recencyHalfLife drive the "most recent, most relevant
	// first" re-rank applied to the retrieved page (rerank.go). Zero weight
	// (the newServer default) leaves the pure relevance order; main.go sets
	// these from config so production blends in freshness.
	recencyWeight   float64
	recencyHalfLife time.Duration

	// profiles, when non-nil, turns ON v3 personalization (per-tenant profile +
	// learned-model re-rank, attention scoring). A nil loader leaves preference
	// ranking off; query understanding still applies. main.go wires the Redis loader.
	profiles profileLoader
	// rrfEnabled fuses a keyword arm and a vector arm with RRF on the
	// retrieval path; candidateCap is how many candidates that path retrieves
	// before re-ranking to the page. Both set from config in main.go.
	rrfEnabled   bool
	candidateCap int32

	// reranker, when non-nil, turns ON the cross-encoder rerank pass (Phase 1):
	// the top rerankCandidates fused candidates are re-scored by the reranker
	// before the final ordering. nil (the newServer default) leaves the pipeline
	// unchanged. rerankCandidates is the rerank depth; rerankDocChars caps the
	// passage sent per candidate. main.go wires these from config when
	// QUERY_RERANK_ENABLED. A request opts in per-call via SearchRequest.rerank.
	reranker         reranker
	rerankCandidates int
	rerankDocChars   int

	// cacheWarnOnce gates the loud log for a down Redis: the contract is
	// "skip silently (log once)" — first failure warns, the rest are debug.
	cacheWarnOnce sync.Once
	// clipWarnOnce gates the loud log for a down clip service: degradation is
	// "log once" (ADR-006) — first failure warns, the rest are debug.
	clipWarnOnce sync.Once
	// rerankWarnOnce gates the loud log for a down reranker service: degradation
	// is "log once" — first failure warns, the rest are debug.
	rerankWarnOnce sync.Once
}

func newServer(embed embedder, clip clipEmbedder, vespa vespaSearcher, cache resultCache, logger *slog.Logger) *server {
	return &server{embed: embed, clip: clip, vespa: vespa, cache: cache, logger: logger, metrics: newQueryMetrics(), searchTimeout: 4500 * time.Millisecond, clipEnabled: true, resultCacheEnabled: true}
}

// Search runs the spec §2.6 pipeline: validate/normalize -> understand ->
// cache -> embed -> Vespa (with the degradation ladder) -> respond.
func (s *server) Search(ctx context.Context, req *queryv1.SearchRequest) (response *queryv1.SearchResponse, searchErr error) {
	start := time.Now()
	budget := s.searchTimeout
	if budget <= 0 {
		budget = 4500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	rerankApplied := false
	var candidateDebug []byte
	defer func() {
		_ = grpc.SetHeader(ctx, metadata.Pairs("x-asker-rerank-applied", fmt.Sprint(rerankApplied)))
		if candidateDebug != nil {
			_ = grpc.SetHeader(ctx, metadata.Pairs("x-asker-candidates-bin", string(candidateDebug)))
		}
		if searchErr != nil {
			s.metrics.recordSearch(ctx, float64(time.Since(start).Milliseconds()), req.GetMode().String(), "none", "miss", "error")
		}
	}()

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
	s.metrics.recordStage(ctx, "understand", stage, plan.SyntaxError != "")
	if plan.SyntaxError != "" {
		return nil, status.Errorf(codes.InvalidArgument, "query: %s", plan.SyntaxError)
	}
	if !plan.From.IsZero() && !plan.To.IsZero() && plan.From.After(plan.To) {
		return nil, status.Error(codes.InvalidArgument, "query: from date is after to date")
	}
	logger.Debug("stage understand",
		"took", time.Since(stage),
		"residual_chars", len(plan.Text),
		"doc_types", len(plan.DocTypes),
		"has_participant", plan.Participant != "",
		"mode", norm.GetMode().String())

	// Stage 2b (v3): load the tenant's personalization profile + learned model
	// used by preference ranking and timezone grounding. A nil loader uses defaults.
	personalizing := s.profiles != nil
	profile := personalization.DefaultProfile()
	var model personalization.LearnedModel
	var profileVersion int64
	if personalizing {
		profileStart := time.Now()
		profile, model = s.profiles.Load(ctx, tc.TenantID())
		s.metrics.recordStage(ctx, "profile", profileStart, ctx.Err() != nil)
		profileVersion = profile.Version
		logger.Debug("stage scope", "intent", plan.Intent.String(),
			"event_window", !plan.EventFrom.IsZero(), "profile_version", profileVersion)
	}
	// Intent and dates are search semantics, independent of optional ranking preferences.
	plan = scopeQuery(plan, profile, now)
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}

	// A schedule/needs-attention intent legitimately drives retrieval with no
	// query terms (it lists a window / scans for salience), so an empty residual
	// is only an error when no intent and no filters back it.
	if plan.Text == "" && !plan.hasFilters() && !intentDrivenEmptyText(plan) {
		return nil, status.Error(codes.InvalidArgument, "query: empty query with no filters")
	}

	// The CLIP text->image arm (ADR-013) is part of the plan for a HYBRID
	// query that has residual text. This is deterministic from the request, so
	// it is folded into the cache key (a CLIP-planning request never collides
	// with a CLIP-less one) and decides whether stage 4b runs.
	mode := norm.GetMode()
	clipPlanned := s.clipEnabled && plan.Text != "" && mode == queryv1.SearchMode_HYBRID && permitsMedia(plan.DocTypes)

	// The cross-encoder rerank pass (Phase 1) fires only when a reranker is
	// wired, the caller opted in (SearchRequest.rerank), and the request is a
	// HYBRID search with residual query text (there is nothing to re-score for a
	// pure filter/schedule lookup). It re-scores the top fused candidates before
	// the final ordering; a reranker failure degrades to the fused order.
	rerankActive := s.reranker != nil && norm.GetRerank() &&
		mode == queryv1.SearchMode_HYBRID && plan.Text != ""

	// Stage 3: result cache. A clean hit serves immediately; a miss (including a
	// Redis outage, which degrades to "no cache") falls through to retrieval.
	// The cache outcome is both a metric label on the search-duration histogram
	// and its own counter so a dashboard can read hit ratio directly.
	stage = time.Now()
	key, keyErr := s.boundCacheKey(cacheKey(tc.TenantID(), norm, clipPlanned, profileVersion), plan, personalizing, rerankActive, norm.GetDebug(), profile, model)
	// A malformed ranking input cannot share a cache entry with a valid one.
	bypassCache := keyErr != nil || !s.resultCacheEnabled
	for _, value := range metadata.ValueFromIncomingContext(ctx, "x-asker-cache-bypass") {
		if value == "true" {
			bypassCache = true
		}
	}
	if !bypassCache {
		cached, cacheErr := s.cacheGet(ctx, logger, key)
		s.metrics.recordStage(ctx, "cache", stage, cacheErr != nil || ctx.Err() != nil)
		if cached != nil {
			if ctx.Err() != nil {
				return nil, status.FromContextError(ctx.Err()).Err()
			}
			cached.Cached = true
			cached.TookMs = time.Since(start).Milliseconds()
			logger.Debug("stage cache", "took", time.Since(stage), "hit", true)
			s.metrics.recordCache(ctx, "hit")
			s.metrics.recordSearch(ctx, float64(time.Since(start).Milliseconds()), mode.String(), "", "hit", "ok")
			return cached, nil
		}
	}
	s.metrics.recordCache(ctx, "miss")
	logger.Debug("stage cache", "took", time.Since(stage), "hit", false)
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	// Stage 4: embed the residual text (HYBRID/VECTOR only). Ladder rung 1:
	// a TEI failure in HYBRID degrades to keyword-only; VECTOR mode errors.
	degradedReasons := append([]string(nil), plan.Warnings...)
	if norm.GetRerank() && !rerankActive {
		if s.reranker == nil {
			degradedReasons = addDegraded(degradedReasons, degradedRerankNotEnabled)
		} else {
			degradedReasons = addDegraded(degradedReasons, degradedRerankNotApplicable)
		}
	}
	var vector []float32
	var clipVector []float32
	type embeddingResult struct {
		vector []float32
		err    error
	}
	textDone := make(chan embeddingResult, 1)
	clipDone := make(chan embeddingResult, 1)
	textPlanned := plan.Text != "" && (mode == queryv1.SearchMode_HYBRID || mode == queryv1.SearchMode_VECTOR)
	if textPlanned {
		go func() {
			started := time.Now()
			v, err := s.embed.Embed(ctx, plan.Text)
			s.metrics.recordStage(ctx, "embed", started, err != nil)
			textDone <- embeddingResult{v, err}
		}()
	}
	if clipPlanned {
		go func() {
			started := time.Now()
			v, err := s.clip.EmbedText(ctx, plan.Text)
			s.metrics.recordStage(ctx, "clip", started, err != nil)
			clipDone <- embeddingResult{v, err}
		}()
	}
	if plan.Text != "" && (mode == queryv1.SearchMode_HYBRID || mode == queryv1.SearchMode_VECTOR) {
		stage = time.Now()
		var embedded embeddingResult
		select {
		case embedded = <-textDone:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		v, embedErr := embedded.vector, embedded.err
		logger.Debug("stage embed", "took", time.Since(stage), "error", embedErr != nil)
		switch {
		case embedErr == nil:
			vector = v
		case mode == queryv1.SearchMode_VECTOR:
			// Record the (brownout) latency so the P90 SLO alert sees failed
			// searches, not just successes (M5 review).
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
	if clipPlanned {
		stage = time.Now()
		var embedded embeddingResult
		select {
		case embedded = <-clipDone:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		cv, clipErr := embedded.vector, embedded.err
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
		TextClauses: plan.TextClauses,
		ToExclusive: plan.ToExclusive,
		EventFrom:   plan.EventFrom, // event_start window for schedule lookups (v3)
		EventTo:     plan.EventTo,
	}
	limit, offset := norm.GetLimit(), norm.GetOffset()

	var result vespaResult
	retrievalRecorded := false
	stage = time.Now()
	if personalizing || s.rrfEnabled || rerankActive {
		// Retrieve a bounded candidate population at offset 0, then rank and page.
		var candidates vespaResult
		base.Hits = min(int32(1000), max(s.candidateCap, limit+offset))
		candidates, err = s.retrievePersonalized(ctx, logger, base, plan, mode, vector, clipActive, clipVector, &degradedReasons)
		s.metrics.recordStage(ctx, "retrieve", stage, err != nil)
		retrievalRecorded = true
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		if err == nil && personalizing {
			// Eligibility is a hard predicate and belongs before model inference,
			// so muted/out-of-window candidates never consume rerank depth.
			candidates.Hits = filterCandidates(candidates.Hits, personalizeParams{
				ctx: ctx, profile: profile, intent: plan.Intent,
				window: timeWindow{From: plan.WinFrom, To: plan.WinTo}, now: now,
			})
		}
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		if err == nil && norm.GetDebug() && rerankActive {
			candidateDebug = encodeCandidateDebug(candidates.Hits, s.rerankCandidates)
		}
		if err == nil && rerankActive && len(candidates.Hits) > 0 {
			// Re-score the top fused candidates with the cross-encoder and reorder
			// them; personalizeRank then reads the improved relevance via its
			// Semantic feature (Hit.Score). A reranker failure drops the pass.
			stageRerank := time.Now()
			// Correction is restricted to soft content. Hard phrase/exclusion,
			// source/person/date scopes and unsupported inline syntax stay literal.
			correctTypos := !plan.hasFilters() && len(plan.Warnings) == 0
			reranked, rErr := applyRerank(ctx, s.reranker, plan.Text, candidates.Hits, candidates.Passages, correctTypos, s.rerankCandidates, s.rerankDocChars)
			s.metrics.recordStage(ctx, "rerank", stageRerank, rErr != nil)
			logger.Debug("stage rerank", "took", time.Since(stageRerank), "error", rErr != nil, "candidates", len(candidates.Hits))
			if ctx.Err() != nil {
				return nil, status.FromContextError(ctx.Err()).Err()
			}
			if rErr != nil {
				s.logRerankError(logger, rErr)
				degradedReasons = addDegraded(degradedReasons, degradedRerankUnavailable)
			} else {
				candidates.Hits = reranked
				rerankApplied = len(candidates.Hits) > 0
			}
		}
		if err == nil && personalizing {
			halfLife := s.recencyHalfLife
			if halfLife <= 0 {
				halfLife = defaultPersonalizationHalfLife
			}
			page, total := personalizeRank(candidates.Hits, personalizeParams{
				ctx:      ctx,
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
		} else if err == nil {
			rerankByRecency(candidates.Hits, s.recencyWeight, s.recencyHalfLife, now)
			// Total describes this bounded ranking population, not documents that
			// were never retrieved/scored and cannot appear in its pages.
			result = vespaResult{Hits: pageHits(candidates.Hits, offset, limit), Total: int64(len(candidates.Hits))}
		}
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
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
	if !retrievalRecorded {
		s.metrics.recordStage(ctx, "retrieve", stage, err != nil)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		if errors.Is(err, errInvalidFilterValue) {
			return nil, status.Errorf(codes.InvalidArgument, "query: %v", err)
		}
		// A search-backend (Vespa) failure: record the latency it consumed so a
		// slow-error brownout is visible to the P90 SLO alert (M5 review).
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
	if !personalizing && !s.rrfEnabled && !rerankActive {
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
	if degraded == "" && !bypassCache && ctx.Err() == nil {
		s.cacheSet(ctx, logger, key, resp)
	}
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	// This is a computed (cache-miss) result; the search-duration histogram is
	// labeled cache="miss" here, cache="hit" on the early cached return above.
	resp.TookMs = time.Since(start).Milliseconds()
	s.metrics.recordSearch(ctx, float64(resp.TookMs), mode.String(), degraded, "miss", "ok")
	logger.Debug("search complete",
		"took", time.Since(start), "hits", len(resp.Hits), "total", resp.Total, "degraded", degraded)
	return resp, nil
}

// Count reports how many documents are indexed for the calling tenant (the
// live "indexed" figure for the Settings indexing-progress view). It is a
// filter-only Vespa query with hits=0, so Vespa returns just the tenant group's
// total match count — no documents, no ranking. The tenant comes from the
// verified gRPC metadata, never the request.
func (s *server) Count(ctx context.Context, _ *queryv1.CountRequest) (*queryv1.CountResponse, error) {
	tc, err := tenancy.FromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "query: no tenant in request context")
	}
	res, err := s.vespa.Search(ctx, vespaQuery{
		Tenant: tc.TenantID(), // from verified ctx — NEVER from the request
		Kind:   retrieveFilterOnly,
		Hits:   0,
	})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "query: count: %v", err)
	}
	return &queryv1.CountResponse{Indexed: res.Total}, nil
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
	clipQ.Hits = prefix
	clipQ.Offset = 0
	arms, err := s.parallelSearch(ctx, []vespaQuery{textQ, clipQ}, true)
	if err != nil {
		return vespaResult{}, err
	}
	textResult, clipResult, clipErr := arms[0].result, arms[1].result, arms[1].err
	if arms[0].keywordFallback {
		*degradedReasons = addDegraded(*degradedReasons, degradedKeywordOnly)
	}
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

type retrievalArmResult struct {
	result          vespaResult
	err             error
	keywordFallback bool
}

// Independent arms share the caller's budget and cancellation. Buffered result
// channels let canceled HTTP calls finish without blocking abandoned senders.
func (s *server) parallelSearch(ctx context.Context, queries []vespaQuery, primaryFallback bool) ([]retrievalArmResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	channels := make([]chan retrievalArmResult, len(queries))
	for i, query := range queries {
		ch := make(chan retrievalArmResult, 1)
		channels[i] = ch
		go func(i int, q vespaQuery) {
			var arm retrievalArmResult
			if ctx.Err() != nil {
				arm.err = ctx.Err()
			} else if i == 0 && primaryFallback {
				arm.result, arm.keywordFallback, arm.err = s.searchWithDegradation(ctx, q)
			} else {
				arm.result, arm.err = s.vespa.Search(ctx, q)
			}
			ch <- arm
		}(i, query)
	}
	arms := make([]retrievalArmResult, len(queries))
	for i, ch := range channels {
		select {
		case arms[i] = <-ch:
			if i == 0 && arms[i].err != nil {
				return nil, arms[i].err
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return arms, nil
}

func permitsMedia(types []documentv1.DocType) bool {
	if len(types) == 0 {
		return true
	}
	for _, typ := range types {
		if typ == documentv1.DocType_IMAGE || typ == documentv1.DocType_AUDIO || typ == documentv1.DocType_VIDEO {
			return true
		}
	}
	return false
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

// cacheGet returns nil on miss or failure. Errors are measured but remain
// non-fatal (Redis down => skip silently, log once).
func (s *server) cacheGet(ctx context.Context, logger *slog.Logger, key string) (*queryv1.SearchResponse, error) {
	data, ok, err := s.cache.Get(ctx, key)
	if err != nil {
		s.logCacheError(logger, "get", err)
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	resp := &queryv1.SearchResponse{}
	if err := proto.Unmarshal(data, resp); err != nil {
		logger.Debug("cache entry undecodable; ignoring", "error", err)
		return nil, err
	}
	return resp, nil
}

// cacheSet stores resp under key with the contract TTL; failures only log.
func (s *server) cacheSet(ctx context.Context, logger *slog.Logger, key string, resp *queryv1.SearchResponse) {
	start := time.Now()
	failed := false
	defer func() { s.metrics.recordStage(ctx, "cache", start, failed) }()
	data, err := proto.Marshal(resp)
	if err != nil {
		failed = true
		logger.Debug("marshal response for cache", "error", err)
		return
	}
	if err := s.cache.Set(ctx, key, data, cacheTTL); err != nil {
		failed = true
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

// logRerankError reports a dropped cross-encoder rerank pass: loud once (the
// reranker service is unavailable or misbehaving), quiet thereafter. The fused
// retrieval order still serves (Phase 1: never fail closed on rerank).
func (s *server) logRerankError(logger *slog.Logger, err error) {
	warned := false
	s.rerankWarnOnce.Do(func() {
		warned = true
		logger.Warn("cross-encoder rerank unavailable; serving fused order", "error", err)
	})
	if !warned {
		logger.Debug("cross-encoder rerank unavailable", "error", err)
	}
}
