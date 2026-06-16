package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/asker/asker/platform/personalization"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy"
)

// --- memStore unit tests ----------------------------------------------------
//
// These exercise the Store implementation directly (the way store_pg_test.go
// drives pgStore), so they need only context.Background() + a tenancy.TenantID,
// not an RPC context.

// TestMemStorePreferencesVersioning covers requirement #1: PutPreferences bumps
// the version monotonically (1, then 2 on re-put), GetPersonalization returns
// the stored profile + version + Exists=true, and a cold tenant reads back as
// the zero value (Exists=false, empty strings) with no error.
func TestMemStorePreferencesVersioning(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	tA := tenancy.TenantID(tenantA)

	// Cold tenant: never an error, just the zero value.
	prefs, weights, err := st.GetPersonalization(ctx, tA)
	if err != nil {
		t.Fatalf("GetPersonalization (cold): %v", err)
	}
	if prefs.Exists || prefs.ProfileJSON != "" || prefs.Version != 0 {
		t.Errorf("cold prefs = %+v, want zero/Exists=false", prefs)
	}
	if weights.Exists || weights.WeightsJSON != "" || weights.SampleCount != 0 {
		t.Errorf("cold weights = %+v, want zero/Exists=false", weights)
	}

	v1, err := st.PutPreferences(ctx, tA, `{"version":1}`)
	if err != nil {
		t.Fatalf("PutPreferences #1: %v", err)
	}
	if v1 != 1 {
		t.Errorf("first version = %d, want 1", v1)
	}

	// Re-put bumps the version monotonically and overwrites the JSON.
	v2, err := st.PutPreferences(ctx, tA, `{"version":2}`)
	if err != nil {
		t.Fatalf("PutPreferences #2: %v", err)
	}
	if v2 != 2 {
		t.Errorf("second version = %d, want 2", v2)
	}

	prefs, _, err = st.GetPersonalization(ctx, tA)
	if err != nil {
		t.Fatalf("GetPersonalization (warm): %v", err)
	}
	if !prefs.Exists || prefs.Version != 2 || prefs.ProfileJSON != `{"version":2}` {
		t.Errorf("warm prefs = %+v, want the second put / version 2 / Exists", prefs)
	}
}

// TestMemStoreLearnedWeightsAndReset covers requirement #2: AppendFeedback +
// UpdateLearnedWeights (the store applies the passed fn and persists its
// result), and ResetLearning clears the model + feedback and reports how many
// feedback rows it deleted.
func TestMemStoreLearnedWeightsAndReset(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	tA := tenancy.TenantID(tenantA)

	for i := 0; i < 3; i++ {
		if err := st.AppendFeedback(ctx, tA, FeedbackEvent{DocID: "d", Action: "open"}); err != nil {
			t.Fatalf("AppendFeedback #%d: %v", i, err)
		}
	}

	// The store is domain-agnostic: it hands the current state to the fn and
	// persists whatever the fn returns.
	err := st.UpdateLearnedWeights(ctx, tA, func(curJSON string, curSamples int64) (string, int64, error) {
		if curJSON != "" || curSamples != 0 {
			t.Errorf("update fn saw non-empty start: json=%q samples=%d", curJSON, curSamples)
		}
		return `{"weights":{"src:gmail":1}}`, 7, nil
	})
	if err != nil {
		t.Fatalf("UpdateLearnedWeights: %v", err)
	}

	_, weights, err := st.GetPersonalization(ctx, tA)
	if err != nil {
		t.Fatalf("GetPersonalization: %v", err)
	}
	if !weights.Exists || weights.SampleCount != 7 || weights.WeightsJSON != `{"weights":{"src:gmail":1}}` {
		t.Errorf("weights after update = %+v, want the fn's output / Exists", weights)
	}

	// A second update sees the persisted state (proves the store reads back what
	// it wrote, not the zero value).
	err = st.UpdateLearnedWeights(ctx, tA, func(curJSON string, curSamples int64) (string, int64, error) {
		if curJSON != `{"weights":{"src:gmail":1}}` || curSamples != 7 {
			t.Errorf("second update saw json=%q samples=%d, want persisted state", curJSON, curSamples)
		}
		return curJSON, curSamples + 1, nil
	})
	if err != nil {
		t.Fatalf("UpdateLearnedWeights #2: %v", err)
	}

	// ResetLearning wipes weights + feedback and returns the deleted feedback
	// count (3 appended above).
	deleted, err := st.ResetLearning(ctx, tA)
	if err != nil {
		t.Fatalf("ResetLearning: %v", err)
	}
	if deleted != 3 {
		t.Errorf("ResetLearning deleted = %d, want 3", deleted)
	}
	_, weights, err = st.GetPersonalization(ctx, tA)
	if err != nil {
		t.Fatalf("GetPersonalization after reset: %v", err)
	}
	if weights.Exists || weights.WeightsJSON != "" || weights.SampleCount != 0 {
		t.Errorf("weights after reset = %+v, want cleared", weights)
	}

	// Idempotent: a second reset finds nothing to delete.
	deleted, err = st.ResetLearning(ctx, tA)
	if err != nil {
		t.Fatalf("ResetLearning (idempotent): %v", err)
	}
	if deleted != 0 {
		t.Errorf("second ResetLearning deleted = %d, want 0", deleted)
	}
}

