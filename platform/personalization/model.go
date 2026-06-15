package personalization

import (
	"math"
	"sort"
	"strings"
)

// LearnedModel is the transparent online learning-to-rank model (spec §3:
// "start with a transparent learning-to-rank approach — e.g. logistic/linear
// ranker … because it's debuggable and the explanations fall out naturally").
// It is a pointwise logistic regressor over engineered BINARY features
// (FeatureKeys): each feature has a coefficient, and the behavioral affinity of
// a candidate is sigmoid(bias + Σ coefficients of its active features). Positive
// coefficients are exactly the human-readable reasons ("you usually open email
// from alice") the explanation surfaces.
//
// The model is per-tenant, stored as JSON, updated online by Update on each
// feedback event, and read by the query service for the w_behav term. An empty
// model predicts 0.5 (no evidence) — the cold-start neutral.
type LearnedModel struct {
	// Weights maps a feature key (FeatureKeys, e.g. "from:alice@x") to its
	// logistic coefficient.
	Weights map[string]float64 `json:"weights"`
	// Bias is the model intercept.
	Bias float64 `json:"bias"`
	// Samples counts feedback events incorporated (observability + cold-start
	// confidence).
	Samples int64 `json:"samples"`
}

// DefaultLearningRate is the online SGD step. Moderate so a single click cannot
// swing ranking wildly (loss aversion to over-fitting one event), yet learning
// is visible within a handful of interactions ("rapid online adaptation").
const DefaultLearningRate = 0.3

// coefClamp bounds any single coefficient so runaway updates cannot saturate the
// sigmoid and drown the other score terms.
const coefClamp = 6.0

// Predict returns the behavioral affinity in (0,1) for a candidate with the
// given active features: sigmoid(bias + Σ weights). An empty model returns 0.5.
func (m LearnedModel) Predict(features []string) float64 {
	z := m.Bias
	for _, f := range features {
		z += m.Weights[f]
	}
	return sigmoid(z)
}

// Update applies one online logistic-regression SGD step toward label (1.0 for a
// positive interaction, 0.0 for a negative one) for the given active features,
// returning the mutated model. lr <= 0 uses DefaultLearningRate. Coefficients
// are clamped to [-coefClamp, coefClamp]. Callers MUST skip this when learning
// is paused (Profile.LearningPaused) — Update itself does not check the profile.
func (m LearnedModel) Update(features []string, label, lr float64) LearnedModel {
	if lr <= 0 {
		lr = DefaultLearningRate
	}
	if m.Weights == nil {
		m.Weights = make(map[string]float64, len(features))
	}
	// Gradient of log-loss for logistic regression: err = label - p; each active
	// (binary, value 1) feature's coefficient moves by lr*err, as does the bias.
	err := label - m.Predict(features)
	m.Bias = clampCoef(m.Bias + lr*err)
	for _, f := range features {
		if f == "" {
			continue
		}
		m.Weights[f] = clampCoef(m.Weights[f] + lr*err)
	}
	m.Samples++
	return m
}

// TopPositiveFeatures returns up to n feature keys with the largest positive
// coefficients (the strongest learned affinities), for the explanation and for
// debugging the ranker. Ties break by feature key for determinism.
func (m LearnedModel) TopPositiveFeatures(n int) []string {
	type kv struct {
		k string
		v float64
	}
	arr := make([]kv, 0, len(m.Weights))
	for k, v := range m.Weights {
		if v > 0 {
			arr = append(arr, kv{k, v})
		}
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].v != arr[j].v {
			return arr[i].v > arr[j].v
		}
		return arr[i].k < arr[j].k
	})
	if n > len(arr) {
		n = len(arr)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, arr[i].k)
	}
	return out
}

// Feature-key namespaces. Binary indicator features; the model learns a
// coefficient per key. Kept short and stable (they are persisted).
const (
	featTypePrefix  = "type:"  // document type, upper-case enum name
	featSrcPrefix   = "src:"   // connector id, lower-case
	featFromPrefix  = "from:"  // participant address/handle, lower-case
	featTopicPrefix = "topic:" // matched profile topic, lower-case
)

// FeatureKeys builds the active binary-feature set for a candidate: its type,
// its connector/source, each sender/participant, and each matched topic. The
// SAME builder is used at update time (control-plane, from a feedback event) and
// at scoring time (query service, from a hit), so a learned coefficient applies
// to exactly the candidates that share the feature.
func FeatureKeys(docType, connectorID string, senders, topics []string) []string {
	out := make([]string, 0, 2+len(senders)+len(topics))
	if dt := strings.ToUpper(strings.TrimSpace(docType)); dt != "" {
		out = append(out, featTypePrefix+dt)
	}
	if src := strings.ToLower(strings.TrimSpace(connectorID)); src != "" {
		out = append(out, featSrcPrefix+src)
	}
	for _, s := range senders {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, featFromPrefix+s)
		}
	}
	for _, t := range topics {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			out = append(out, featTopicPrefix+t)
		}
	}
	return out
}

// HumanizeFeature renders a feature key as a short explanation fragment, e.g.
// "from:alice@x" -> "from alice@x", "type:EMAIL" -> "emails", "src:gmail" ->
// "gmail", "topic:budget" -> "about budget". Used by the explanation builder.
func HumanizeFeature(key string) string {
	switch {
	case strings.HasPrefix(key, featFromPrefix):
		return "from " + strings.TrimPrefix(key, featFromPrefix)
	case strings.HasPrefix(key, featTopicPrefix):
		return "about " + strings.TrimPrefix(key, featTopicPrefix)
	case strings.HasPrefix(key, featSrcPrefix):
		return strings.TrimPrefix(key, featSrcPrefix)
	case strings.HasPrefix(key, featTypePrefix):
		return strings.ToLower(strings.TrimPrefix(key, featTypePrefix)) + "s"
	default:
		return key
	}
}

// LabelForAction maps a behavioral feedback action to a logistic training label
// and whether it is a usable training signal at all. Positive engagement
// (click/open/reply/dwell/show-more) is 1; negative (dismiss/show-fewer) is 0;
// anything else returns ok=false and must not train the model.
//
// Zeigarnik/engagement framing: opens and replies are strong "this mattered to
// me" signals; a dismissal or an explicit "show fewer like this" is the
// strongest negative the user can give and is honored as such.
func LabelForAction(action string) (label float64, ok bool) {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "click", "open", "reply", "dwell", "show_more", "show-more", "like":
		return 1.0, true
	case "dismiss", "hide", "show_fewer", "show-fewer", "dislike":
		return 0.0, true
	default:
		return 0, false
	}
}

func sigmoid(z float64) float64 {
	return 1.0 / (1.0 + math.Exp(-z))
}

func clampCoef(v float64) float64 {
	switch {
	case v > coefClamp:
		return coefClamp
	case v < -coefClamp:
		return -coefClamp
	default:
		return v
	}
}
