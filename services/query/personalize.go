package main

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/asker/asker/platform/personalization"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
)

// defaultPersonalizationHalfLife is the recency-bonus half-life used by the
// personalized re-rank when the server's recencyHalfLife is unset (30 days).
const defaultPersonalizationHalfLife = 720 * time.Hour

// retrievePersonalized fetches the wide candidate set the personalized re-rank
// scores over. It retrieves CandidateCap candidates at offset 0 (the re-rank
// pages afterward) and, when RRF is enabled and both a keyword and a vector arm
// are available, fuses them (plus the CLIP arm) by Reciprocal Rank Fusion;
// otherwise it runs the single-arm retrieval (filter-only schedule/needs-
// attention lookups, keyword fallback, or RRF disabled), optionally fused with
// the CLIP arm.
func (s *server) retrievePersonalized(
	ctx context.Context, logger *slog.Logger,
	base vespaQuery, plan parsedQuery, mode queryv1.SearchMode,
	vector []float32, clipActive bool, clipVector []float32, degradedReasons *[]string,
) (vespaResult, error) {
	candCap := max(s.candidateCap, base.Hits)
	if candCap <= 0 {
		candCap = maxLimit
	}

	if s.rrfEnabled && mode == queryv1.SearchMode_HYBRID && plan.Text != "" && vector != nil {
		return s.retrieveRRF(ctx, logger, base, vector, clipActive, clipVector, candCap, degradedReasons)
	}

	textQ := base
	textQ.Kind = retrievalPlan(plan, mode, vector)
	textQ.Vector = vector
	textQ.Hits = candCap
	textQ.Offset = 0
	if !clipActive {
		textRes, keywordFallback, err := s.searchWithDegradation(ctx, textQ)
		if err != nil {
			return vespaResult{}, err
		}
		if keywordFallback {
			*degradedReasons = addDegraded(*degradedReasons, degradedKeywordOnly)
		}
		return textRes, nil
	}
	clipQ := base
	clipQ.Kind = retrieveCLIP
	clipQ.ClipVector = clipVector
	clipQ.Hits = candCap
	clipQ.Offset = 0
	arms, err := s.parallelSearch(ctx, []vespaQuery{textQ, clipQ}, true)
	if err != nil {
		return vespaResult{}, err
	}
	textRes, clipRes, clipErr := arms[0].result, arms[1].result, arms[1].err
	if arms[0].keywordFallback {
		*degradedReasons = addDegraded(*degradedReasons, degradedKeywordOnly)
	}
	if clipErr != nil {
		s.logClipError(logger, clipErr)
		*degradedReasons = addDegraded(*degradedReasons, degradedClipUnavailable)
		return textRes, nil
	}
	fused := rrfFuse(textRes.Hits, clipRes.Hits)
	return vespaResult{Hits: fused, Total: mergedTotal(textRes, clipRes, len(fused)), Passages: mergeRerankPassages(textRes, clipRes)}, nil
}

