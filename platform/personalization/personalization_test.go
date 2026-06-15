package personalization

import (
	"encoding/json"
	"math"
	"testing"
)

func TestDefaultProfileIsSaneAndStable(t *testing.T) {
	p := DefaultProfile()
	if p.AttentionSensitivity != 0.5 || p.RecencyVsImportance != 0.5 {
		t.Errorf("default sliders = %v/%v, want 0.5/0.5", p.AttentionSensitivity, p.RecencyVsImportance)
	}
	if p.Weights != DefaultWeights() {
		t.Errorf("default weights = %+v, want %+v", p.Weights, DefaultWeights())
	}
	if p.Location() != nil && p.Location().String() != "UTC" {
		t.Errorf("default location = %v, want UTC", p.Location())
	}
	if p.WorkingHours.StartHour != 9 || p.WorkingHours.EndHour != 17 {
		t.Errorf("default working hours = %+v, want 9-17", p.WorkingHours)
	}
	// Default must round-trip through JSON (it is persisted as profile_json).
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Profile
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Weights != p.Weights {
		t.Errorf("weights did not round-trip: %+v vs %+v", back.Weights, p.Weights)
	}
}

func TestClampBoundsAndNormalizes(t *testing.T) {
	in := Profile{
		SourceWeights:        map[string]float64{"email": 99, "  ": 1, "FILE": -2},
		ImportantPeople:      []string{" Alice@Example.com ", "alice@example.com", ""},
		Topics:               []string{"Budget", "budget", "  "},
		Mute:                 MuteList{Sources: []string{"chat_message", "chat_message"}},
		AttentionSensitivity: 5,
		RecencyVsImportance:  -1,
		NoveltyVsFamiliarity: 0.3,
		Weights:              Weights{Semantic: -1, Preference: 2, Fatigue: -3},
		Timezone:             "Not/AZone",
		WorkingHours:         WorkingHours{StartHour: 30, EndHour: 2},
	}
	got := Clamp(in)

	if got.AttentionSensitivity != 1 || got.RecencyVsImportance != 0 || got.NoveltyVsFamiliarity != 0.3 {
		t.Errorf("sliders not clamped: %+v", got)
	}
	if got.Weights.Semantic != 0 || got.Weights.Preference != 2 || got.Weights.Fatigue != 0 {
		t.Errorf("weights not clamped >= 0: %+v", got.Weights)
	}
	if got.SourceWeights["EMAIL"] != sourceWeightMax {
		t.Errorf("source weight not capped: %v", got.SourceWeights["EMAIL"])
	}
	if got.SourceWeights["FILE"] != 0 {
		t.Errorf("negative source weight not floored: %v", got.SourceWeights["FILE"])
	}
	if _, ok := got.SourceWeights[""]; ok {
		t.Error("blank source key survived")
	}
	if len(got.ImportantPeople) != 1 || got.ImportantPeople[0] != "alice@example.com" {
		t.Errorf("important people not normalized/deduped: %v", got.ImportantPeople)
	}
	if len(got.Topics) != 1 || got.Topics[0] != "Budget" {
		t.Errorf("topics not deduped (case-insensitively, keeping first): %v", got.Topics)
	}
	if len(got.Mute.Sources) != 1 || got.Mute.Sources[0] != "CHAT_MESSAGE" {
		t.Errorf("muted sources not upper/deduped: %v", got.Mute.Sources)
	}
	if got.Timezone != "" {
		t.Errorf("invalid timezone not reset: %q", got.Timezone)
	}
	if got.WorkingHours.StartHour != 9 || got.WorkingHours.EndHour != 17 {
		t.Errorf("degenerate working hours not reset: %+v", got.WorkingHours)
	}
}

func TestClampAllZeroWeightsFallsBackToDefaults(t *testing.T) {
	got := Clamp(Profile{Weights: Weights{}})
	if got.Weights != DefaultWeights() {
		t.Errorf("all-zero weights = %+v, want defaults", got.Weights)
	}
}

