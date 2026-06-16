package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/asker/asker/platform/personalization"
	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	"github.com/asker/asker/platform/tenancy"
)

// fakeProfileLoader returns per-tenant profiles/models for the personalized
// path; a tenant absent from the maps cold-starts to DefaultProfile + empty
// model (exactly the production loader's miss behavior).
type fakeProfileLoader struct {
	profiles map[string]personalization.Profile
	models   map[string]personalization.LearnedModel
}

func newFakeLoader() *fakeProfileLoader {
	return &fakeProfileLoader{
		profiles: map[string]personalization.Profile{},
		models:   map[string]personalization.LearnedModel{},
	}
}

func (f *fakeProfileLoader) set(tenant string, p personalization.Profile) {
	f.profiles[tenant] = personalization.Clamp(p)
}

func (f *fakeProfileLoader) Load(_ context.Context, tenant tenancy.TenantID) (personalization.Profile, personalization.LearnedModel) {
	p, ok := f.profiles[string(tenant)]
	if !ok {
		p = personalization.DefaultProfile()
	}
	return p, f.models[string(tenant)]
}

// docSpec is one fixture document.
type docSpec struct {
	id        string
	typ       string // DocType enum name
	title     string
	relevance float64
	metadata  map[string]string
	created   time.Time
}

// buildFixture renders docs as a Vespa default-renderer JSON response the
// vespaStub can serve (the same shape parseVespaResponse consumes).
func buildFixture(t *testing.T, docs []docSpec) string {
	t.Helper()
	type fxField struct {
		DocID        string `json:"doc_id"`
		Type         string `json:"type"`
		Title        string `json:"title"`
		Snippet      string `json:"snippet"`
		MetadataJSON string `json:"metadata_json,omitempty"`
		CreatedAt    int64  `json:"created_at,omitempty"`
		EventStart   int64  `json:"event_start,omitempty"`
	}
	type fxChild struct {
		Relevance float64 `json:"relevance"`
		Fields    fxField `json:"fields"`
	}
	children := make([]fxChild, 0, len(docs))
	for _, d := range docs {
		f := fxField{DocID: d.id, Type: d.typ, Title: d.title, Snippet: d.title}
		if len(d.metadata) > 0 {
			b, err := json.Marshal(d.metadata)
			if err != nil {
				t.Fatalf("marshal metadata: %v", err)
			}
			f.MetadataJSON = string(b)
			if start, ok := d.metadata["start"]; ok {
				if ts, perr := time.Parse(time.RFC3339, start); perr == nil {
					f.EventStart = ts.Unix()
				}
			}
		}
		if !d.created.IsZero() {
			f.CreatedAt = d.created.Unix()
		}
		children = append(children, fxChild{Relevance: d.relevance, Fields: f})
	}
	root := map[string]any{
		"root": map[string]any{
			"fields":   map[string]any{"totalCount": len(docs)},
			"children": children,
		},
	}
	b, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(b)
}