// retrieveRRF runs the keyword and vector arms as SEPARATE Vespa queries and
// fuses their (plus the CLIP arm's) ranked lists with RRF (spec §1.4). The
// keyword arm is the backbone — its failure fails the search; the vector and
// CLIP arms degrade independently (drop the arm, never fail closed).
func (s *server) retrieveRRF(
	ctx context.Context, logger *slog.Logger, base vespaQuery,
	vector []float32, clipActive bool, clipVector []float32, candCap int32, degradedReasons *[]string,
) (vespaResult, error) {
	kq := base
	kq.Kind = retrieveKeyword
	kq.Hits = candCap
	kq.Offset = 0
	vq := base
	vq.Kind = retrieveVector
	vq.Vector = vector
	vq.Hits = candCap
	vq.Offset = 0
	queries := []vespaQuery{kq, vq}
	if clipActive {
		cq := base
		cq.Kind = retrieveCLIP
		cq.ClipVector = clipVector
		cq.Hits = candCap
		cq.Offset = 0
		queries = append(queries, cq)
	}
	arms, err := s.parallelSearch(ctx, queries, false)
	if err != nil {
		return vespaResult{}, err
	}
	lists := [][]*queryv1.Hit{arms[0].result.Hits}
	passageResults := []vespaResult{arms[0].result}
	maxTotal := arms[0].result.Total
	for i := 1; i < len(arms); i++ {
		arm := arms[i]
		if arm.err != nil {
			if i == 1 {
				logger.Warn("rrf vector arm failed; fusing keyword-only", "error", arm.err)
				*degradedReasons = addDegraded(*degradedReasons, degradedKeywordOnly)
			} else {
				s.logClipError(logger, arm.err)
				*degradedReasons = addDegraded(*degradedReasons, degradedClipUnavailable)
			}
			continue
		}
		lists = append(lists, arm.result.Hits)
		passageResults = append(passageResults, arm.result)
		if arm.result.Total > maxTotal {
			maxTotal = arm.result.Total
		}
	}

	fused := rrfFuse(lists...)
	total := int64(len(fused))
	if maxTotal > total {
		total = maxTotal
	}
	return vespaResult{Hits: fused, Total: total, Passages: mergeRerankPassages(passageResults...)}, nil
}

// Personalized re-ranking (spec v3.2 §1.7 + §3). This is the stage that turns
// "candidates that mean the right thing" (hybrid retrieval) into "the order THIS
// user cares about". It runs ONLY when a profile loader is wired (the ON path);
// the non-personalized path (server.go, existing tests) is untouched.
//
// Pipeline: hard filters (mute, schedule occurrence window) -> per-candidate
// feature extraction -> combined score (personalization.Score) -> diversify
// (MMR / novelty) -> serial-position ordering -> cognitive-load top-K. Each hit
// gets a "why it ranked" explanation; debug also attaches the feature
// contributions.

// personalizeParams bundles the inputs to a personalized re-rank.
type personalizeParams struct {
	ctx      context.Context
	profile  personalization.Profile
	model    personalization.LearnedModel
	intent   intentClass
	window   timeWindow
	now      time.Time
	halfLife time.Duration
	debug    bool
	limit    int32
	offset   int32
}

// scoredHit is a candidate with its computed features and combined score, plus
// the coarse signals MMR diversifies on.
type scoredHit struct {
	hit            *queryv1.Hit
	features       personalization.Features
	retrievalScore float64 // incoming retrieval/fusion or model-reranked relevance
	combined       float64
	rankScore      float64 // combined relevance or the selected MMR utility
	relNorm        float64 // combined score min-max normalized across the page, for MMR
	mmrApplied     bool
	mmrPenalty     float64 // weighted redundancy penalty at this hit's selection
	docType        string
	sender         string
	reasons        []string
}

// recencyBonusWeight scales the recency-vs-importance slider's contribution to
// the combined score (added on top of the five weighted terms). The slider in
// [0,1] times this is how much pure freshness can lift an item.
const recencyBonusWeight = 0.6

