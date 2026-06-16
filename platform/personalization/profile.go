package personalization

import (
	"strings"
	"time"
)

// Bounds on stored list sizes, so a hostile or buggy client cannot bloat a
// profile (defense in depth; the gateway also validates request sizes).
const (
	maxListEntries = 200
	maxEntryLen    = 256
)

// Weights are the coefficients of the combined relevance score (score.go,
// spec §3 "Final relevance score"). They are partly user-tunable (the Settings
// sliders map onto them) and partly learned. All are clamped >= 0 by Clamp; the
// fatigue term is SUBTRACTED in Score, so its weight is still stored as a
// non-negative magnitude.
type Weights struct {
	Semantic   float64 `json:"semantic"`
	Preference float64 `json:"preference"`
	Behavioral float64 `json:"behavioral"`
	Attention  float64 `json:"attention"`
	Fatigue    float64 `json:"fatigue"`
}

// DefaultWeights are the population-level priors (choice architecture: good
// defaults that work before the user touches a single setting). Semantic leads
// so meaning still dominates; attention is weighted strongly because the
// needs-attention intent is the headline use case; behavioral starts moderate
// and grows as the learned model accrues evidence.
func DefaultWeights() Weights {
	return Weights{Semantic: 1.0, Preference: 0.6, Behavioral: 0.5, Attention: 0.8, Fatigue: 0.3}
}

// WorkingHours bounds the user's active day (local time, [StartHour, EndHour)),
// used to ground "this week"/"next week" and what counts as urgent (spec §2:
// "Working hours & timezone → temporal grounding"). EndHour is exclusive and may
// be 24.
type WorkingHours struct {
	StartHour int `json:"start_hour"`
	EndHour   int `json:"end_hour"`
}

// MuteList holds people/topics/sources to down-rank or hide (spec §2 "Mute
// list" → negative weights / filters). Sources are DocType enum names.
type MuteList struct {
	People  []string `json:"people"`
	Topics  []string `json:"topics"`
	Sources []string `json:"sources"`
}

// Profile is the single resolved UserPreferenceProfile the ranker reads (spec
// §2 "The ranker must read a single resolved UserPreferenceProfile object").
// Every field maps to a named ranking input — no setting is cosmetic. It is
// versioned so a change invalidates the per-tenant result cache.
type Profile struct {
	Version int64 `json:"version"`

	// SourceWeights multiplies a candidate's contribution by the weight of its
	// source (key = DocType enum name, e.g. "EMAIL"); a source with no entry
	// uses 1.0. Spec §2 "Priority sources → source-weight vector".
	SourceWeights map[string]float64 `json:"source_weights"`

	// ImportantPeople boosts items from these people (emails/handles). Spec §2
	// "Important people → participant-importance boost"; Social Proof / Authority.
	ImportantPeople []string `json:"important_people"`

	// Topics boosts items matching these interests. Spec §2 "Priority
	// topics/projects/keywords → topical-affinity boost".
	Topics []string `json:"topics"`

	// Mute down-ranks/hides people/topics/sources. Spec §2 "Mute list".
	Mute MuteList `json:"mute"`

	// SelfEmails are the user's own addresses, used by the attention scorer to
	// resolve RSVP status (response_status:<self>) and to/cc addressing. Seeded
	// from the verified JWT email on first write.
	SelfEmails []string `json:"self_emails"`

	// Timezone (IANA name; "" => UTC) and WorkingHours ground temporal scope.
	Timezone     string       `json:"timezone"`
	WorkingHours WorkingHours `json:"working_hours"`

	// AttentionSensitivity is the Signal-Detection criterion in [0,1]: 0 = "show
	// me everything" (low threshold, more false alarms), 1 = "only the critical
	// few" (high threshold, more misses). Spec §2 "Attention sensitivity".
	AttentionSensitivity float64 `json:"attention_sensitivity"`

	// RecencyVsImportance in [0,1] trades freshness against significance:
	// 0 = importance, 1 = recency. Drives the time-decay weight. Spec §2.
	RecencyVsImportance float64 `json:"recency_vs_importance"`

	// NoveltyVsFamiliarity in [0,1] is the exploration rate for the MMR
	// diversification step: 0 = pure exploitation (familiar), 1 = maximal
	// novelty. Spec §2 "Novelty vs. familiarity".
	NoveltyVsFamiliarity float64 `json:"novelty_vs_familiarity"`

	// LearningPaused, when true, freezes the learned model: feedback is dropped
	// rather than incorporated (autonomy/control; spec §2 transparency controls).
	LearningPaused bool `json:"learning_paused"`

	// Weights are the combined-score coefficients (partly user-tunable via the
	// sliders above, partly learned).
	Weights Weights `json:"weights"`
}