func TestProfileLookups(t *testing.T) {
	p := Clamp(Profile{
		SourceWeights:   map[string]float64{"EMAIL": 2},
		ImportantPeople: []string{"alice@example.com"},
		Topics:          []string{"budget", "roadmap"},
		Mute:            MuteList{People: []string{"spam@x.com"}, Topics: []string{"lunch"}, Sources: []string{"IMAGE"}},
	})

	if p.SourceWeight("email") != 2 || p.SourceWeight("FILE") != 1.0 {
		t.Errorf("source weights: email=%v file=%v", p.SourceWeight("email"), p.SourceWeight("FILE"))
	}
	if !p.IsImportantPerson("ALICE@example.com") {
		t.Error("important person not matched case-insensitively")
	}
	if !p.IsImportantPerson("alice") { // substring either direction
		t.Error("important person not matched on fragment")
	}
	if p.IsImportantPerson("bob@x.com") {
		t.Error("non-important person matched")
	}
	if !p.IsMutedPerson("spam@x.com") || !p.IsMutedSource("image") {
		t.Error("mute lookups failed")
	}
	if got := p.MatchedTopics("the Q3 BUDGET review"); len(got) != 1 || got[0] != "budget" {
		t.Errorf("matched topics = %v, want [budget]", got)
	}
	if got := p.MutedTopics("team LUNCH thread"); len(got) != 1 || got[0] != "lunch" {
		t.Errorf("muted topics = %v, want [lunch]", got)
	}
}

func TestLocationValidTimezone(t *testing.T) {
	p := Clamp(Profile{Timezone: "America/New_York"})
	if p.Timezone != "America/New_York" {
		t.Fatalf("valid timezone dropped: %q", p.Timezone)
	}
	if p.Location().String() != "America/New_York" {
		t.Errorf("location = %v", p.Location())
	}
}

func TestScoreFormula(t *testing.T) {
	w := Weights{Semantic: 1, Preference: 0.5, Behavioral: 0.4, Attention: 0.8, Fatigue: 0.3}
	f := Features{Semantic: 1, Preference: 1, Behavioral: 1, Attention: 1, Fatigue: 1}
	want := 1*1 + 0.5*1 + 0.4*1 + 0.8*1 - 0.3*1
	if got := Score(f, w); math.Abs(got-want) > 1e-9 {
		t.Errorf("Score = %v, want %v", got, want)
	}
}

func TestContributionsSortedByMagnitude(t *testing.T) {
	w := DefaultWeights()
	f := Features{Semantic: 0.1, Preference: 0, Behavioral: 0, Attention: 0.9, Fatigue: 0.5}
	cs := Contributions(f, w)
	if len(cs) != 5 {
		t.Fatalf("want 5 contributions, got %d", len(cs))
	}
	// attention (0.8*0.9=0.72) should dominate; verify descending |value|.
	for i := 1; i < len(cs); i++ {
		if math.Abs(cs[i-1].Value) < math.Abs(cs[i].Value) {
			t.Errorf("contributions not sorted by |value|: %+v", cs)
		}
	}
	if cs[0].Name != "attention" {
		t.Errorf("top contributor = %q, want attention", cs[0].Name)
	}
}