// personalizeRank applies the full personalized re-rank and returns the
// requested page plus the post-filter candidate count (the honest total for the
// personalized result set). hits is the wide candidate set retrieved at offset 0.
func personalizeRank(hits []*queryv1.Hit, p personalizeParams) ([]*queryv1.Hit, int64) {
	candidates := filterCandidates(hits, p)

	// Semantic feature = retrieval relevance, min-max normalized across the page
	// so it is comparable to the other [0,1] features regardless of the ranking
	// profile's raw score scale.
	minScore, maxScore := scoreRange(candidates)

	scored := make([]scoredHit, 0, len(candidates))
	for _, h := range candidates {
		if p.ctx != nil && p.ctx.Err() != nil {
			return nil, 0
		}
		sh := scoreCandidate(h, p, minScore, maxScore)
		scored = append(scored, sh)
	}
	total := int64(len(scored))

	if p.intent == intentScheduleLookup {
		// Time-ordered (spec §1): a calendar lookup lists events chronologically
		// by occurrence time; scores still drive the explanation and the tiebreak.
		sortByEventStart(scored)
	} else {
		// Serial Position: best first. Stable so equal scores keep retrieval order.
		sort.SliceStable(scored, func(i, j int) bool { return scored[i].combined > scored[j].combined })
		// Exploration vs. Exploitation: reserve slots for novel/diverse results.
		if p.profile.NoveltyVsFamiliarity > 0 && len(scored) > 1 {
			normalizeRelevance(scored)
			scored = mmrReorder(p.ctx, scored, p.profile.NoveltyVsFamiliarity)
		}
	}
	if p.ctx != nil && p.ctx.Err() != nil {
		return nil, total
	}

	// Cognitive Load (Hick's/Miller's): for needs-attention, show only the
	// critical few — the attention-sensitivity criterion shrinks the page.
	effLimit := p.limit
	if p.intent == intentNeedsAttention {
		effLimit = attentionPageLimit(p.limit, p.profile.AttentionSensitivity)
	}

	out := make([]*queryv1.Hit, 0, len(scored))
	for i := range scored {
		applyExplanation(&scored[i], p)
		out = append(out, scored[i].hit)
	}
	return pageHits(out, p.offset, effLimit), total
}

// sortByEventStart orders scored hits chronologically by occurrence time
// (metadata["start"]); hits with no parseable start sort last, then by combined
// score so the order is deterministic. Stable.
func sortByEventStart(scored []scoredHit) {
	sort.SliceStable(scored, func(i, j int) bool {
		ti, oki := flexibleTime(scored[i].hit.GetMetadata()["start"])
		tj, okj := flexibleTime(scored[j].hit.GetMetadata()["start"])
		switch {
		case oki && okj:
			if !ti.Equal(tj) {
				return ti.Before(tj)
			}
			return scored[i].combined > scored[j].combined
		case oki != okj:
			return oki // a hit with a start time sorts before one without
		default:
			return scored[i].combined > scored[j].combined
		}
	})
}

// filterCandidates applies the HARD filters: hide muted sources/people, and for
// a schedule lookup keep only events whose occurrence time falls in the window
// (the post-retrieval guard that complements the Vespa event_start filter, so
// the result is correct even against a stub or an un-reindexed document).
// needs-attention additionally drops candidates with zero salience (nothing
// makes them need attention).
func filterCandidates(hits []*queryv1.Hit, p personalizeParams) []*queryv1.Hit {
	out := make([]*queryv1.Hit, 0, len(hits))
	for _, h := range hits {
		if p.ctx != nil && p.ctx.Err() != nil {
			return nil
		}
		md := h.GetMetadata()
		if p.profile.IsMutedSource(h.GetType().String()) {
			continue
		}
		if from := md["from"]; from != "" && p.profile.IsMutedPerson(from) {
			continue
		}
		if p.intent == intentScheduleLookup && !p.window.isZero() {
			start, ok := flexibleTime(md["start"])
			end, _ := flexibleTime(md["end"])
			if !ok || !calendarOverlaps(start, end, p.window) {
				continue
			}
		}
		if p.intent == intentNeedsAttention {
			if scoreAttention(h, p.profile, p.window, p.now).score <= 0 {
				continue
			}
		}
		out = append(out, h)
	}
	return out
}

func calendarOverlaps(start, end time.Time, window timeWindow) bool {
	if !window.To.IsZero() && !start.Before(window.To) {
		return false
	}
	if window.From.IsZero() || !start.Before(window.From) {
		return true
	}
	// Missing/invalid ends fall back to a start-in-window test.
	return end.After(start) && end.After(window.From)
}