// DefaultProfile is the cold-start profile for a user with no stored
// preferences and no behavior (spec §3 "Cold start": explicit Settings →
// population priors → online adaptation; never empty/random). Every value is a
// sensible default that already produces good ranking (Choice Architecture).
func DefaultProfile() Profile {
	return Profile{
		Version:              0,
		SourceWeights:        map[string]float64{},
		ImportantPeople:      nil,
		Topics:               nil,
		Mute:                 MuteList{},
		SelfEmails:           nil,
		Timezone:             "",
		WorkingHours:         WorkingHours{StartHour: 9, EndHour: 17},
		AttentionSensitivity: 0.5,
		RecencyVsImportance:  0.5,
		NoveltyVsFamiliarity: 0.15,
		LearningPaused:       false,
		Weights:              DefaultWeights(),
	}
}

// Clamp returns a sanitized copy of p: sliders clamped to [0,1], weights clamped
// to >= 0, source weights clamped to [0, sourceWeightMax], list entries trimmed/
// deduped/length-capped/count-capped, and the timezone validated (an unparseable
// zone falls back to UTC). It is idempotent and is the single validation point
// the gateway runs before persisting and the loaders run after reading, so the
// ranker never sees an out-of-range value.
func Clamp(p Profile) Profile {
	out := p
	out.AttentionSensitivity = clamp01(p.AttentionSensitivity)
	out.RecencyVsImportance = clamp01(p.RecencyVsImportance)
	out.NoveltyVsFamiliarity = clamp01(p.NoveltyVsFamiliarity)

	out.Weights = Weights{
		Semantic:   clampNonNeg(p.Weights.Semantic),
		Preference: clampNonNeg(p.Weights.Preference),
		Behavioral: clampNonNeg(p.Weights.Behavioral),
		Attention:  clampNonNeg(p.Weights.Attention),
		Fatigue:    clampNonNeg(p.Weights.Fatigue),
	}
	// An all-zero weight set (e.g. a malformed payload) would rank everything
	// equally; fall back to the priors so ranking never collapses.
	if out.Weights == (Weights{}) {
		out.Weights = DefaultWeights()
	}

	out.SourceWeights = clampSourceWeights(p.SourceWeights)
	out.ImportantPeople = normalizeList(p.ImportantPeople, true)
	out.Topics = normalizeList(p.Topics, false)
	out.SelfEmails = normalizeList(p.SelfEmails, true)
	out.Mute = MuteList{
		People:  normalizeList(p.Mute.People, true),
		Topics:  normalizeList(p.Mute.Topics, false),
		Sources: normalizeSources(p.Mute.Sources),
	}

	out.WorkingHours = clampWorkingHours(p.WorkingHours)

	if p.Timezone != "" {
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			out.Timezone = "" // invalid zone => UTC
		}
	}
	return out
}

// sourceWeightMax bounds a single source weight so one source cannot dwarf the
// combined score's other terms.
const sourceWeightMax = 5.0

