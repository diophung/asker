package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/asker/asker/platform/personalization"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/tenancy"
)

// Personalization REST surface (spec v3.2 Part 2 + Part 3). The gateway is the
// orchestration point: it validates the profile, persists it via the
// control-plane (Postgres of record), and write-through-caches the resolved
// profile + learned model into Redis for the query hot path. The tenant is
// always the verified-token tenant (the chokepoint); no body field selects one.
//
//	GET    /v1/preferences          -> resolved profile + learning summary
//	PUT    /v1/preferences          -> validate, persist, write-through, bump version
//	POST   /v1/feedback             -> record a behavioral event, update the model
//	POST   /v1/preferences/reset    -> reset what we've learned (model + feedback)
//	GET    /v1/preferences/export   -> full data-rights export (profile + model)

const (
	maxPreferencesBodyBytes = 64 << 10
	maxFeedbackBodyBytes    = 8 << 10
)

// prefWriteStore write-through-caches the resolved profile + learned model into
// Redis (Set with no expiry) and drops the model on reset (DeleteKey). nil-safe
// at the call sites: a cache write failure never fails the request.
type prefWriteStore interface {
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	DeleteKey(ctx context.Context, key string) error
}

// preferencesResponse is the GET /v1/preferences shape.
type preferencesResponse struct {
	Profile     personalization.Profile `json:"profile"`
	SampleCount int64                   `json:"sample_count"`
}

// handleGetPreferences returns the caller's resolved profile (cold-start
// defaults when nothing is saved) plus how many feedback events have been
// learned from.
func (d *deps) handleGetPreferences(w http.ResponseWriter, r *http.Request) {
	resp, err := d.control.GetPersonalization(r.Context(), &controlplanev1.GetPersonalizationRequest{})
	if err != nil {
		d.upstreamError(w, r, "ControlPlaneService.GetPersonalization", err)
		return
	}
	profile := resolveProfile(resp.GetProfileJson(), resp.GetVersion())
	writeJSON(w, http.StatusOK, preferencesResponse{Profile: profile, SampleCount: resp.GetSampleCount()})
}

// handlePutPreferences validates and persists the caller's profile, then
// write-through-caches it (with the new version) for the query path.
func (d *deps) handlePutPreferences(w http.ResponseWriter, r *http.Request) {
	var p personalization.Profile
	if err := json.NewDecoder(io.LimitReader(r.Body, maxPreferencesBodyBytes)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	p = personalization.Clamp(p) // single validation point (bounds sliders/weights/lists)

	// Seed the user's own addresses from the verified email on first save, so the
	// attention scorer can resolve RSVP-to-self / addressed-to-you without the
	// user having to type their own address.
	if len(p.SelfEmails) == 0 {
		if email := callerEmail(r.Context()); email != "" {
			p.SelfEmails = []string{strings.ToLower(email)}
		}
	}

	body, err := json.Marshal(p)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "encode profile"})
		return
	}
	resp, err := d.control.PutPreferences(r.Context(), &controlplanev1.PutPreferencesRequest{ProfileJson: string(body)})
	if err != nil {
		d.upstreamError(w, r, "ControlPlaneService.PutPreferences", err)
		return
	}
	p.Version = resp.GetVersion()
	d.cacheProfile(r.Context(), p)
	writeJSON(w, http.StatusOK, map[string]any{"version": resp.GetVersion()})
}

// feedbackRequest is the POST /v1/feedback body (one behavioral interaction).
type feedbackRequest struct {
	DocID       string   `json:"doc_id"`
	DocType     string   `json:"doc_type"`
	ConnectorID string   `json:"connector_id"`
	Senders     []string `json:"senders"`
	Topics      []string `json:"topics"`
	Action      string   `json:"action"`
	DwellMs     int64    `json:"dwell_ms"`
	Query       string   `json:"query"`
}