// scoreCandidate builds the feature vector and combined score for one hit.
func scoreCandidate(h *queryv1.Hit, p personalizeParams, minScore, maxScore float64) scoredHit {
	md := h.GetMetadata()
	docType := h.GetType().String()
	senders := hitSenders(md)
	text := h.GetTitle() + " " + h.GetSnippet()
	topics := p.profile.MatchedTopics(text)
	mutedTopics := p.profile.MutedTopics(text)

	var f personalization.Features

	// Semantic.
	f.Semantic = normalizedRelevance(h.GetScore(), minScore, maxScore)

	// Preference (explicit Settings) and the reasons it contributes.
	pref, prefReasons := preferenceMatch(p.profile, docType, senders, topics, mutedTopics)
	f.Preference = pref

	// Behavioral (learned model).
	f.Behavioral = p.model.Predict(personalization.FeatureKeys(docType, h.GetConnectorId(), senders, topics))

	// Salience answers an explicit attention request. An imminent event is not
	// a stronger answer to an ordinary content query merely because it is urgent.
	att := scoreAttention(h, p.profile, p.window, p.now)
	if p.intent == intentNeedsAttention {
		f.Attention = att.score
	}

	// Fatigue/repetition: no per-session shown-history is threaded into the
	// stateless query path yet, so this is 0 here; the term is kept in the score
	// so the hook (and the explanation) are in place (DECISIONS D7).
	f.Fatigue = 0

	combined := personalization.Score(f, p.profile.Weights)

	// Recency vs. Importance: the slider adds a freshness bonus on top of the
	// weighted terms (Recency / Mere-Exposure). 0 => pure importance.
	if p.profile.RecencyVsImportance > 0 {
		combined += p.profile.RecencyVsImportance * recencyBonusWeight *
			recencyScore(hitTime(h), p.now, p.halfLife)
	}
	// Profile weights can be very large finite values. Preserve ordinary
	// arithmetic/order, but keep an overflowing sum representable on the API
	// and in MMR normalization; the settings themselves are unchanged.
	if math.IsInf(combined, 0) {
		combined = math.Copysign(math.MaxFloat64, combined)
	}

	var reasons []string
	if p.intent == intentNeedsAttention {
		reasons = append(reasons, att.reasons...)
	}
	reasons = append(reasons, prefReasons...)
	if f.Behavioral >= behavioralReasonThreshold {
		reasons = append(reasons, "matches your usual activity")
	}

	return scoredHit{
		hit:            h,
		features:       f,
		retrievalScore: h.GetScore(),
		combined:       combined,
		rankScore:      combined,
		docType:        docType,
		sender:         firstNonEmpty(senders),
		reasons:        reasons,
	}
}

const (
	// behavioralReasonThreshold is the learned-affinity level above which we tell
	// the user the result matches their usual activity.
	behavioralReasonThreshold = 0.62
)

// preferenceMatch computes the explicit-Settings contribution in [-1,1] and the
// reasons behind it: source weight, important people, topic affinity, muted
// topics (Social Proof / topical affinity / negative weights).
func preferenceMatch(profile personalization.Profile, docType string, senders, topics, mutedTopics []string) (float64, []string) {
	var pref float64
	var reasons []string

	// Source weight: 1.0 is neutral; >1 boosts, <1 demotes.
	if w := profile.SourceWeight(docType); w != 1.0 {
		pref += (w - 1.0) * 0.5
		if w > 1.0 {
			reasons = append(reasons, "a source you prioritize")
		}
	}

	// Important people (Social Proof / Authority).
	if profile.IsImportantPerson(senders...) {
		pref += 0.5
		reasons = append(reasons, "from someone important to you")
	}

	// Topic affinity (topical boost).
	if len(topics) > 0 {
		pref += 0.3
		reasons = append(reasons, "about "+topics[0])
	}

	// Muted topics down-rank (kept visible, just demoted — negative weight).
	if len(mutedTopics) > 0 {
		pref -= 0.5
	}

	return clampPref(pref), reasons
}