// TestMemStorePersonalizationResidueAndPurge covers requirement #3: a tenant
// with personalization rows is NOT residue-free, and PurgeTenant removes prefs,
// weights, and feedback together (the in-memory twin of the FK cascade) so the
// GDPR verification pass sees an empty tenant.
func TestMemStorePersonalizationResidueAndPurge(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	tA := tenancy.TenantID(tenantA)

	if _, err := st.PutPreferences(ctx, tA, `{"version":1}`); err != nil {
		t.Fatalf("PutPreferences: %v", err)
	}
	if err := st.UpdateLearnedWeights(ctx, tA, func(string, int64) (string, int64, error) {
		return `{"weights":{}}`, 1, nil
	}); err != nil {
		t.Fatalf("UpdateLearnedWeights: %v", err)
	}
	if err := st.AppendFeedback(ctx, tA, FeedbackEvent{DocID: "d", Action: "open"}); err != nil {
		t.Fatalf("AppendFeedback: %v", err)
	}

	// Personalization rows count as residue.
	empty, err := st.TenantResidue(ctx, tA)
	if err != nil {
		t.Fatalf("TenantResidue (before purge): %v", err)
	}
	if empty {
		t.Error("TenantResidue reported empty while prefs/weights/feedback exist")
	}

	if _, err := st.PurgeTenant(ctx, tA); err != nil {
		t.Fatalf("PurgeTenant: %v", err)
	}

	empty, err = st.TenantResidue(ctx, tA)
	if err != nil {
		t.Fatalf("TenantResidue (after purge): %v", err)
	}
	if !empty {
		t.Error("TenantResidue not empty after PurgeTenant")
	}

	// All three are gone individually.
	st.mu.Lock()
	_, prefsLeft := st.prefs[tA]
	_, weightsLeft := st.weights[tA]
	feedbackLeft := len(st.feedback[tA])
	st.mu.Unlock()
	if prefsLeft || weightsLeft || feedbackLeft != 0 {
		t.Errorf("purge left residue: prefs=%v weights=%v feedback=%d", prefsLeft, weightsLeft, feedbackLeft)
	}
}

// --- server RPC tests -------------------------------------------------------

// TestRPCPreferencesRoundTrip covers requirement #4: PutPreferences /
// GetPersonalization round-trip through the gRPC server — the version bumps and
// the profile JSON survives byte-for-byte.
func TestRPCPreferencesRoundTrip(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)

	profile := personalization.Clamp(personalization.DefaultProfile())
	profile.ImportantPeople = []string{"alice@example.com"}
	raw, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	profileJSON := string(raw)

	put, err := s.PutPreferences(ctx, &controlplanev1.PutPreferencesRequest{ProfileJson: profileJSON})
	if err != nil {
		t.Fatalf("PutPreferences: %v", err)
	}
	if put.GetVersion() != 1 {
		t.Errorf("first version = %d, want 1", put.GetVersion())
	}

	got, err := s.GetPersonalization(ctx, &controlplanev1.GetPersonalizationRequest{})
	if err != nil {
		t.Fatalf("GetPersonalization: %v", err)
	}
	if !got.GetExists() {
		t.Error("Exists = false after PutPreferences")
	}
	if got.GetVersion() != 1 {
		t.Errorf("version = %d, want 1", got.GetVersion())
	}
	if got.GetProfileJson() != profileJSON {
		t.Errorf("profile JSON did not survive:\n got %s\nwant %s", got.GetProfileJson(), profileJSON)
	}

	// Re-put bumps to version 2.
	put, err = s.PutPreferences(ctx, &controlplanev1.PutPreferencesRequest{ProfileJson: `{"version":9}`})
	if err != nil {
		t.Fatalf("PutPreferences #2: %v", err)
	}
	if put.GetVersion() != 2 {
		t.Errorf("second version = %d, want 2", put.GetVersion())
	}
}

