package main

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

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
	candCap := s.candidateCap
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
	textRes, keywordFallback, err := s.searchWithDegradation(ctx, textQ)
	if err != nil {
		return vespaResult{}, err
	}
	if keywordFallback {
		*degradedReasons = addDegraded(*degradedReasons, degradedKeywordOnly)
	}
	if !clipActive {
		return textRes, nil
	}
	clipQ := base
	clipQ.Kind = retrieveCLIP
	clipQ.ClipVector = clipVector
	clipQ.Hits = candCap
	clipQ.Offset = 0
	clipRes, clipErr := s.vespa.Search(ctx, clipQ)
	if clipErr != nil {
		s.logClipError(logger, clipErr)
		*degradedReasons = addDegraded(*degradedReasons, degradedClipUnavailable)
		return textRes, nil
	}
	fused := rrfFuse(textRes.Hits, clipRes.Hits)
	return vespaResult{Hits: fused, Total: mergedTotal(textRes, clipRes, len(fused))}, nil
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
	kwRes, kwErr := s.vespa.Search(ctx, kq)
	if kwErr != nil {
		return vespaResult{}, kwErr
	}
	lists := [][]*queryv1.Hit{kwRes.Hits}
	maxTotal := kwRes.Total

	vq := base
	vq.Kind = retrieveVector
	vq.Vector = vector
	vq.Hits = candCap
	vq.Offset = 0
	if vRes, vErr := s.vespa.Search(ctx, vq); vErr == nil {
		lists = append(lists, vRes.Hits)
		if vRes.Total > maxTotal {
			maxTotal = vRes.Total
		}
	} else {
		logger.Warn("rrf vector arm failed; fusing keyword-only", "error", vErr)
		*degradedReasons = addDegraded(*degradedReasons, degradedKeywordOnly)
	}

	if clipActive {
		cq := base
		cq.Kind = retrieveCLIP
		cq.ClipVector = clipVector
		cq.Hits = candCap
		cq.Offset = 0
		if cRes, cErr := s.vespa.Search(ctx, cq); cErr == nil {
			lists = append(lists, cRes.Hits)
			if cRes.Total > maxTotal {
				maxTotal = cRes.Total
			}
		} else {
			s.logClipError(logger, cErr)
			*degradedReasons = addDegraded(*degradedReasons, degradedClipUnavailable)
		}
	}

	fused := rrfFuse(lists...)
	total := int64(len(fused))
	if maxTotal > total {
		total = maxTotal
	}
	return vespaResult{Hits: fused, Total: total}, nil
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
	hit      *queryv1.Hit
	features personalization.Features
	combined float64
	relNorm  float64 // combined score min-max normalized across the page, for MMR
	docType  string
	sender   string
	reasons  []string
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
	span := maxScore - minScore

	scored := make([]scoredHit, 0, len(candidates))
	for _, h := range candidates {
		sh := scoreCandidate(h, p, minScore, span)
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
			scored = mmrReorder(scored, p.profile.NoveltyVsFamiliarity)
		}
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
		md := h.GetMetadata()
		if p.profile.IsMutedSource(h.GetType().String()) {
			continue
		}
		if from := md["from"]; from != "" && p.profile.IsMutedPerson(from) {
			continue
		}
		if p.intent == intentScheduleLookup && !p.window.isZero() {
			start, ok := flexibleTime(md["start"])
			if !ok || start.Before(p.window.From) || !start.Before(p.window.To) {
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

// scoreCandidate builds the feature vector and combined score for one hit.
func scoreCandidate(h *queryv1.Hit, p personalizeParams, minScore, span float64) scoredHit {
	md := h.GetMetadata()
	docType := h.GetType().String()
	senders := hitSenders(md)
	text := h.GetTitle() + " " + h.GetSnippet()
	topics := p.profile.MatchedTopics(text)
	mutedTopics := p.profile.MutedTopics(text)

	var f personalization.Features

	// Semantic.
	f.Semantic = 1.0
	if span > 0 {
		f.Semantic = (h.GetScore() - minScore) / span
	}

	// Preference (explicit Settings) and the reasons it contributes.
	pref, prefReasons := preferenceMatch(p.profile, docType, senders, topics, mutedTopics)
	f.Preference = pref

	// Behavioral (learned model).
	f.Behavioral = p.model.Predict(personalization.FeatureKeys(docType, h.GetConnectorId(), senders, topics))

	// Attention / salience, gated by intent (full for needs-attention, a smaller
	// constant otherwise so urgency still nudges but does not dominate a content
	// search).
	att := scoreAttention(h, p.profile, p.window, p.now)
	gate := nonAttentionGate
	if p.intent == intentNeedsAttention {
		gate = 1.0
	}
	f.Attention = att.score * gate

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

	reasons := att.reasons
	reasons = append(reasons, prefReasons...)
	if f.Behavioral >= behavioralReasonThreshold {
		reasons = append(reasons, "matches your usual activity")
	}

	return scoredHit{
		hit:      h,
		features: f,
		combined: combined,
		docType:  docType,
		sender:   firstNonEmpty(senders),
		reasons:  reasons,
	}
}

const (
	// nonAttentionGate scales the attention term outside the needs-attention
	// intent: urgency still nudges a content search but never dominates it.
	nonAttentionGate = 0.25
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

// applyExplanation writes the human-readable explanation and (when debug) the
// per-term feature contributions onto the hit.
func applyExplanation(sh *scoredHit, p personalizeParams) {
	sh.hit.Explanation = composeExplanation(sh.reasons)
	if p.debug {
		contribs := personalization.Contributions(sh.features, p.profile.Weights)
		m := make(map[string]float64, len(contribs))
		for _, c := range contribs {
			m[c.Name] = c.Value
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
// run of near-identical results (same type AND sender) is broken up. lambda is
// the novelty rate in (0,1]: 0 keeps pure relevance order, 1 maximizes novelty.
// Similarity is a coarse, embedding-free proxy (shared type or sender) — enough
// to combat filter-bubble monotony without a second vector pass.
func mmrReorder(scored []scoredHit, novelty float64) []scoredHit {
	n := len(scored)
	selected := make([]scoredHit, 0, n)
	used := make([]bool, n)
	selTypes := map[string]bool{}
	selSenders := map[string]bool{}

	for len(selected) < n {
		bestIdx, bestVal := -1, 0.0
		for i := range scored {
			if used[i] {
				continue
			}
			sim := 0.0
			if selTypes[scored[i].docType] {
				sim += 0.5
			}
			if scored[i].sender != "" && selSenders[scored[i].sender] {
				sim += 0.5
			}
			val := (1-novelty)*scored[i].relNorm - novelty*sim
			if bestIdx < 0 || val > bestVal {
				bestIdx, bestVal = i, val
			}
		}
		used[bestIdx] = true
		sh := scored[bestIdx]
		selected = append(selected, sh)
		selTypes[sh.docType] = true
		if sh.sender != "" {
			selSenders[sh.sender] = true
		}
	}
	return selected
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
	span := hi - lo
	for i := range scored {
		if span > 0 {
			scored[i].relNorm = (scored[i].combined - lo) / span
		} else {
			scored[i].relNorm = 1
		}
	}
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
