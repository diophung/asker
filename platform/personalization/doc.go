// Package personalization is the pure-logic core of Asker's per-user relevance
// model (spec v3.2 Part 3). It defines the resolved UserPreferenceProfile, the
// online learning-to-rank model, and the combined relevance score, with NO I/O:
// the query service imports it to APPLY a profile during re-ranking, and the
// control-plane imports it to UPDATE the learned model on a feedback event.
//
// Keeping it here (rather than inside one service) means the gateway's
// validation, the control-plane's online update, and the query service's
// scoring all share one source of truth for the model and its defaults — so a
// preference always means the same thing wherever it is read, which is what
// makes ranking testable and explainable (spec: "the ranker must read a single
// resolved UserPreferenceProfile object").
//
// Everything is keyed by tenant_id upstream; this package never sees a tenant
// and holds no per-user state of its own — it is a library of value types and
// functions. The combined score is the spec's formula (score.go):
//
//	relevance = w_sem·semantic + w_pref·preference + w_behav·behavioral
//	          + w_attn·attention − w_fatigue·repetition
//
// The w_* live on the Profile (Weights) and are partly user-tunable (Settings
// sliders) and partly learned.
package personalization