// TestRPCRecordFeedbackTrains covers requirement #5: a positive ("open") action
// on a FeatureKeys-able event increments sample_count and returns a weights_json
// whose decoded LearnedModel predicts > 0.5 for that doc's features (it rose
// from the empty-model neutral 0.5). A non-training action ("impression") is
// appended for audit but leaves the model untouched.
func TestRPCRecordFeedbackTrains(t *testing.T) {
	s, st := testServer(t)
	ctx := tenantCtx(t, tenantA)

	ev := &controlplanev1.FeedbackEvent{
		DocId:       "doc-1",
		DocType:     "EMAIL",
		ConnectorId: "gmail",
		Senders:     []string{"alice@example.com"},
		Topics:      []string{"budget"},
		Action:      "open",
		Query:       "q3 budget",
	}
	// The feature set the model learns about; we assert Predict rose for exactly
	// these (the same builder the handler uses).
	features := personalization.FeatureKeys(ev.GetDocType(), ev.GetConnectorId(), ev.GetSenders(), ev.GetTopics())

	resp, err := s.RecordFeedback(ctx, &controlplanev1.RecordFeedbackRequest{Event: ev})
	if err != nil {
		t.Fatalf("RecordFeedback (open): %v", err)
	}
	if resp.GetLearningPaused() {
		t.Error("learning_paused = true for an unpaused tenant")
	}
	if resp.GetSampleCount() != 1 {
		t.Errorf("sample_count = %d, want 1", resp.GetSampleCount())
	}
	if resp.GetWeightsJson() == "" {
		t.Fatal("weights_json empty after a training event")
	}

	var model personalization.LearnedModel
	if err := json.Unmarshal([]byte(resp.GetWeightsJson()), &model); err != nil {
		t.Fatalf("decode weights_json: %v", err)
	}
	if p := model.Predict(features); p <= 0.5 {
		t.Errorf("Predict after a positive open = %v, want > 0.5 (rose from 0.5)", p)
	}

	// The event was appended to the feedback log.
	st.mu.Lock()
	feedbackN := len(st.feedback[tenancy.TenantID(tenantA)])
	st.mu.Unlock()
	if feedbackN != 1 {
		t.Errorf("feedback log has %d events, want 1", feedbackN)
	}

	// A non-training action is recorded but does not move the model: same
	// weights JSON and same sample_count as before.
	before := resp.GetWeightsJson()
	resp2, err := s.RecordFeedback(ctx, &controlplanev1.RecordFeedbackRequest{Event: &controlplanev1.FeedbackEvent{
		DocId:       "doc-1",
		DocType:     "EMAIL",
		ConnectorId: "gmail",
		Senders:     []string{"alice@example.com"},
		Action:      "impression", // unknown to LabelForAction -> not a training signal
	}})
	if err != nil {
		t.Fatalf("RecordFeedback (impression): %v", err)
	}
	if resp2.GetSampleCount() != 1 {
		t.Errorf("sample_count after non-training action = %d, want 1 (unchanged)", resp2.GetSampleCount())
	}
	if resp2.GetWeightsJson() != before {
		t.Errorf("non-training action changed the model:\n got %s\nwant %s", resp2.GetWeightsJson(), before)
	}

	// But it WAS appended (append-only audit trail).
	st.mu.Lock()
	feedbackN = len(st.feedback[tenancy.TenantID(tenantA)])
	st.mu.Unlock()
	if feedbackN != 2 {
		t.Errorf("feedback log has %d events, want 2 (impression appended)", feedbackN)
	}
}