// Location resolves the profile timezone to a *time.Location, defaulting to UTC
// for an empty or (defensively) unparseable zone.
func (p Profile) Location() *time.Location {
	if p.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// SourceWeight returns the multiplier for a DocType enum name (1.0 when unset).
func (p Profile) SourceWeight(docType string) float64 {
	if w, ok := p.SourceWeights[strings.ToUpper(docType)]; ok {
		return w
	}
	return 1.0
}

// IsImportantPerson reports whether any of a candidate's participant addresses
// is on the important-people list (case-insensitive substring either way, so
// "alice" matches "alice@example.com" and vice versa).
func (p Profile) IsImportantPerson(addrs ...string) bool {
	return anyMatch(p.ImportantPeople, addrs)
}

// IsMutedPerson reports whether any participant address is muted.
func (p Profile) IsMutedPerson(addrs ...string) bool {
	return anyMatch(p.Mute.People, addrs)
}

// IsMutedSource reports whether a DocType enum name is muted.
func (p Profile) IsMutedSource(docType string) bool {
	dt := strings.ToUpper(docType)
	for _, s := range p.Mute.Sources {
		if s == dt {
			return true
		}
	}
	return false
}

// MatchedTopics returns the profile topics that appear (case-insensitive) in
// the given text, in profile order. Used for the topical-affinity boost and the
// explanation ("matches your topic: budget").
func (p Profile) MatchedTopics(text string) []string {
	return matchedTerms(p.Topics, text)
}

// MutedTopics returns the muted topics that appear in the given text.
func (p Profile) MutedTopics(text string) []string {
	return matchedTerms(p.Mute.Topics, text)
}

// --- helpers ----------------------------------------------------------------

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

func clampNonNeg(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

func clampWorkingHours(wh WorkingHours) WorkingHours {
	out := wh
	if out.StartHour < 0 || out.StartHour > 23 {
		out.StartHour = 9
	}
	if out.EndHour < 1 || out.EndHour > 24 {
		out.EndHour = 17
	}
	if out.EndHour <= out.StartHour {
		// Degenerate range: fall back to the default workday rather than an
		// empty window that would make nothing ever "within working hours".
		out.StartHour, out.EndHour = 9, 17
	}
	return out
}

func clampSourceWeights(in map[string]float64) map[string]float64 {
	if len(in) == 0 {
		return map[string]float64{}
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		key := strings.ToUpper(strings.TrimSpace(k))
		if key == "" || len(out) >= maxListEntries {
			continue
		}
		if v < 0 {
			v = 0
		}
		if v > sourceWeightMax {
			v = sourceWeightMax
		}
		out[key] = v
	}
	return out
}

// normalizeList trims, drops empties, length-caps each entry, lowercases (for
// address/handle lists when lower=true), deduplicates preserving order, and
// caps the count.
func normalizeList(in []string, lower bool) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if lower {
			v = strings.ToLower(v)
		}
		if r := []rune(v); len(r) > maxEntryLen {
			v = string(r[:maxEntryLen])
		}
		// Dedupe case-insensitively but preserve the first occurrence's display
		// case (topics are user-facing; addresses are already lower-cased above).
		key := strings.ToLower(v)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, v)
		if len(out) >= maxListEntries {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeSources upper-cases source (DocType) names and dedupes them.
func normalizeSources(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		v := strings.ToUpper(strings.TrimSpace(raw))
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// anyMatch reports whether any needle and any candidate share a case-
// insensitive substring (either direction), so partial identifiers match.
func anyMatch(needles, candidates []string) bool {
	for _, n := range needles {
		if n == "" {
			continue
		}
		nl := strings.ToLower(n)
		for _, c := range candidates {
			if c == "" {
				continue
			}
			cl := strings.ToLower(c)
			if strings.Contains(cl, nl) || strings.Contains(nl, cl) {
				return true
			}
		}
	}
	return false
}

// matchedTerms returns the terms that appear case-insensitively in text.
func matchedTerms(terms []string, text string) []string {
	if text == "" || len(terms) == 0 {
		return nil
	}
	lt := strings.ToLower(text)
	var out []string
	for _, t := range terms {
		if t == "" {
			continue
		}
		if strings.Contains(lt, strings.ToLower(t)) {
			out = append(out, t)
		}
	}
	return out
}