// applyExplanation publishes the score that selected this hit, its explanation,
// and (when debug) both the term contributions and the earlier scoring stages.
// This runs after the ranking cancellation guard, so MMR never mutates a hit
// while deciding its order. Schedule lookups use combined relevance as their
// score/tiebreak but retain the explicit chronological ordering.
func applyExplanation(sh *scoredHit, p personalizeParams) {
	sh.hit.Score = sh.rankScore
	sh.hit.Explanation = composeExplanation(sh.reasons)
	if p.debug {
		contribs := personalization.Contributions(sh.features, p.profile.Weights)
		m := make(map[string]float64, len(contribs)+5)
		for _, c := range contribs {
			m[c.Name] = c.Value
		}
		m["retrieval_score"] = sh.retrievalScore
		m["combined_score"] = sh.combined
		m["ranking_score"] = sh.rankScore
		if sh.mmrApplied {
			m["mmr_relevance"] = sh.relNorm
			m["mmr_penalty"] = sh.mmrPenalty
		}
		sh.hit.Features = m
	}
}

// composeExplanation joins up to maxExplanationReasons distinct reasons with
// " · ", newest/most-actionable first (attention reasons were appended first).
func composeExplanation(reasons []string) string {
	const maxExplanationReasons = 3
	seen := make(map[string]bool, len(reasons))
	out := make([]string, 0, maxExplanationReasons)
	for _, r := range reasons {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
		if len(out) >= maxExplanationReasons {
			break
		}
	}
	if len(out) == 0 {
		return "strong match for your query"
	}
	return strings.Join(out, " · ")
}

// mmrReorder greedily reorders by Maximal Marginal Relevance: it balances the
// combined relevance against dissimilarity to the already-selected items, so a
// run of near-identical passages is broken up. lambda is
// the novelty rate in (0,1]: 0 keeps pure relevance order, 1 maximizes novelty.
// Token overlap is a conservative, embedding-free redundancy signal. Shared
// type/sender can strengthen overlapping passages, but cannot make two distinct
// answers duplicates just because both are files or came from the same person.
func mmrReorder(ctx context.Context, scored []scoredHit, novelty float64) []scoredHit {
	n := len(scored)
	selected := make([]scoredHit, 0, n)
	used := make([]bool, n)
	tokens := make([]map[string]struct{}, n)
	maxSimilarity := make([]float64, n)
	for i, sh := range scored {
		if ctx != nil && ctx.Err() != nil {
			return scored
		}
		tokens[i] = redundancyTokens(sh.hit.GetTitle() + " " + stripHighlights(sh.hit.GetSnippet()))
	}

	for len(selected) < n {
		if ctx != nil && ctx.Err() != nil {
			return scored // the caller returns the context error, never partial ranking
		}
		bestIdx, bestVal := -1, 0.0
		for i := range scored {
			if used[i] {
				continue
			}
			val := (1-novelty)*scored[i].relNorm - novelty*maxSimilarity[i]
			if bestIdx < 0 || val > bestVal {
				bestIdx, bestVal = i, val
			}
		}
		used[bestIdx] = true
		sh := scored[bestIdx]
		// Every remaining utility can only decrease as maxSimilarity grows,
		// so these selected utilities follow the existing greedy order without
		// re-sorting or inventing an ordinal score after diversification.
		sh.rankScore = bestVal
		sh.mmrApplied = true
		sh.mmrPenalty = novelty * maxSimilarity[bestIdx]
		selected = append(selected, sh)
		for i := range scored {
			if used[i] {
				continue
			}
			sim := tokenJaccard(tokens[bestIdx], tokens[i])
			// Source/sender are tie-breaking diversity context, gated by actual
			// content overlap. Similarity stays in [0,1].
			contextWeight := 0.5
			if sh.docType == scored[i].docType {
				contextWeight += 0.25
			}
			if sh.sender != "" && sh.sender == scored[i].sender {
				contextWeight += 0.25
			}
			maxSimilarity[i] = max(maxSimilarity[i], sim*contextWeight)
		}
	}
	return selected
}