// TestRPCRecordFeedbackPaused covers requirement #6: with a stored profile that
// has learning_paused=true, RecordFeedback reports learning_paused=true and does
// NOT change the model (the event is dropped — the user asked us not to learn).
func TestRPCRecordFeedbackPaused(t *testing.T) {
	s, st := testServer(t)
	ctx := tenantCtx(t, tenantA)

	// Train once so there is a model to (not) change, then pause learning.
	trainEv := &controlplanev1.FeedbackEvent{
		DocType: "EMAIL", ConnectorId: "gmail", Senders: []string{"alice@example.com"}, Action: "open",
	}
	first, err := s.RecordFeedback(ctx, &controlplanev1.RecordFeedbackRequest{Event: trainEv})
	if err != nil {
		t.Fatalf("RecordFeedback (warm-up): %v", err)
	}
	modelBefore := first.GetWeightsJson()

	paused := personalization.DefaultProfile()
	paused.LearningPaused = true
	raw, err := json.Marshal(paused)
	if err != nil {
		t.Fatalf("marshal paused profile: %v", err)
	}
	if _, err := s.PutPreferences(ctx, &controlplanev1.PutPreferencesRequest{ProfileJson: string(raw)}); err != nil {
		t.Fatalf("PutPreferences (pause): %v", err)
	}

	resp, err := s.RecordFeedback(ctx, &controlplanev1.RecordFeedbackRequest{Event: trainEv})
	if err != nil {
		t.Fatalf("RecordFeedback (paused): %v", err)
	}
	if !resp.GetLearningPaused() {
		t.Error("learning_paused = false, want true for a paused profile")
	}
	// Model is reported unchanged (same weights + sample count as before the pause).
	if resp.GetWeightsJson() != modelBefore {
		t.Errorf("paused feedback changed the model:\n got %s\nwant %s", resp.GetWeightsJson(), modelBefore)
	}
	if resp.GetSampleCount() != first.GetSampleCount() {
		t.Errorf("paused sample_count = %d, want %d (unchanged)", resp.GetSampleCount(), first.GetSampleCount())
	}

	// The dropped event was NOT appended: still just the warm-up event.
	st.mu.Lock()
	feedbackN := len(st.feedback[tenancy.TenantID(tenantA)])
	st.mu.Unlock()
	if feedbackN != 1 {
		t.Errorf("feedback log has %d events, want 1 (paused event dropped)", feedbackN)
	}
}

// TestRPCResetLearning covers requirement #7: ResetLearning clears the learned
// model, so a subsequent GetPersonalization shows no learned weights and
// sample_count 0.
func TestRPCResetLearning(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)

	if _, err := s.RecordFeedback(ctx, &controlplanev1.RecordFeedbackRequest{Event: &controlplanev1.FeedbackEvent{
		DocType: "EMAIL", ConnectorId: "gmail", Senders: []string{"alice@example.com"}, Action: "open",
	}}); err != nil {
		t.Fatalf("RecordFeedback: %v", err)
	}

	// Sanity: there IS a model now.
	got, err := s.GetPersonalization(ctx, &controlplanev1.GetPersonalizationRequest{})
	if err != nil {
		t.Fatalf("GetPersonalization (before reset): %v", err)
	}
	if got.GetSampleCount() == 0 || got.GetWeightsJson() == "" {
		t.Fatalf("no model to reset: %+v", got)
	}

	reset, err := s.ResetLearning(ctx, &controlplanev1.ResetLearningRequest{})
	if err != nil {
		t.Fatalf("ResetLearning: %v", err)
	}
	if reset.GetFeedbackDeleted() != 1 {
		t.Errorf("feedback_deleted = %d, want 1", reset.GetFeedbackDeleted())
	}

	got, err = s.GetPersonalization(ctx, &controlplanev1.GetPersonalizationRequest{})
	if err != nil {
		t.Fatalf("GetPersonalization (after reset): %v", err)
	}
	if got.GetWeightsJson() != "" || got.GetSampleCount() != 0 {
		t.Errorf("model survived reset: weights=%q samples=%d", got.GetWeightsJson(), got.GetSampleCount())
	}
}

