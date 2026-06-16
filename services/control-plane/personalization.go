package main

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/asker/asker/platform/personalization"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
)

// Personalization request bounds. Profiles and learned models are small JSON
// blobs; a misbehaving caller must not be able to bloat a row.
const (
	maxProfileJSONBytes = 64 << 10 // 64 KiB
	maxFeedbackStrLen   = 1024
	maxFeedbackList     = 64
)

// GetPersonalization returns the caller's stored profile + learned model. A
// tenant with nothing saved gets empty strings + exists=false (cold start), not
// an error — the caller substitutes personalization.DefaultProfile().
func (s *server) GetPersonalization(ctx context.Context, _ *controlplanev1.GetPersonalizationRequest) (*controlplanev1.GetPersonalizationResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	prefs, weights, err := s.store.GetPersonalization(ctx, tc.TenantID())
	if err != nil {
		return nil, s.rpcErr(ctx, "GetPersonalization", tc, err)
	}
	return &controlplanev1.GetPersonalizationResponse{
		ProfileJson: prefs.ProfileJSON,
		Version:     prefs.Version,
		WeightsJson: weights.WeightsJSON,
		SampleCount: weights.SampleCount,
		Exists:      prefs.Exists,
	}, nil
}

// PutPreferences upserts the caller's profile JSON and bumps its version. The
// gateway has already validated/clamped the profile (platform/personalization);
// the control plane re-checks it is well-formed, bounded JSON before storing.
func (s *server) PutPreferences(ctx context.Context, req *controlplanev1.PutPreferencesRequest) (*controlplanev1.PutPreferencesResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	profileJSON := req.GetProfileJson()
	switch {
	case profileJSON == "":
		return nil, status.Error(codes.InvalidArgument, "profile_json is required")
	case len(profileJSON) > maxProfileJSONBytes:
		return nil, status.Errorf(codes.InvalidArgument, "profile_json exceeds %d bytes", maxProfileJSONBytes)
	case !json.Valid([]byte(profileJSON)):
		return nil, status.Error(codes.InvalidArgument, "profile_json is not valid JSON")
	}
	version, err := s.store.PutPreferences(ctx, tc.TenantID(), profileJSON)
	if err != nil {
		return nil, s.rpcErr(ctx, "PutPreferences", tc, err)
	}
	return &controlplanev1.PutPreferencesResponse{Version: version}, nil
}

// RecordFeedback appends one behavioral event and folds it into the online
// learning-to-rank model (platform/personalization) — unless learning is paused
// in the stored profile, in which case the event is dropped entirely (the user
// asked us not to learn from it; autonomy/control). The learned-model update is
// atomic under a per-tenant row lock so concurrent feedback cannot lose an
// update.
func (s *server) RecordFeedback(ctx context.Context, req *controlplanev1.RecordFeedbackRequest) (*controlplanev1.RecordFeedbackResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	ev := req.GetEvent()
	if ev == nil {
		return nil, status.Error(codes.InvalidArgument, "event is required")
	}
	if err := validateFeedback(ev); err != nil {
		return nil, err
	}

	// Honor the pause-learning control: if paused, drop the event and report the
	// model unchanged.
	prefs, weights, err := s.store.GetPersonalization(ctx, tc.TenantID())
	if err != nil {
		return nil, s.rpcErr(ctx, "RecordFeedback", tc, err)
	}
	if profilePaused(prefs.ProfileJSON) {
		return &controlplanev1.RecordFeedbackResponse{
			WeightsJson:    weights.WeightsJSON,
			SampleCount:    weights.SampleCount,
			LearningPaused: true,
		}, nil
	}

	if err := s.store.AppendFeedback(ctx, tc.TenantID(), FeedbackEvent{
		DocID:       ev.GetDocId(),
		DocType:     ev.GetDocType(),
		ConnectorID: ev.GetConnectorId(),
		Action:      ev.GetAction(),
		DwellMs:     ev.GetDwellMs(),
		Query:       ev.GetQuery(),
	}); err != nil {
		return nil, s.rpcErr(ctx, "RecordFeedback", tc, err)
	}

	var updatedJSON string
	var updatedSamples int64
	err = s.store.UpdateLearnedWeights(ctx, tc.TenantID(), func(curJSON string, _ int64) (string, int64, error) {
		model := decodeModel(curJSON)
		features := personalization.FeatureKeys(ev.GetDocType(), ev.GetConnectorId(), ev.GetSenders(), ev.GetTopics())
		if label, ok := personalization.LabelForAction(ev.GetAction()); ok && len(features) > 0 {
			model = model.Update(features, label, 0)
		}
		b, mErr := json.Marshal(model)
		if mErr != nil {
			return "", 0, fmt.Errorf("marshal learned model: %w", mErr)
		}
		updatedJSON = string(b)
		updatedSamples = model.Samples
		return updatedJSON, updatedSamples, nil
	})
	if err != nil {
		return nil, s.rpcErr(ctx, "RecordFeedback", tc, err)
	}

	return &controlplanev1.RecordFeedbackResponse{
		WeightsJson:    updatedJSON,
		SampleCount:    updatedSamples,
		LearningPaused: false,
	}, nil
}

// ResetLearning clears the caller's learned model and feedback log.
func (s *server) ResetLearning(ctx context.Context, _ *controlplanev1.ResetLearningRequest) (*controlplanev1.ResetLearningResponse, error) {
	tc, err := callerTenant(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.store.ResetLearning(ctx, tc.TenantID())
	if err != nil {
		return nil, s.rpcErr(ctx, "ResetLearning", tc, err)
	}
	return &controlplanev1.ResetLearningResponse{FeedbackDeleted: n}, nil
}

// validateFeedback bounds the event's string fields/lists so a hostile caller
// cannot bloat the feedback log or the learned model's key space.
func validateFeedback(ev *controlplanev1.FeedbackEvent) error {
	for _, f := range []struct {
		name, val string
	}{
		{"doc_id", ev.GetDocId()},
		{"doc_type", ev.GetDocType()},
		{"connector_id", ev.GetConnectorId()},
		{"action", ev.GetAction()},
		{"query", ev.GetQuery()},
	} {
		if len(f.val) > maxFeedbackStrLen {
			return status.Errorf(codes.InvalidArgument, "event.%s exceeds %d bytes", f.name, maxFeedbackStrLen)
		}
	}
	if len(ev.GetSenders()) > maxFeedbackList || len(ev.GetTopics()) > maxFeedbackList {
		return status.Errorf(codes.InvalidArgument, "event senders/topics exceed %d entries", maxFeedbackList)
	}
	return nil
}

// profilePaused reports whether the stored profile JSON has learning paused. A
// missing/unparseable profile is treated as not paused (cold-start default).
func profilePaused(profileJSON string) bool {
	if profileJSON == "" {
		return false
	}
	var p personalization.Profile
	if err := json.Unmarshal([]byte(profileJSON), &p); err != nil {
		return false
	}
	return p.LearningPaused
}

// decodeModel parses a learned-model JSON blob, returning a zero model for an
// empty/unparseable value (cold start) so an update always has a base.
func decodeModel(weightsJSON string) personalization.LearnedModel {
	var m personalization.LearnedModel
	if weightsJSON == "" {
		return m
	}
	if err := json.Unmarshal([]byte(weightsJSON), &m); err != nil {
		return personalization.LearnedModel{}
	}
	return m
}
