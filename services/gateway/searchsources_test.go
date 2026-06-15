package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	documentv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

var errInjected = errors.New("injected store failure")

func toStringSlice(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}

// --- Per-source search endpoints -------------------------------------------

func TestSourceSearchAppliesDocTypeFilter(t *testing.T) {
	cases := map[string][]documentv1.DocType{
		"email":    {documentv1.DocType_EMAIL},
		"files":    {documentv1.DocType_FILE, documentv1.DocType_WIKI_PAGE, documentv1.DocType_TICKET},
		"messages": {documentv1.DocType_CHAT_MESSAGE},
		"calendar": {documentv1.DocType_CALENDAR_EVENT},
		"photos":   {documentv1.DocType_IMAGE, documentv1.DocType_VIDEO, documentv1.DocType_AUDIO},
	}
	for source, want := range cases {
		t.Run(source, func(t *testing.T) {
			env := newTestEnv(t)
			env.query.resp = &queryv1.SearchResponse{}
			// A types= param must be OVERRIDDEN by the path source (a tab can
			// never be widened past its own source).
			rec := env.do(http.MethodGet, "/v1/search/"+source+"?q=hi&types=EMAIL", nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			_, gotReq := env.query.captured()
			if !reflect.DeepEqual(gotReq.GetDocTypes(), want) {
				t.Errorf("DocTypes = %v, want %v", gotReq.GetDocTypes(), want)
			}
			if gotReq.GetQuery() != "hi" {
				t.Errorf("query = %q, want hi", gotReq.GetQuery())
			}
		})
	}
}

func TestSourceSearchUnknownSource(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodGet, "/v1/search/bogus?q=x", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if gotTenant, _ := env.query.captured(); gotTenant != "" {
		t.Errorf("QueryService reached for unknown source (tenant %q)", gotTenant)
	}
}

func TestSourceSearchValidatesParams(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodGet, "/v1/search/email?q=x&mode=fuzzy", nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestSourceSearchRequiresAuth(t *testing.T) {
	env := newTestEnv(t)
	req, rec := newRawRequest(http.MethodGet, "/v1/search/email?q=x")
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if gotTenant, _ := env.query.captured(); gotTenant != "" {
		t.Errorf("QueryService reached without auth (tenant %q)", gotTenant)
	}
}

// --- Derived People endpoint -----------------------------------------------

func TestPeopleSearchAggregates(t *testing.T) {
	env := newTestEnv(t)
	t1 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	env.query.resp = &queryv1.SearchResponse{
		Hits: []*queryv1.Hit{
			{Type: documentv1.DocType_EMAIL, Modified: timestamppb.New(t1),
				Metadata: map[string]string{"from": `"Sarah Chen" <sarah@acme.com>`}},
			{Type: documentv1.DocType_EMAIL, Modified: timestamppb.New(t2),
				Metadata: map[string]string{"from": "sarah@acme.com"}}, // same person, bare email
			{Type: documentv1.DocType_CHAT_MESSAGE, Modified: timestamppb.New(t2),
				Metadata: map[string]string{"from": "Marcus Lee"}}, // does not match "sarah"
		},
	}
	rec := env.do(http.MethodGet, "/v1/search/people?q=sarah", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	// The query service is asked for the human-bearing types over the scan window.
	_, gotReq := env.query.captured()
	wantTypes := []documentv1.DocType{documentv1.DocType_EMAIL, documentv1.DocType_CHAT_MESSAGE, documentv1.DocType_CALENDAR_EVENT}
	if !reflect.DeepEqual(gotReq.GetDocTypes(), wantTypes) {
		t.Errorf("people scan DocTypes = %v, want %v", gotReq.GetDocTypes(), wantTypes)
	}
	if gotReq.GetLimit() != peopleScanLimit {
		t.Errorf("people scan limit = %d, want %d", gotReq.GetLimit(), peopleScanLimit)
	}

	got := decodeObject(t, rec)
	people, _ := got["people"].([]any)
	if len(people) != 1 {
		t.Fatalf("people = %v, want exactly Sarah (name match on 'sarah')", got["people"])
	}
	p, _ := people[0].(map[string]any)
	if p["name"] != "Sarah Chen" {
		t.Errorf("name = %v, want 'Sarah Chen' (full name preferred over bare email local-part)", p["name"])
	}
	if p["email"] != "sarah@acme.com" {
		t.Errorf("email = %v, want sarah@acme.com", p["email"])
	}
	if p["count"] != float64(2) {
		t.Errorf("count = %v, want 2 (deduped across two emails)", p["count"])
	}
	if p["summary"] != "2 emails" {
		t.Errorf("summary = %v, want '2 emails'", p["summary"])
	}
	if p["last_contacted"] != "2026-06-10T09:00:00Z" {
		t.Errorf("last_contacted = %v, want the most recent of the two", p["last_contacted"])
	}
}

func TestPeopleSearchRelatednessFallback(t *testing.T) {
	env := newTestEnv(t)
	ts := time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC)
	env.query.resp = &queryv1.SearchResponse{
		Hits: []*queryv1.Hit{
			{Type: documentv1.DocType_EMAIL, Modified: timestamppb.New(ts),
				Metadata: map[string]string{"from": `"Sarah Chen" <sarah@acme.com>`}},
		},
	}
	// A topic query matches no person name, so the endpoint falls back to the
	// people who appear in the relevant results instead of returning nothing.
	rec := env.do(http.MethodGet, "/v1/search/people?q=q3+planning", nil, nil)
	got := decodeObject(t, rec)
	if people, _ := got["people"].([]any); len(people) != 1 {
		t.Fatalf("relatedness fallback should surface Sarah, got %v", got["people"])
	}
}

func TestPeopleSearchEmpty(t *testing.T) {
	env := newTestEnv(t)
	env.query.resp = &queryv1.SearchResponse{}
	rec := env.do(http.MethodGet, "/v1/search/people?q=nobody", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"people":[]`) {
		t.Errorf("body = %s, want \"people\":[] (never null)", rec.Body.String())
	}
}

func TestAggregatePeopleRankingAndParsing(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	mk := func(dt documentv1.DocType, md map[string]string, mod time.Time) *queryv1.Hit {
		return &queryv1.Hit{Type: dt, Modified: timestamppb.New(mod), Metadata: md}
	}
	hits := []*queryv1.Hit{
		mk(documentv1.DocType_CALENDAR_EVENT, map[string]string{
			"response_status:sarah@acme.com": "accepted",
			"organizer":                      "Tom Alvarez <tom@acme.com>",
		}, now.Add(-2*24*time.Hour)),
		mk(documentv1.DocType_EMAIL, map[string]string{
			"from": `"Sarah Chen" <sarah@acme.com>`,
			"to":   "bob@x.com, carol@x.com",
		}, now.Add(-1*24*time.Hour)),
	}
	people := aggregatePeople(hits, "", now) // empty query => everyone, ranked
	if len(people) == 0 || people[0].Name != "Sarah Chen" {
		t.Fatalf("ranking[0] = %+v, want Sarah Chen first (highest frequency)", people)
	}
	emails := map[string]bool{}
	for _, p := range people {
		emails[p.Email] = true
	}
	for _, e := range []string{"sarah@acme.com", "tom@acme.com", "bob@x.com", "carol@x.com"} {
		if !emails[e] {
			t.Errorf("missing %s in aggregated people %+v", e, people)
		}
	}
}

func TestParsePeople(t *testing.T) {
	cases := map[string][]personRef{
		`"Sarah Chen" <sarah@acme.com>`: {{name: "Sarah Chen", email: "sarah@acme.com"}},
		"bob@x.com, carol@y.com":        {{name: "bob", email: "bob@x.com"}, {name: "carol", email: "carol@y.com"}},
		"Marcus Lee":                    {{name: "Marcus Lee", email: ""}},
		"   ":                           nil,
		// An unquoted-comma display name makes ParseAddressList reject the whole
		// list; the salvage path must still recover the valid addresses (and not
		// fabricate one junk contact from the raw header).
		"Lastname, Firstname <first@x.com>, Other <other@y.com>": {
			{name: "Firstname", email: "first@x.com"},
			{name: "Other", email: "other@y.com"},
		},
	}
	for in, want := range cases {
		if got := parsePeople(in); !reflect.DeepEqual(got, want) {
			t.Errorf("parsePeople(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestAggregatePeopleDeterministicCutoff(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	ts := now.Add(-24 * time.Hour)
	// Distinct people that collide on display name ("john") AND score (same
	// count + same last-contact) — the order/cutoff must be stable across runs.
	hits := make([]*queryv1.Hit, 0, 4)
	for _, dom := range []string{"team1.com", "team2.com", "team3.com", "team4.com"} {
		hits = append(hits, &queryv1.Hit{
			Type:     documentv1.DocType_EMAIL,
			Modified: timestamppb.New(ts),
			Metadata: map[string]string{"from": "john@" + dom},
		})
	}
	first := aggregatePeople(hits, "john", now)
	for i := 0; i < 20; i++ {
		if got := aggregatePeople(hits, "john", now); !reflect.DeepEqual(got, first) {
			t.Fatalf("aggregatePeople not deterministic:\n run0 = %+v\n run%d = %+v", first, i+1, got)
		}
	}
	// And the order is the stable email tiebreak (team1..team4).
	wantEmails := []string{"john@team1.com", "john@team2.com", "john@team3.com", "john@team4.com"}
	for i, p := range first {
		if p.Email != wantEmails[i] {
			t.Errorf("order[%d] email = %q, want %q", i, p.Email, wantEmails[i])
		}
	}
}

// --- Recent-search history -------------------------------------------------

func TestRecentSearchRecordedOnSearch(t *testing.T) {
	env := newTestEnv(t)
	env.query.resp = &queryv1.SearchResponse{}
	env.do(http.MethodGet, "/v1/search?q=q3+planning", nil, nil)
	if got := env.recent.snapshot(recentKey(tenancy.TenantID(testSubject))); !reflect.DeepEqual(got, []string{"q3 planning"}) {
		t.Errorf("recent = %v, want [q3 planning] (auto-recorded, normalized)", got)
	}
	// A per-source search auto-records too.
	env.do(http.MethodGet, "/v1/search/files?q=design", nil, nil)
	if got := env.recent.snapshot(recentKey(tenancy.TenantID(testSubject))); !reflect.DeepEqual(got, []string{"design", "q3 planning"}) {
		t.Errorf("recent = %v, want [design, q3 planning]", got)
	}
}

func TestRecentSearchListDedupOrder(t *testing.T) {
	env := newTestEnv(t)
	env.query.resp = &queryv1.SearchResponse{}
	for _, q := range []string{"alpha", "beta", "alpha"} {
		env.do(http.MethodGet, "/v1/search?q="+q, nil, nil)
	}
	rec := env.do(http.MethodGet, "/v1/searches/recent", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := toStringSlice(decodeObject(t, rec)["searches"])
	if want := []string{"alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("searches = %v, want %v (most-recent-first, deduped)", got, want)
	}
}

func TestRecentSearchDeleteOneAndAll(t *testing.T) {
	env := newTestEnv(t)
	env.query.resp = &queryv1.SearchResponse{}
	for _, q := range []string{"alpha", "beta", "gamma"} {
		env.do(http.MethodGet, "/v1/search?q="+q, nil, nil)
	}
	key := recentKey(tenancy.TenantID(testSubject))

	rec := env.do(http.MethodDelete, "/v1/searches?q=beta", nil, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete one: status = %d", rec.Code)
	}
	if got := env.recent.snapshot(key); !reflect.DeepEqual(got, []string{"gamma", "alpha"}) {
		t.Errorf("after remove beta = %v, want [gamma alpha]", got)
	}

	rec = env.do(http.MethodDelete, "/v1/searches", nil, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete all: status = %d", rec.Code)
	}
	if got := env.recent.snapshot(key); len(got) != 0 {
		t.Errorf("after clear = %v, want empty", got)
	}
}

func TestRecentSearchPostNormalizes(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodPost, "/v1/searches", strings.NewReader(`{"q":"  hello   world  "}`), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("post: status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := env.recent.snapshot(recentKey(tenancy.TenantID(testSubject))); !reflect.DeepEqual(got, []string{"hello world"}) {
		t.Errorf("after post = %v, want [hello world] (whitespace normalized)", got)
	}
}

func TestRecentSearchTenantIsolation(t *testing.T) {
	env := newTestEnv(t)
	env.query.resp = &queryv1.SearchResponse{}
	doAs := func(sub, method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", env.bearerFor(sub))
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)
		return rec
	}
	doAs("alice", http.MethodGet, "/v1/search?q=secret")

	// Bob must NOT see Alice's history.
	bob := toStringSlice(decodeObject(t, doAs("bob", http.MethodGet, "/v1/searches/recent"))["searches"])
	if len(bob) != 0 {
		t.Errorf("bob sees alice's recents: %v", bob)
	}
	// Alice sees her own.
	alice := toStringSlice(decodeObject(t, doAs("alice", http.MethodGet, "/v1/searches/recent"))["searches"])
	if !reflect.DeepEqual(alice, []string{"secret"}) {
		t.Errorf("alice recents = %v, want [secret]", alice)
	}
}

func TestRecentSearchRequiresAuth(t *testing.T) {
	env := newTestEnv(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/searches/recent"},
		{http.MethodPost, "/v1/searches"},
		{http.MethodDelete, "/v1/searches"},
	} {
		req, rec := newRawRequest(tc.method, tc.path)
		env.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestRecentSearchStoreOutageDegrades(t *testing.T) {
	env := newTestEnv(t)
	env.recent.err = errInjected
	// A failing store must not fail search or the list endpoint.
	env.query.resp = &queryv1.SearchResponse{}
	if rec := env.do(http.MethodGet, "/v1/search?q=x", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("search with failing recent store: status = %d, want 200", rec.Code)
	}
	rec := env.do(http.MethodGet, "/v1/searches/recent", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list with failing store: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"searches":[]`) {
		t.Errorf("body = %s, want empty searches on store outage", rec.Body.String())
	}
}