// TestRPCResetLearningKeepsPreferences proves ResetLearning erases the learned
// model but leaves the preference profile intact ("reset what you've learned
// about me" is not "forget my settings").
func TestRPCResetLearningKeepsPreferences(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)

	if _, err := s.PutPreferences(ctx, &controlplanev1.PutPreferencesRequest{ProfileJson: `{"version":1}`}); err != nil {
		t.Fatalf("PutPreferences: %v", err)
	}
	if _, err := s.ResetLearning(ctx, &controlplanev1.ResetLearningRequest{}); err != nil {
		t.Fatalf("ResetLearning: %v", err)
	}

	got, err := s.GetPersonalization(ctx, &controlplanev1.GetPersonalizationRequest{})
	if err != nil {
		t.Fatalf("GetPersonalization: %v", err)
	}
	if !got.GetExists() || got.GetProfileJson() != `{"version":1}` || got.GetVersion() != 1 {
		t.Errorf("ResetLearning disturbed preferences: %+v", got)
	}
}

// TestRPCPutPreferencesValidation covers the PutPreferences arm of requirement
// #8: empty, invalid, and oversized profile_json are all InvalidArgument.
func TestRPCPutPreferencesValidation(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)

	cases := []struct {
		name        string
		profileJSON string
	}{
		{"empty", ""},
		{"invalid JSON", `{nope`},
		// One byte over the cap; a valid JSON string so only the size check trips.
		{"oversized", `"` + strings.Repeat("a", maxProfileJSONBytes) + `"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.PutPreferences(ctx, &controlplanev1.PutPreferencesRequest{ProfileJson: tc.profileJSON})
			wantCode(t, err, codes.InvalidArgument)
		})
	}
}

// TestRPCRecordFeedbackValidation covers the RecordFeedback arm of requirement
// #8: a nil event is InvalidArgument.
func TestRPCRecordFeedbackValidation(t *testing.T) {
	s, _ := testServer(t)
	ctx := tenantCtx(t, tenantA)

	_, err := s.RecordFeedback(ctx, &controlplanev1.RecordFeedbackRequest{})
	wantCode(t, err, codes.InvalidArgument)
}

// TestPersonalizationTenantIsolation covers requirement #9: tenant A's
// preferences and learned weights are never visible to tenant B (and vice
// versa); B reads a clean cold-start world even after A has saved everything.
func TestPersonalizationTenantIsolation(t *testing.T) {
	s, _ := testServer(t)
	ctxA := tenantCtx(t, tenantA)
	ctxB := tenantCtx(t, tenantB)

	// A saves preferences and trains a model.
	if _, err := s.PutPreferences(ctxA, &controlplanev1.PutPreferencesRequest{ProfileJson: `{"version":42}`}); err != nil {
		t.Fatalf("A PutPreferences: %v", err)
	}
	if _, err := s.RecordFeedback(ctxA, &controlplanev1.RecordFeedbackRequest{Event: &controlplanev1.FeedbackEvent{
		DocType: "EMAIL", ConnectorId: "gmail", Senders: []string{"alice@example.com"}, Action: "open",
	}}); err != nil {
		t.Fatalf("A RecordFeedback: %v", err)
	}

	// B sees nothing: cold start, no error.
	gotB, err := s.GetPersonalization(ctxB, &controlplanev1.GetPersonalizationRequest{})
	if err != nil {
		t.Fatalf("B GetPersonalization: %v", err)
	}
	if gotB.GetExists() || gotB.GetProfileJson() != "" || gotB.GetVersion() != 0 {
		t.Errorf("B sees A's preferences: %+v", gotB)
	}
	if gotB.GetWeightsJson() != "" || gotB.GetSampleCount() != 0 {
		t.Errorf("B sees A's learned model: weights=%q samples=%d", gotB.GetWeightsJson(), gotB.GetSampleCount())
	}

	// B writing its own data does not disturb A.
	if _, err := s.PutPreferences(ctxB, &controlplanev1.PutPreferencesRequest{ProfileJson: `{"version":7}`}); err != nil {
		t.Fatalf("B PutPreferences: %v", err)
	}
	gotA, err := s.GetPersonalization(ctxA, &controlplanev1.GetPersonalizationRequest{})
	if err != nil {
		t.Fatalf("A GetPersonalization after B writes: %v", err)
	}
	if gotA.GetProfileJson() != `{"version":42}` || gotA.GetVersion() != 1 {
		t.Errorf("A's preferences changed by B: %+v", gotA)
	}
	if gotA.GetSampleCount() != 1 || gotA.GetWeightsJson() == "" {
		t.Errorf("A's learned model changed by B: weights=%q samples=%d", gotA.GetWeightsJson(), gotA.GetSampleCount())
	}
}
