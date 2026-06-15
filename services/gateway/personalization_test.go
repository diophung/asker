package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/asker/asker/platform/personalization"
)

// The personalization REST surface (v3.2). These tests exercise the gateway
// over the in-proc control-plane fake (behind the REAL tenancygrpc interceptors)
// and a fake prefWriteStore, so both the per-tenant chokepoint and the Redis
// write-through are asserted end to end. The default env.do() bearer mints
// testSubject as the tenant, so every key/store assertion uses testSubject
// unless the test sends a different bearer.

// decodeProfile pulls the "profile" object out of a response and re-decodes it
// into a personalization.Profile (the same type the handler marshals).
func decodeProfile(t *testing.T, raw map[string]any) personalization.Profile {
	t.Helper()
	sub, ok := raw["profile"]
	if !ok {
		t.Fatalf("response missing profile: %v", raw)
	}
	b, err := json.Marshal(sub)
	if err != nil {
		t.Fatalf("re-marshal profile: %v", err)
	}
	var p personalization.Profile
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	return p
}

// TestGetPreferencesColdStart: a tenant that has never saved preferences gets
// the cold-start default profile (attention_sensitivity 0.5) and sample_count 0.
func TestGetPreferencesColdStart(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(http.MethodGet, "/v1/preferences", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeObject(t, rec)
	if sc, _ := got["sample_count"].(float64); sc != 0 {
		t.Errorf("sample_count = %v, want 0", got["sample_count"])
	}
	p := decodeProfile(t, got)
	if p.AttentionSensitivity != 0.5 {
		t.Errorf("attention_sensitivity = %v, want 0.5 (default)", p.AttentionSensitivity)
	}
	if p.Version != 0 {
		t.Errorf("version = %d, want 0 (cold)", p.Version)
	}
	// Cold start defaults bleed through from DefaultProfile.
	want := personalization.DefaultProfile()
	if p.RecencyVsImportance != want.RecencyVsImportance || p.NoveltyVsFamiliarity != want.NoveltyVsFamiliarity {
		t.Errorf("profile sliders = %+v, want defaults %+v", p, want)
	}
	if p.Weights != want.Weights {
		t.Errorf("weights = %+v, want defaults %+v", p.Weights, want.Weights)
	}
	// Nothing should have been written through on a read.
	if _, ok := env.prefs.get(personalization.RedisProfileKey(testSubject)); ok {
		t.Error("GET wrote through a profile cache; reads must not cache")
	}
}

// TestPutPreferences: a valid PUT returns {version:1}, a follow-up GET reflects
// the saved values, the resolved profile is write-through cached under
// RedisProfileKey(tenant), self_emails are seeded from the token email when
// omitted, and out-of-range sliders are clamped.
func TestPutPreferences(t *testing.T) {
	env := newTestEnv(t)

	// attention_sensitivity is out of range (5 -> clamp to 1); self_emails omitted
	// so the verified token email seeds it.
	body := `{
		"attention_sensitivity": 5,
		"recency_vs_importance": -2,
		"important_people": ["Bob@Example.com"],
		"topics": ["budget"]
	}`
	rec := env.do(http.MethodPut, "/v1/preferences", strings.NewReader(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if v, _ := decodeObject(t, rec)["version"].(float64); v != 1 {
		t.Fatalf("PUT version = %v, want 1", decodeObject(t, rec)["version"])
	}

	// Follow-up GET reflects the saved (clamped, seeded) values.
	rec = env.do(http.MethodGet, "/v1/preferences", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
	}
	p := decodeProfile(t, decodeObject(t, rec))
	if p.Version != 1 {
		t.Errorf("version after PUT = %d, want 1", p.Version)
	}
	if p.AttentionSensitivity != 1 {
		t.Errorf("attention_sensitivity = %v, want clamped to 1", p.AttentionSensitivity)
	}
	if p.RecencyVsImportance != 0 {
		t.Errorf("recency_vs_importance = %v, want clamped to 0", p.RecencyVsImportance)
	}
	if len(p.ImportantPeople) != 1 || p.ImportantPeople[0] != "bob@example.com" {
		t.Errorf("important_people = %v, want lowercased [bob@example.com]", p.ImportantPeople)
	}
	if len(p.SelfEmails) != 1 || p.SelfEmails[0] != strings.ToLower(testEmail) {
		t.Errorf("self_emails = %v, want seeded [%s] from token", p.SelfEmails, strings.ToLower(testEmail))
	}

	// Write-through: the resolved profile is cached under RedisProfileKey(tenant).
	cached, ok := env.prefs.get(personalization.RedisProfileKey(testSubject))
	if !ok {
		t.Fatalf("profile not write-through cached under %s", personalization.RedisProfileKey(testSubject))
	}
	var cachedProfile personalization.Profile
	if err := json.Unmarshal([]byte(cached), &cachedProfile); err != nil {
		t.Fatalf("cached profile is not JSON: %v", err)
	}
	if cachedProfile.Version != 1 || cachedProfile.AttentionSensitivity != 1 {
		t.Errorf("cached profile = %+v, want version 1 + clamped sensitivity 1", cachedProfile)
	}
	if len(cachedProfile.SelfEmails) != 1 || cachedProfile.SelfEmails[0] != strings.ToLower(testEmail) {
		t.Errorf("cached self_emails = %v, want seeded", cachedProfile.SelfEmails)
	}

	// The control-plane stored the same verbatim profile JSON the gateway sent.
	if stored := env.control.storedProfile(testSubject); stored == "" {
		t.Error("control plane did not persist the profile")
	}
}

// TestPutPreferencesKeepsExplicitSelfEmails: when the caller supplies
// self_emails, the gateway does not overwrite them with the token email.
func TestPutPreferencesKeepsExplicitSelfEmails(t *testing.T) {
	env := newTestEnv(t)
	body := `{"self_emails": ["me@work.example"]}`
	rec := env.do(http.MethodPut, "/v1/preferences", strings.NewReader(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/v1/preferences", nil, nil)
	p := decodeProfile(t, decodeObject(t, rec))
	if len(p.SelfEmails) != 1 || p.SelfEmails[0] != "me@work.example" {
		t.Errorf("self_emails = %v, want explicit [me@work.example] preserved", p.SelfEmails)
	}
}

// TestPutPreferencesBadJSON: a malformed body is a 400 and nothing is persisted.
func TestPutPreferencesBadJSON(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodPut, "/v1/preferences", strings.NewReader(`{"attention_sensitivity":`), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if decodeObject(t, rec)["error"] == "" {
		t.Error("missing error message")
	}
	if stored := env.control.storedProfile(testSubject); stored != "" {
		t.Errorf("bad PUT persisted a profile: %q", stored)
	}
}

// TestFeedback: a valid open action is 200, the control fake recorded it, and
// the updated learned model is write-through cached under RedisWeightsKey(tenant).
// An empty action is a 400 that never reaches the control plane.
func TestFeedback(t *testing.T) {
	env := newTestEnv(t)

	body := `{
		"doc_id": "doc-1",
		"doc_type": "EMAIL",
		"connector_id": "gmail",
		"senders": ["alice@example.com"],
		"topics": ["budget"],
		"action": "open",
		"dwell_ms": 4200,
		"query": "q3 budget"
	}`
	rec := env.do(http.MethodPost, "/v1/feedback", strings.NewReader(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if sc, _ := decodeObject(t, rec)["sample_count"].(float64); sc != 1 {
		t.Errorf("sample_count = %v, want 1", decodeObject(t, rec)["sample_count"])
	}

	// Control fake recorded the event for this tenant with the body fields intact.
	events := env.control.recordedFeedbackFor(testSubject)
	if len(events) != 1 {
		t.Fatalf("recorded feedback = %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.GetDocId() != "doc-1" || ev.GetAction() != "open" || ev.GetConnectorId() != "gmail" {
		t.Errorf("recorded event = %+v, want doc-1/open/gmail", ev)
	}
	if ev.GetDwellMs() != 4200 || ev.GetQuery() != "q3 budget" {
		t.Errorf("recorded event dwell/query = %d/%q, want 4200/q3 budget", ev.GetDwellMs(), ev.GetQuery())
	}
	if len(ev.GetSenders()) != 1 || ev.GetSenders()[0] != "alice@example.com" {
		t.Errorf("recorded senders = %v, want [alice@example.com]", ev.GetSenders())
	}

	// Write-through: the new learned model is cached under RedisWeightsKey(tenant).
	if _, ok := env.prefs.get(personalization.RedisWeightsKey(testSubject)); !ok {
		t.Errorf("learned model not write-through cached under %s", personalization.RedisWeightsKey(testSubject))
	}
}

// TestFeedbackMissingAction: action:"" is a 400 and never reaches the control
// plane (no event recorded, no weights cached).
func TestFeedbackMissingAction(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodPost, "/v1/feedback", strings.NewReader(`{"doc_id":"x","action":""}`), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if decodeObject(t, rec)["error"] == "" {
		t.Error("missing error message")
	}
	if got := env.control.recordedFeedbackFor(testSubject); len(got) != 0 {
		t.Errorf("400 feedback still recorded %d events", len(got))
	}
	if _, ok := env.prefs.get(personalization.RedisWeightsKey(testSubject)); ok {
		t.Error("400 feedback wrote through a weights cache")
	}
}

// TestResetLearning: reset is 200 and the Redis weights key is deleted so the
// query path falls back to behavioral neutrality.
func TestResetLearning(t *testing.T) {
	env := newTestEnv(t)

	// Seed a learned model via a feedback event so reset has something to drop.
	rec := env.do(http.MethodPost, "/v1/feedback", strings.NewReader(`{"doc_id":"d","action":"open"}`), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed feedback status = %d", rec.Code)
	}
	if _, ok := env.prefs.get(personalization.RedisWeightsKey(testSubject)); !ok {
		t.Fatal("precondition: weights cache should exist before reset")
	}

	rec = env.do(http.MethodPost, "/v1/preferences/reset", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if fd, _ := decodeObject(t, rec)["feedback_deleted"].(float64); fd != 1 {
		t.Errorf("feedback_deleted = %v, want 1", decodeObject(t, rec)["feedback_deleted"])
	}

	// The Redis weights key was deleted (and is gone from the cache).
	if !env.prefs.wasDeleted(personalization.RedisWeightsKey(testSubject)) {
		t.Errorf("reset did not delete %s", personalization.RedisWeightsKey(testSubject))
	}
	if _, ok := env.prefs.get(personalization.RedisWeightsKey(testSubject)); ok {
		t.Error("weights cache still present after reset")
	}
}

// TestExportPersonalization: export returns profile + learned_model +
// sample_count for the caller.
func TestExportPersonalization(t *testing.T) {
	env := newTestEnv(t)

	// Save preferences and record feedback so the export has real content.
	if rec := env.do(http.MethodPut, "/v1/preferences", strings.NewReader(`{"topics":["budget"]}`), nil); rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", rec.Code)
	}
	if rec := env.do(http.MethodPost, "/v1/feedback", strings.NewReader(`{"doc_id":"d","action":"open"}`), nil); rec.Code != http.StatusOK {
		t.Fatalf("feedback status = %d", rec.Code)
	}

	rec := env.do(http.MethodGet, "/v1/preferences/export", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeObject(t, rec)
	for _, k := range []string{"profile", "learned_model", "sample_count"} {
		if _, ok := got[k]; !ok {
			t.Errorf("export missing %q: %v", k, got)
		}
	}
	if sc, _ := got["sample_count"].(float64); sc != 1 {
		t.Errorf("export sample_count = %v, want 1", got["sample_count"])
	}
	p := decodeProfile(t, got)
	if len(p.Topics) != 1 || p.Topics[0] != "budget" {
		t.Errorf("export profile topics = %v, want [budget]", p.Topics)
	}
	// learned_model decodes as a LearnedModel.
	mb, _ := json.Marshal(got["learned_model"])
	var model personalization.LearnedModel
	if err := json.Unmarshal(mb, &model); err != nil {
		t.Fatalf("learned_model is not a LearnedModel: %v", err)
	}
}

// TestPreferencesTenantIsolation: tenant A's saved preferences are never visible
// to tenant B (B sees the cold-start default), proving the JWT tenant scopes
// every control-plane personalization RPC.
func TestPreferencesTenantIsolation(t *testing.T) {
	env := newTestEnv(t)

	// Tenant A (default bearer) saves a distinctive profile.
	if rec := env.do(http.MethodPut, "/v1/preferences", strings.NewReader(`{"topics":["secret-a"]}`), nil); rec.Code != http.StatusOK {
		t.Fatalf("A PUT status = %d", rec.Code)
	}

	asTenantB := http.Header{"Authorization": []string{env.bearerFor("tenant-b")}}

	rec := env.do(http.MethodGet, "/v1/preferences", nil, asTenantB)
	if rec.Code != http.StatusOK {
		t.Fatalf("B GET status = %d", rec.Code)
	}
	got := decodeObject(t, rec)
	if sc, _ := got["sample_count"].(float64); sc != 0 {
		t.Errorf("B sample_count = %v, want 0 (B has saved nothing)", got["sample_count"])
	}
	p := decodeProfile(t, got)
	if p.Version != 0 {
		t.Errorf("B version = %d, want 0 (cold); A's prefs leaked to B", p.Version)
	}
	for _, topic := range p.Topics {
		if topic == "secret-a" {
			t.Fatalf("tenant B sees tenant A's topic %q", topic)
		}
	}
}

// TestPreferencesTenantChokepoint mirrors TestSearchTenantChokepointAndShape:
// attacker-controlled tenant headers cannot change which tenant the gateway
// reads/writes — the tenant is the verified JWT subject end to end, through the
// real client+server tenancy interceptors.
func TestPreferencesTenantChokepoint(t *testing.T) {
	env := newTestEnv(t)
	attackerHdr := http.Header{
		"X-Asker-Tenant": []string{"attacker-tenant"},
		"X-Tenant-Id":    []string{"attacker-tenant"},
	}

	// PUT under the default bearer (tenant testSubject) but with attacker headers.
	rec := env.do(http.MethodPut, "/v1/preferences", strings.NewReader(`{"topics":["mine"]}`), attackerHdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// The control plane stored under the JWT subject, NOT the attacker tenant.
	if stored := env.control.storedProfile(testSubject); stored == "" {
		t.Error("profile not persisted under the JWT tenant")
	}
	if stored := env.control.storedProfile("attacker-tenant"); stored != "" {
		t.Errorf("profile persisted under attacker tenant: %q", stored)
	}
	// Write-through used the JWT tenant's key, not the attacker's.
	if _, ok := env.prefs.get(personalization.RedisProfileKey(testSubject)); !ok {
		t.Errorf("profile not cached under JWT tenant key %s", personalization.RedisProfileKey(testSubject))
	}
	if _, ok := env.prefs.get(personalization.RedisProfileKey("attacker-tenant")); ok {
		t.Error("profile cached under attacker tenant key")
	}

	// And a feedback POST with attacker headers records under the JWT tenant only.
	rec = env.do(http.MethodPost, "/v1/feedback", strings.NewReader(`{"doc_id":"d","action":"open"}`), attackerHdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("feedback status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := env.control.recordedFeedbackFor(testSubject); len(got) != 1 {
		t.Errorf("feedback recorded for JWT tenant = %d, want 1", len(got))
	}
	if got := env.control.recordedFeedbackFor("attacker-tenant"); len(got) != 0 {
		t.Errorf("feedback recorded for attacker tenant = %d, want 0", len(got))
	}
}

// TestPreferencesMethodNotAllowed: POST /v1/preferences (only GET/PUT) is a 405
// with an Allow header; the same for the reset/feedback single-method routes.
func TestPreferencesMethodNotAllowed(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(http.MethodPost, "/v1/preferences", strings.NewReader(`{}`), nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/preferences status = %d, want 405 (body: %s)", rec.Code, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) || !strings.Contains(allow, http.MethodPut) {
		t.Errorf("Allow = %q, want to list GET and PUT", allow)
	}
	if decodeObject(t, rec)["error"] == "" {
		t.Error("missing error message")
	}

	// /v1/preferences/reset is POST-only; GET is a 405.
	if rec := env.do(http.MethodGet, "/v1/preferences/reset", nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/preferences/reset status = %d, want 405", rec.Code)
	}
	// /v1/feedback is POST-only; GET is a 405.
	if rec := env.do(http.MethodGet, "/v1/feedback", nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/feedback status = %d, want 405", rec.Code)
	}
	// /v1/preferences/export is GET-only; POST is a 405.
	if rec := env.do(http.MethodPost, "/v1/preferences/export", strings.NewReader(`{}`), nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/preferences/export status = %d, want 405", rec.Code)
	}
}