// handleFeedback records a behavioral event and folds it into the learned model
// (the control-plane runs the online update), then write-through-caches the new
// model. Best-effort feedback never blocks the user.
func (d *deps) handleFeedback(w http.ResponseWriter, r *http.Request) {
	var fb feedbackRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxFeedbackBodyBytes)).Decode(&fb); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if strings.TrimSpace(fb.Action) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action is required"})
		return
	}
	resp, err := d.control.RecordFeedback(r.Context(), &controlplanev1.RecordFeedbackRequest{
		Event: &controlplanev1.FeedbackEvent{
			DocId:       fb.DocID,
			DocType:     fb.DocType,
			ConnectorId: fb.ConnectorID,
			Senders:     fb.Senders,
			Topics:      fb.Topics,
			Action:      fb.Action,
			DwellMs:     fb.DwellMs,
			Query:       fb.Query,
		},
	})
	if err != nil {
		d.upstreamError(w, r, "ControlPlaneService.RecordFeedback", err)
		return
	}
	if resp.GetWeightsJson() != "" {
		d.cacheWeights(r.Context(), resp.GetWeightsJson())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sample_count":    resp.GetSampleCount(),
		"learning_paused": resp.GetLearningPaused(),
	})
}

// handleResetLearning clears the caller's learned model + feedback history and
// drops the Redis model key (so the query path immediately falls back to
// cold-start behavioral neutrality).
func (d *deps) handleResetLearning(w http.ResponseWriter, r *http.Request) {
	resp, err := d.control.ResetLearning(r.Context(), &controlplanev1.ResetLearningRequest{})
	if err != nil {
		d.upstreamError(w, r, "ControlPlaneService.ResetLearning", err)
		return
	}
	if d.prefs != nil {
		if tc, tErr := tenancy.FromContext(r.Context()); tErr == nil {
			if dErr := d.prefs.DeleteKey(r.Context(), personalization.RedisWeightsKey(string(tc.TenantID()))); dErr != nil {
				d.logger.Debug("drop learned-model cache failed", "error", dErr)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"feedback_deleted": resp.GetFeedbackDeleted()})
}

// handleExportPersonalization returns the caller's full personalization data
// (profile + learned model) for the data-rights export.
func (d *deps) handleExportPersonalization(w http.ResponseWriter, r *http.Request) {
	resp, err := d.control.GetPersonalization(r.Context(), &controlplanev1.GetPersonalizationRequest{})
	if err != nil {
		d.upstreamError(w, r, "ControlPlaneService.GetPersonalization", err)
		return
	}
	var model personalization.LearnedModel
	if resp.GetWeightsJson() != "" {
		_ = json.Unmarshal([]byte(resp.GetWeightsJson()), &model)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"profile":       resolveProfile(resp.GetProfileJson(), resp.GetVersion()),
		"learned_model": model,
		"sample_count":  resp.GetSampleCount(),
	})
}

// resolveProfile parses a stored profile JSON into a clamped Profile, falling
// back to DefaultProfile when nothing is saved, and pins the version.
func resolveProfile(profileJSON string, version int64) personalization.Profile {
	profile := personalization.DefaultProfile()
	if profileJSON != "" {
		var p personalization.Profile
		if json.Unmarshal([]byte(profileJSON), &p) == nil {
			profile = personalization.Clamp(p)
		}
	}
	profile.Version = version
	return profile
}

// cacheProfile write-through-caches the resolved profile JSON for the query
// path (best effort; a cache miss degrades to cold-start defaults there).
func (d *deps) cacheProfile(ctx context.Context, p personalization.Profile) {
	if d.prefs == nil {
		return
	}
	tc, err := tenancy.FromContext(ctx)
	if err != nil {
		return
	}
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	if err := d.prefs.Set(ctx, personalization.RedisProfileKey(string(tc.TenantID())), string(body), 0); err != nil {
		d.logger.Debug("write-through profile cache failed", "error", err)
	}
}

// cacheWeights write-through-caches the learned-model JSON for the query path.
func (d *deps) cacheWeights(ctx context.Context, weightsJSON string) {
	if d.prefs == nil {
		return
	}
	tc, err := tenancy.FromContext(ctx)
	if err != nil {
		return
	}
	if err := d.prefs.Set(ctx, personalization.RedisWeightsKey(string(tc.TenantID())), weightsJSON, 0); err != nil {
		d.logger.Debug("write-through learned-model cache failed", "error", err)
	}
}

// callerEmail returns the verified email claim, or "" when absent.
func callerEmail(ctx context.Context) string {
	if claims := claimsFromContext(ctx); claims != nil {
		if e, ok := claims["email"].(string); ok {
			return e
		}
	}
	return ""
}
