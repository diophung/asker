package personalization

import "sort"

// Features are the per-candidate signals fed to the combined relevance score.
// The query service extracts these from a retrieved hit (semantic relevance,
// explicit-preference match, learned behavioral affinity, attention/urgency,
// and a repetition penalty) and passes them to Score. Keeping the struct here
// pins the contract the Weights coefficients multiply.
type Features struct {
	// Semantic is the retrieval relevance, min-max normalized across the page to
	// [0,1] so it is comparable across ranking profiles.
	Semantic float64
	// Preference is the explicit-Settings match in [-1,1]: positive for source/
	// person/topic boosts, negative for muted (but non-hidden) items.
	Preference float64
	// Behavioral is the learned affinity in [0,1] (LearnedModel.Predict).
	Behavioral float64
	// Attention is the urgency/salience score in [0,1] (the attention scorer).
	Attention float64
	// Fatigue is the repetition penalty in [0,1]: how recently/often this item
	// was already shown. Subtracted in Score.
	Fatigue float64
}

// Score is the spec's combined relevance (spec §3 "Final relevance score"):
//
//	relevance = w_sem·semantic + w_pref·preference + w_behav·behavioral
//	          + w_attn·attention − w_fatigue·repetition
//
// It is a transparent linear combination: every term's contribution is
// w·feature, which is exactly what the explanation reports (Contributions).
func Score(f Features, w Weights) float64 {
	return w.Semantic*f.Semantic +
		w.Preference*f.Preference +
		w.Behavioral*f.Behavioral +
		w.Attention*f.Attention -
		w.Fatigue*f.Fatigue
}

// Contribution is one named term's signed contribution to the combined score
// (w·feature), used to explain WHY an item ranked where it did.
type Contribution struct {
	Name  string
	Value float64
}

// Contributions returns each term's signed contribution (w·feature; fatigue
// negated), sorted by absolute magnitude descending so the caller can name the
// dominant reasons first. This is the data behind the per-result explanation
// (Peak-End / Trust: explanations materially increase trust and perceived
// relevance) and the Hit.features debug map.
func Contributions(f Features, w Weights) []Contribution {
	out := []Contribution{
		{"semantic", w.Semantic * f.Semantic},
		{"preference", w.Preference * f.Preference},
		{"behavioral", w.Behavioral * f.Behavioral},
		{"attention", w.Attention * f.Attention},
		{"repetition", -w.Fatigue * f.Fatigue},
	}
	sort.SliceStable(out, func(i, j int) bool {
		return abs(out[i].Value) > abs(out[j].Value)
	})
	return out
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