func TestFeatureKeysBuild(t *testing.T) {
	got := FeatureKeys("email", "Gmail", []string{"Alice@X", "", "bob@y"}, []string{"Budget"})
	want := []string{"type:EMAIL", "src:gmail", "from:alice@x", "from:bob@y", "topic:budget"}
	if len(got) != len(want) {
		t.Fatalf("FeatureKeys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("FeatureKeys[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestFeatureKeysSenderNormalization guards the train/score key mismatch: a
// feedback event carries the raw "Name <email>" header while the query path
// extracts the bare email; both MUST produce the same from: feature so the
// learned per-sender signal actually applies at ranking time.
func TestFeatureKeysSenderNormalization(t *testing.T) {
	header := FeatureKeys("EMAIL", "gmail", []string{"Sharebird Events <mail@events.sharebird.com>"}, nil)
	bare := FeatureKeys("EMAIL", "gmail", []string{"mail@events.sharebird.com"}, nil)
	want := "from:mail@events.sharebird.com"
	if len(header) < 3 || header[2] != want {
		t.Errorf("header-form sender = %v, want %q", header, want)
	}
	if len(bare) < 3 || bare[2] != want {
		t.Errorf("bare-email sender = %v, want %q", bare, want)
	}
	if header[2] != bare[2] {
		t.Errorf("header and bare senders produced different keys: %q vs %q", header[2], bare[2])
	}
}

func TestEmptyModelPredictsNeutral(t *testing.T) {
	var m LearnedModel
	if got := m.Predict([]string{"type:EMAIL"}); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("empty model Predict = %v, want 0.5", got)
	}
}

func TestUpdateLearnsAffinityAndConverges(t *testing.T) {
	var m LearnedModel
	pos := FeatureKeys("email", "gmail", []string{"alice@x"}, nil)
	neg := FeatureKeys("chat_message", "slack", []string{"spam@y"}, nil)

	before := m.Predict(pos)
	for i := 0; i < 25; i++ {
		m = m.Update(pos, 1.0, 0)
		m = m.Update(neg, 0.0, 0)
	}
	afterPos := m.Predict(pos)
	afterNeg := m.Predict(neg)

	if !(afterPos > before) {
		t.Errorf("positive affinity did not rise: before=%v after=%v", before, afterPos)
	}
	if !(afterPos > 0.7) {
		t.Errorf("positive affinity = %v, want > 0.7 after repeated positive feedback", afterPos)
	}
	if !(afterNeg < 0.3) {
		t.Errorf("negative affinity = %v, want < 0.3 after repeated negative feedback", afterNeg)
	}
	if m.Samples != 50 {
		t.Errorf("samples = %d, want 50", m.Samples)
	}
	// "alice@x" must be a top positive feature; "spam@y" must not.
	top := m.TopPositiveFeatures(3)
	if len(top) == 0 || !contains(top, "from:alice@x") {
		t.Errorf("top positive features = %v, want to include from:alice@x", top)
	}
	if contains(top, "from:spam@y") {
		t.Errorf("negative feature leaked into top positives: %v", top)
	}
}

func TestUpdateCoefficientsClamped(t *testing.T) {
	var m LearnedModel
	f := []string{"from:whale@x"}
	for i := 0; i < 1000; i++ {
		m = m.Update(f, 1.0, 1.0)
	}
	if m.Weights["from:whale@x"] > coefClamp+1e-9 {
		t.Errorf("coefficient not clamped: %v", m.Weights["from:whale@x"])
	}
}

func TestUpdateRoundTripsThroughJSON(t *testing.T) {
	var m LearnedModel
	m = m.Update(FeatureKeys("email", "gmail", []string{"a@x"}, nil), 1.0, 0)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back LearnedModel
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Samples != m.Samples || math.Abs(back.Predict([]string{"from:a@x"})-m.Predict([]string{"from:a@x"})) > 1e-9 {
		t.Errorf("model did not round-trip: %+v vs %+v", back, m)
	}
}

func TestLabelForAction(t *testing.T) {
	cases := map[string]struct {
		label float64
		ok    bool
	}{
		"click": {1, true}, "open": {1, true}, "reply": {1, true}, "show_more": {1, true},
		"dismiss": {0, true}, "show-fewer": {0, true}, "dislike": {0, true},
		"impression": {0, false}, "": {0, false},
	}
	for action, want := range cases {
		got, ok := LabelForAction(action)
		if ok != want.ok || (ok && got != want.label) {
			t.Errorf("LabelForAction(%q) = %v,%v want %v,%v", action, got, ok, want.label, want.ok)
		}
	}
}

func TestHumanizeFeature(t *testing.T) {
	cases := map[string]string{
		"from:alice@x": "from alice@x",
		"topic:budget": "about budget",
		"src:gmail":    "gmail",
		"type:EMAIL":   "emails",
		"weird":        "weird",
	}
	for in, want := range cases {
		if got := HumanizeFeature(in); got != want {
			t.Errorf("HumanizeFeature(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedisKeys(t *testing.T) {
	if got := RedisProfileKey("tenant-a"); got != "asker:pref:tenant-a" {
		t.Errorf("RedisProfileKey = %q", got)
	}
	if got := RedisWeightsKey("tenant-a"); got != "asker:weights:tenant-a" {
		t.Errorf("RedisWeightsKey = %q", got)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