// Bound token work independently of corpus size; no additional embedding call
// is needed for diversification. Unicode letters/numbers preserve non-English
// passages, and empty/missing text provides no redundancy evidence.
func redundancyTokens(text string) map[string]struct{} {
	const maxTokens = 128
	tokens := make(map[string]struct{})
	for _, token := range strings.FieldsFunc(strings.ToLower(truncateRunes(text, 2048)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if len([]rune(token)) < 2 {
			continue
		}
		tokens[token] = struct{}{}
		if len(tokens) == maxTokens {
			break
		}
	}
	return tokens
}

func tokenJaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	intersection := 0
	for token := range a {
		if _, ok := b[token]; ok {
			intersection++
		}
	}
	return float64(intersection) / float64(len(a)+len(b)-intersection)
}

// normalizeRelevance fills relNorm (combined score min-max normalized) for MMR.
func normalizeRelevance(scored []scoredHit) {
	if len(scored) == 0 {
		return
	}
	lo, hi := scored[0].combined, scored[0].combined
	for _, s := range scored {
		if s.combined < lo {
			lo = s.combined
		}
		if s.combined > hi {
			hi = s.combined
		}
	}
	for i := range scored {
		scored[i].relNorm = normalizedRelevance(scored[i].combined, lo, hi)
	}
}

// normalizedRelevance keeps the usual min-max arithmetic except when finite
// opposite-sign bounds overflow their span. Halving all three operands leaves
// the ratio unchanged while keeping both differences representable.
func normalizedRelevance(value, lo, hi float64) float64 {
	span := hi - lo
	if span <= 0 {
		return 1
	}
	if math.IsInf(span, 1) {
		value, lo, hi = value/2, lo/2, hi/2
		span = hi - lo
	}
	return (value - lo) / span
}

// attentionPageLimit shrinks the page for the needs-attention intent per the
// Signal-Detection criterion: higher sensitivity ("only the critical few") =>
// fewer results, trading misses for fewer false alarms. Always at least
// minAttentionResults so the page is never empty when matches exist.
func attentionPageLimit(limit int32, sensitivity float64) int32 {
	const minAttentionResults = 3
	// Scale from full limit (sensitivity 0) down to ~1/4 (sensitivity 1).
	scaled := float64(limit) * (1 - 0.75*sensitivity)
	out := int32(scaled)
	if out < minAttentionResults {
		out = minAttentionResults
	}
	if out > limit {
		out = limit
	}
	return out
}

// hitSenders extracts the candidate's sender address(es) for behavioral/
// preference features (email/chat carry "from"; calendar organizer is not in
// hit metadata today, a documented limitation).
func hitSenders(md map[string]string) []string {
	from := strings.TrimSpace(md["from"])
	if from == "" {
		return nil
	}
	return []string{extractEmail(from)}
}

// extractEmail pulls a lower-cased email address out of a From header
// ("Name <a@b>" -> "a@b"); falls back to the trimmed lower-cased value.
func extractEmail(from string) string {
	if i := strings.IndexByte(from, '<'); i >= 0 {
		if j := strings.IndexByte(from[i:], '>'); j > 0 {
			return strings.ToLower(strings.TrimSpace(from[i+1 : i+j]))
		}
	}
	return strings.ToLower(strings.TrimSpace(from))
}

func scoreRange(hits []*queryv1.Hit) (minScore, maxScore float64) {
	if len(hits) == 0 {
		return 0, 0
	}
	minScore, maxScore = hits[0].GetScore(), hits[0].GetScore()
	for _, h := range hits {
		s := h.GetScore()
		if s < minScore {
			minScore = s
		}
		if s > maxScore {
			maxScore = s
		}
	}
	return minScore, maxScore
}

func clampPref(v float64) float64 {
	switch {
	case v > 1:
		return 1
	case v < -1:
		return -1
	default:
		return v
	}
}

func firstNonEmpty(ss []string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