func hitIDs(resp *queryv1.SearchResponse) []string {
	ids := make([]string, 0, len(resp.GetHits()))
	for _, h := range resp.GetHits() {
		ids = append(ids, h.GetDocId())
	}
	return ids
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestAcceptanceCalendarNextWeek is canonical query #1: "what's on my calendar
// next week" returns the user's next-week events, time-ordered, and excludes
// events outside that window — even though those events' CREATION time
// (created_at) is identical, proving the filter is on occurrence time.
func TestAcceptanceCalendarNextWeek(t *testing.T) {
	loader := newFakeLoader()
	env := newQueryEnv(t, withProfiles(loader, false))

	now := time.Now().UTC()
	loc := time.UTC
	nextMon := startOfWeek(now, loc).AddDate(0, 0, 7)
	tue := nextMon.AddDate(0, 0, 1).Add(10 * time.Hour)
	wed := nextMon.AddDate(0, 0, 2).Add(15 * time.Hour)
	thisWeek := startOfWeek(now, loc).AddDate(0, 0, 1).Add(9 * time.Hour) // this week, not next

	// All three created at the SAME instant (months ago) — only occurrence time
	// differs, so a created_at filter would (wrongly) keep or drop all three.
	createdLongAgo := now.AddDate(0, -3, 0)
	docs := []docSpec{
		{id: "e-wed", typ: "CALENDAR_EVENT", title: "Design review", relevance: 0.3,
			metadata: map[string]string{"start": wed.Format(time.RFC3339)}, created: createdLongAgo},
		{id: "e-tue", typ: "CALENDAR_EVENT", title: "Sprint planning", relevance: 0.3,
			metadata: map[string]string{"start": tue.Format(time.RFC3339)}, created: createdLongAgo},
		{id: "e-thisweek", typ: "CALENDAR_EVENT", title: "Standup", relevance: 0.9,
			metadata: map[string]string{"start": thisWeek.Format(time.RFC3339)}, created: createdLongAgo},
	}
	// A pure-intent calendar lookup retrieves filter-only (the "keyword" profile).
	env.vespa.setProfileFixture("keyword", buildFixture(t, docs))

	resp, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{
		Query: "what's on my calendar next week",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	got := hitIDs(resp)
	want := []string{"e-tue", "e-wed"} // chronological; this-week excluded
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("calendar next week = %v, want %v (chronological, occurrence-filtered)", got, want)
	}

	// The hard occurrence-time filter is pushed to Vespa as an event_start range.
	yql, _ := env.vespa.bodyForProfile(t, "keyword")["yql"].(string)
	if !strings.Contains(yql, "event_start >=") || !strings.Contains(yql, "event_start <") {
		t.Errorf("YQL missing event_start window filter: %s", yql)
	}
	if !strings.Contains(yql, `type contains "CALENDAR_EVENT"`) {
		t.Errorf("YQL missing calendar type scope: %s", yql)
	}
}

// TestAcceptanceNeedsAttentionThisWeek is canonical query #2: "what needs my
// attention this week" surfaces items requiring action (RSVP-pending, overdue,
// unread-from-important) ranked by salience, and drops items with no attention
// signal. Each result carries a why-it-ranked explanation.
func TestAcceptanceNeedsAttentionThisWeek(t *testing.T) {
	loader := newFakeLoader()
	loader.set("alice", personalization.Profile{
		SelfEmails:      []string{"me@example.com"},
		ImportantPeople: []string{"boss@example.com"},
	})
	env := newQueryEnv(t, withProfiles(loader, false))

	now := time.Now().UTC()
	docs := []docSpec{
		{id: "d-rsvp", typ: "CALENDAR_EVENT", title: "Budget sign-off", relevance: 0.2,
			metadata: map[string]string{
				"response_status:me@example.com": "needsAction",
				"start":                          now.Add(48 * time.Hour).Format(time.RFC3339),
			}, created: now.Add(-24 * time.Hour)},
		{id: "d-overdue", typ: "TICKET", title: "Ship the release", relevance: 0.2,
			metadata: map[string]string{"due": now.Add(-72 * time.Hour).Format(time.RFC3339), "status": "open"},
			created:  now.Add(-96 * time.Hour)},
		{id: "d-unread", typ: "EMAIL", title: "Re: contract", relevance: 0.2,
			metadata: map[string]string{"from": "boss@example.com", "to": "me@example.com", "unread": "true"},
			created:  now.Add(-12 * time.Hour)},
		{id: "d-boring", typ: "EMAIL", title: "Weekly newsletter", relevance: 0.95,
			metadata: map[string]string{"from": "news@vendor.com", "to": "list@example.com"},
			created:  now.AddDate(0, -2, 0)},
	}
	env.vespa.setProfileFixture("keyword", buildFixture(t, docs))

	resp, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{
		Query: "what needs my attention this week",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	got := hitIDs(resp)
	// The boring (high-relevance but zero-salience) newsletter must be dropped.
	if containsID(got, "d-boring") {
		t.Errorf("needs-attention surfaced a zero-salience item: %v", got)
	}
	for _, want := range []string{"d-rsvp", "d-overdue", "d-unread"} {
		if !containsID(got, want) {
			t.Errorf("needs-attention missing salient item %q: %v", want, got)
		}
	}
	// Every surfaced item explains why it ranked (trust / transparency).
	for _, h := range resp.GetHits() {
		if h.GetExplanation() == "" {
			t.Errorf("hit %q has no explanation", h.GetDocId())
		}
	}
}

// TestPersonalizationRanksDifferentlyForTwoUsers proves the SAME query returns
// a differently-ordered result for two users with different Settings (the core
// personalization property).
func TestPersonalizationRanksDifferentlyForTwoUsers(t *testing.T) {
	loader := newFakeLoader()
	loader.set("alice", personalization.Profile{ImportantPeople: []string{"alice@example.com"}})
	loader.set("bob", personalization.Profile{ImportantPeople: []string{"bob@example.com"}})
	env := newQueryEnv(t, withProfiles(loader, false))

	now := time.Now().UTC()
	docs := []docSpec{
		{id: "d-from-bob", typ: "EMAIL", title: "project update", relevance: 0.5,
			metadata: map[string]string{"from": "bob@example.com"}, created: now},
		{id: "d-from-alice", typ: "EMAIL", title: "project update", relevance: 0.5,
			metadata: map[string]string{"from": "alice@example.com"}, created: now},
	}
	env.vespa.setProfileFixture("keyword", buildFixture(t, docs))

	req := func() *queryv1.SearchRequest {
		return &queryv1.SearchRequest{Query: "project update", Mode: queryv1.SearchMode_KEYWORD}
	}
	respA, err := env.client.Search(tenantCtx(t, "alice"), req())
	if err != nil {
		t.Fatalf("Search alice: %v", err)
	}
	respB, err := env.client.Search(tenantCtx(t, "bob"), req())
	if err != nil {
		t.Fatalf("Search bob: %v", err)
	}

	topA := hitIDs(respA)[0]
	topB := hitIDs(respB)[0]
	if topA != "d-from-alice" {
		t.Errorf("alice's top = %q, want d-from-alice (her important person)", topA)
	}
	if topB != "d-from-bob" {
		t.Errorf("bob's top = %q, want d-from-bob (his important person)", topB)
	}
	if topA == topB {
		t.Error("same query ranked identically for two users with different preferences")
	}
}

// TestPersonalizationProfileIsolatedPerTenant proves one user's profile never
// influences another user's ranking: alice (who boosts alice@) sees her boost;
// bob (no profile) sees the unboosted arrival order.
func TestPersonalizationProfileIsolatedPerTenant(t *testing.T) {
	loader := newFakeLoader()
	loader.set("alice", personalization.Profile{ImportantPeople: []string{"alice@example.com"}})
	// bob: deliberately no profile -> cold-start defaults.
	env := newQueryEnv(t, withProfiles(loader, false))

	now := time.Now().UTC()
	docs := []docSpec{
		{id: "d-from-bob", typ: "EMAIL", title: "project update", relevance: 0.5,
			metadata: map[string]string{"from": "bob@example.com"}, created: now},
		{id: "d-from-alice", typ: "EMAIL", title: "project update", relevance: 0.5,
			metadata: map[string]string{"from": "alice@example.com"}, created: now},
	}
	env.vespa.setProfileFixture("keyword", buildFixture(t, docs))

	req := func() *queryv1.SearchRequest {
		return &queryv1.SearchRequest{Query: "project update", Mode: queryv1.SearchMode_KEYWORD}
	}
	respA, err := env.client.Search(tenantCtx(t, "alice"), req())
	if err != nil {
		t.Fatalf("Search alice: %v", err)
	}
	respB, err := env.client.Search(tenantCtx(t, "bob"), req())
	if err != nil {
		t.Fatalf("Search bob: %v", err)
	}

	if hitIDs(respA)[0] != "d-from-alice" {
		t.Errorf("alice's profile did not boost her important person: %v", hitIDs(respA))
	}
	// bob, with no profile, must see the unboosted arrival order (alice's boost
	// must not leak): equal scores keep the fixture order [d-from-bob, ...].
	if hitIDs(respB)[0] != "d-from-bob" {
		t.Errorf("bob's ranking was influenced by alice's profile: %v", hitIDs(respB))
	}
}

// TestRRFRetrievalIssuesBothArms verifies the personalized hybrid path issues a
// separate keyword arm and vector arm (fused by RRF), not a single blend.
func TestRRFRetrievalIssuesBothArms(t *testing.T) {
	loader := newFakeLoader()
	// clip down so the only arms are keyword + vector (cleaner assertion).
	env := newQueryEnv(t, withProfiles(loader, true), withClipDown())

	now := time.Now().UTC()
	env.vespa.setProfileFixture("keyword", buildFixture(t, []docSpec{
		{id: "k1", typ: "EMAIL", title: "alpha", relevance: 0.8, created: now},
		{id: "shared", typ: "EMAIL", title: "alpha beta", relevance: 0.4, created: now},
	}))
	env.vespa.setProfileFixture("hybrid", buildFixture(t, []docSpec{
		{id: "shared", typ: "EMAIL", title: "alpha beta", relevance: 0.9, created: now},
		{id: "v1", typ: "EMAIL", title: "beta", relevance: 0.5, created: now},
	}))

	resp, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{Query: "alpha beta"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Both arms must have been queried as distinct Vespa requests.
	env.vespa.bodyForProfile(t, "keyword")
	env.vespa.bodyForProfile(t, "hybrid")
	// The doc both arms ranked highly should fuse to the top.
	if got := hitIDs(resp); len(got) == 0 || got[0] != "shared" {
		t.Errorf("RRF top = %v, want 'shared' (consensus of both arms)", got)
	}
}

// TestNormalizeRequestPreservesDebug guards the bug where normalizeRequest
// rebuilt the request and dropped the debug flag, so feature contributions were
// never emitted even with ?debug=1.
func TestNormalizeRequestPreservesDebug(t *testing.T) {
	got := normalizeRequest(&queryv1.SearchRequest{Query: "x", Debug: true})
	if !got.GetDebug() {
		t.Error("normalizeRequest dropped the debug flag")
	}
}

// TestPersonalizeDebugFeatures verifies the per-hit feature contributions are
// attached when debug is set on the personalized path.
func TestPersonalizeDebugFeatures(t *testing.T) {
	loader := newFakeLoader()
	loader.set("alice", personalization.Profile{ImportantPeople: []string{"alice@example.com"}})
	env := newQueryEnv(t, withProfiles(loader, false))

	now := time.Now().UTC()
	env.vespa.setProfileFixture("keyword", buildFixture(t, []docSpec{
		{id: "d1", typ: "EMAIL", title: "project update", relevance: 0.7,
			metadata: map[string]string{"from": "alice@example.com"}, created: now},
	}))

	resp, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{
		Query: "project update", Mode: queryv1.SearchMode_KEYWORD, Debug: true,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.GetHits()) == 0 {
		t.Fatal("no hits")
	}
	feats := resp.GetHits()[0].GetFeatures()
	if len(feats) == 0 {
		t.Fatal("debug features not attached")
	}
	for _, k := range []string{"semantic", "preference", "behavioral", "attention", "repetition"} {
		if _, ok := feats[k]; !ok {
			t.Errorf("feature contribution %q missing: %v", k, feats)
		}
	}
}

// TestPersonalizeMuteHidesSource verifies a muted source is hidden from results.
func TestPersonalizeMuteHidesSource(t *testing.T) {
	loader := newFakeLoader()
	loader.set("alice", personalization.Profile{Mute: personalization.MuteList{Sources: []string{"CHAT_MESSAGE"}}})
	env := newQueryEnv(t, withProfiles(loader, false))

	now := time.Now().UTC()
	env.vespa.setProfileFixture("keyword", buildFixture(t, []docSpec{
		{id: "d-email", typ: "EMAIL", title: "status", relevance: 0.5, created: now},
		{id: "d-chat", typ: "CHAT_MESSAGE", title: "status", relevance: 0.9, created: now},
	}))

	resp, err := env.client.Search(tenantCtx(t, "alice"), &queryv1.SearchRequest{
		Query: "status", Mode: queryv1.SearchMode_KEYWORD,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if containsID(hitIDs(resp), "d-chat") {
		t.Errorf("muted CHAT_MESSAGE source surfaced: %v", hitIDs(resp))
	}
	if !containsID(hitIDs(resp), "d-email") {
		t.Errorf("non-muted source missing: %v", hitIDs(resp))
	}
}
