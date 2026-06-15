package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
	"github.com/asker/asker/platform/tenancy/tenancygrpc"
)

const testDim = 4

// --- stubs -----------------------------------------------------------------

// fakeCache is an in-memory resultCache with failure injection.
type fakeCache struct {
	mu   sync.Mutex
	data map[string][]byte
	ttls map[string]time.Duration
	gets int
	sets int
	err  error
}

func newFakeCache() *fakeCache {
	return &fakeCache{data: make(map[string][]byte), ttls: make(map[string]time.Duration)}
}

func (f *fakeCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.err != nil {
		return nil, false, f.err
	}
	v, ok := f.data[key]
	return v, ok, nil
}

func (f *fakeCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	if f.err != nil {
		return f.err
	}
	f.data[key] = append([]byte(nil), value...)
	f.ttls[key] = ttl
	return nil
}

func (f *fakeCache) counts() (gets, sets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, f.sets
}

// newTEIStub serves POST /embed with deterministic vectors of dim values.
func newTEIStub(t *testing.T, dim int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Inputs []string `json:"inputs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		vectors := make([][]float32, len(req.Inputs))
		for i := range vectors {
			v := make([]float32, dim)
			for j := range v {
				v[j] = 0.01 * float32(j+1)
			}
			vectors[i] = v
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(vectors)
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

// newClipStub serves POST /embed/text with deterministic CLIP-space vectors
// of dim values, in the {"embeddings":[[...]]} shape (ADR-013).
func newClipStub(t *testing.T, dim int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/embed/text" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		var req struct {
			Inputs []string `json:"inputs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		embeddings := make([][]float32, len(req.Inputs))
		for i := range embeddings {
			v := make([]float32, dim)
			for j := range v {
				v[j] = 0.02 * float32(j+1)
			}
			embeddings[i] = v
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Embeddings [][]float32 `json:"embeddings"`
		}{Embeddings: embeddings})
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

// vespaStub records every /search/ body and can fail selected ranking
// profiles with HTTP 500. It also serves /state/v1/health for readiness. Per
// ranking profile it can serve a custom response fixture (e.g. the CLIP arm's
// image hits); profiles without an override get vespaFixture.
type vespaStub struct {
	srv *httptest.Server

	mu             sync.Mutex
	bodies         []map[string]any
	failProfiles   map[string]bool
	profileFixture map[string]string
}

func newVespaStub(t *testing.T) *vespaStub {
	t.Helper()
	v := &vespaStub{failProfiles: make(map[string]bool), profileFixture: make(map[string]string)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /search/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		profile, _ := body["ranking.profile"].(string)
		v.mu.Lock()
		v.bodies = append(v.bodies, body)
		fail := v.failProfiles[profile]
		fixture, ok := v.profileFixture[profile]
		v.mu.Unlock()
		if fail {
			http.Error(w, "injected backend failure", http.StatusInternalServerError)
			return
		}
		if !ok {
			fixture = vespaFixture
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixture))
	})
	mux.HandleFunc("GET /state/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":{"code":"up"}}`))
	})
	v.srv = httptest.NewServer(mux)
	t.Cleanup(v.srv.Close)
	return v
}

func (v *vespaStub) failProfile(profile string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.failProfiles[profile] = true
}

func (v *vespaStub) setProfileFixture(profile, fixture string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.profileFixture[profile] = fixture
}

func (v *vespaStub) recorded() []map[string]any {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]map[string]any(nil), v.bodies...)
}

// bodyForProfile returns the most recent recorded request that used the given
// ranking profile, or fails the test. Two-arm searches issue one body per
// profile, so this disambiguates the text arm from the CLIP arm.
func (v *vespaStub) bodyForProfile(t *testing.T, profile string) map[string]any {
	t.Helper()
	bodies := v.recorded()
	for i := len(bodies) - 1; i >= 0; i-- {
		if bodies[i]["ranking.profile"] == profile {
			return bodies[i]
		}
	}
	t.Fatalf("no Vespa request recorded with ranking.profile=%q", profile)
	return nil
}

func (v *vespaStub) lastBody(t *testing.T) map[string]any {
	t.Helper()
	bodies := v.recorded()
	if len(bodies) == 0 {
		t.Fatal("no Vespa request recorded")
	}
	return bodies[len(bodies)-1]
}

// --- harness ----------------------------------------------------------------

type queryEnv struct {
	client    queryv1.QueryServiceClient
	rawConn   *grpc.ClientConn // no client interceptor: sends no tenant metadata
	vespa     *vespaStub
	teiCalls  *atomic.Int32
	clipCalls *atomic.Int32
	cache     *fakeCache
	teiURL    string
}

// option mutates the environment before the server is built.
type envOption func(*envConfig)

type envConfig struct {
	teiDown  bool
	teiDim   int
	clipDown bool
	clipDim  int
	// v3 personalization: when profiles is non-nil the server runs the
	// personalized path (rrf controls dual-arm RRF retrieval).
	profiles profileLoader
	rrf      bool
}

func withTEIDown() envOption       { return func(c *envConfig) { c.teiDown = true } }
func withTEIDim(dim int) envOption { return func(c *envConfig) { c.teiDim = dim } }
func withClipDown() envOption      { return func(c *envConfig) { c.clipDown = true } }
func withClipDim(dim int) envOption {
	return func(c *envConfig) { c.clipDim = dim }
}

// withProfiles turns ON the personalized path with the given loader; rrf selects
// dual-arm RRF retrieval.
func withProfiles(l profileLoader, rrf bool) envOption {
	return func(c *envConfig) { c.profiles = l; c.rrf = rrf }
}

func newQueryEnv(t *testing.T, opts ...envOption) *queryEnv {
	t.Helper()
	ec := envConfig{teiDim: testDim, clipDim: testDim}
	for _, o := range opts {
		o(&ec)
	}

	tei, teiCalls := newTEIStub(t, ec.teiDim)
	teiURL := tei.URL
	if ec.teiDown {
		tei.Close() // refuses connections from here on
	}
	clip, clipCalls := newClipStub(t, ec.clipDim)
	clipURL := clip.URL
	if ec.clipDown {
		clip.Close() // refuses connections from here on
	}
	vespa := newVespaStub(t)
	cache := newFakeCache()

	srv := newServer(
		newTEIEmbedder(teiURL, testDim, 500*time.Millisecond),
		newClipEmbedder(clipURL, testDim, 500*time.Millisecond),
		newVespaClient(vespa.srv.URL),
		cache,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if ec.profiles != nil {
		srv.profiles = ec.profiles
		srv.rrfEnabled = ec.rrf
		srv.candidateCap = maxLimit
		srv.recencyHalfLife = 720 * time.Hour // enable the recency bonus in scoring
	}

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(tenancygrpc.UnaryServerInterceptor()))
	queryv1.RegisterQueryServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	dialer := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})
	conn, err := grpc.NewClient("passthrough:///bufnet", dialer,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(tenancygrpc.UnaryClientInterceptor()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	rawConn, err := grpc.NewClient("passthrough:///bufnet", dialer,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient (raw): %v", err)
	}
	t.Cleanup(func() { _ = rawConn.Close() })

	return &queryEnv{
		client:    queryv1.NewQueryServiceClient(conn),
		rawConn:   rawConn,
		vespa:     vespa,
		teiCalls:  teiCalls,
		clipCalls: clipCalls,
		cache:     cache,
		teiURL:    teiURL,
	}
}

func tenantCtx(t *testing.T, tenant string) context.Context {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": tenant})
	if err != nil {
		t.Fatalf("FromClaims: %v", err)
	}
	ctx, cancel := context.WithTimeout(tenancy.WithContext(context.Background(), tc), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("status code = %v (err %v), want %v", got, err, want)
	}
}

// --- tests -------------------------------------------------------------------

func TestSearchHybridFlow(t *testing.T) {
	env := newQueryEnv(t)

	resp, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{
		Query: "quarterly plans type:email from:alice",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if resp.GetDegraded() != "" {
		t.Errorf("Degraded = %q, want empty", resp.GetDegraded())
	}
	if resp.GetCached() {
		t.Error("Cached = true on first search")
	}
	if resp.GetTotal() != 2 || len(resp.GetHits()) != 2 {
		t.Errorf("total/hits = %d/%d, want 2/2", resp.GetTotal(), len(resp.GetHits()))
	}
	hit := resp.GetHits()[0]
	if hit.GetDocId() != "doc-1" || hit.GetType() != askerv1.DocType_EMAIL {
		t.Errorf("hit = %s/%v, want doc-1/EMAIL", hit.GetDocId(), hit.GetType())
	}
	if !strings.Contains(hit.GetSnippet(), "<hi>") {
		t.Errorf("snippet %q carries no highlight", hit.GetSnippet())
	}

	if got := env.teiCalls.Load(); got != 1 {
		t.Errorf("TEI calls = %d, want 1", got)
	}

	body := env.vespa.bodyForProfile(t, "hybrid")
	wantYQL := `select * from sources * where rank(userQuery(), ({targetHits:100}nearestNeighbor(embedding,q))) and type contains "EMAIL" and participants contains ({substring:true}"alice")`
	if body["yql"] != wantYQL {
		t.Errorf("yql =\n  %v\nwant\n  %s", body["yql"], wantYQL)
	}
	if body["query"] != "quarterly plans" {
		t.Errorf("query param = %v, want residual text", body["query"])
	}
	if body["ranking.profile"] != "hybrid" {
		t.Errorf("ranking.profile = %v, want hybrid", body["ranking.profile"])
	}
	if body["presentation.summary"] != "search" {
		t.Errorf("presentation.summary = %v, want search", body["presentation.summary"])
	}
	if body["timeout"] != "2s" {
		t.Errorf("timeout = %v, want 2s", body["timeout"])
	}
	if got := body["hits"].(float64); got != 20 {
		t.Errorf("hits = %v, want default 20", got)
	}
	if got := body["offset"].(float64); got != 0 {
		t.Errorf("offset = %v, want 0", got)
	}
	vec, ok := body["input.query(q)"].([]any)
	if !ok || len(vec) != testDim {
		t.Errorf("input.query(q) = %v, want %d floats", body["input.query(q)"], testDim)
	}

	// Non-degraded results are cached with the contract TTL.
	_, sets := env.cache.counts()
	if sets != 1 {
		t.Errorf("cache sets = %d, want 1", sets)
	}
	env.cache.mu.Lock()
	defer env.cache.mu.Unlock()
	for _, ttl := range env.cache.ttls {
		if ttl != cacheTTL {
			t.Errorf("cache TTL = %v, want %v", ttl, cacheTTL)
		}
	}
}

// TestStreamingGroupnameIsContextTenant is THE isolation property: the Vespa
// group always comes from verified gRPC metadata, never from request content.
func TestStreamingGroupnameIsContextTenant(t *testing.T) {
	env := newQueryEnv(t)

	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		// Request content tries to look like another tenant; it must not matter.
		_, err := env.client.Search(tenantCtx(t, tenant), &queryv1.SearchRequest{
			Query: "tenant-c secrets from:tenant-c",
		})
		if err != nil {
			t.Fatalf("Search(%s): %v", tenant, err)
		}
		if got := env.vespa.lastBody(t)["streaming.groupname"]; got != tenant {
			t.Errorf("streaming.groupname = %v, want %v (the metadata tenant)", got, tenant)
		}
	}
}

func TestSearchCacheHit(t *testing.T) {
	env := newQueryEnv(t)
	ctx := tenantCtx(t, "tenant-a")
	req := &queryv1.SearchRequest{Query: "quarterly plans"}

	first, err := env.client.Search(ctx, req)
	if err != nil {
		t.Fatalf("Search #1: %v", err)
	}
	if first.GetCached() {
		t.Error("first search reported cached=true")
	}
	vespaCalls := len(env.vespa.recorded())

	second, err := env.client.Search(ctx, req)
	if err != nil {
		t.Fatalf("Search #2: %v", err)
	}
	if !second.GetCached() {
		t.Error("second identical search not served from cache")
	}
	if got := env.teiCalls.Load(); got != 1 {
		t.Errorf("TEI calls = %d, want 1 (cache hit must skip embed)", got)
	}
	if got := len(env.vespa.recorded()); got != vespaCalls {
		t.Errorf("vespa calls = %d, want %d (cache hit must skip retrieval)", got, vespaCalls)
	}
	if len(second.GetHits()) != len(first.GetHits()) || second.GetTotal() != first.GetTotal() {
		t.Error("cached response differs from original")
	}

	// A different tenant with the identical request must miss.
	third, err := env.client.Search(tenantCtx(t, "tenant-b"), req)
	if err != nil {
		t.Fatalf("Search #3: %v", err)
	}
	if third.GetCached() {
		t.Error("tenant-b was served tenant-a's cache entry")
	}
}

func TestSearchCacheFailureIsSilent(t *testing.T) {
	env := newQueryEnv(t)
	env.cache.err = errors.New("redis down")

	resp, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{Query: "plans"})
	if err != nil {
		t.Fatalf("Search with cache down: %v", err)
	}
	if resp.GetCached() {
		t.Error("Cached = true with cache down")
	}
	gets, sets := env.cache.counts()
	if gets == 0 || sets == 0 {
		t.Errorf("cache ops attempted = %d gets/%d sets, want both attempted", gets, sets)
	}
}

func TestTEIDownHybridDegradesToKeyword(t *testing.T) {
	env := newQueryEnv(t, withTEIDown())

	resp, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{Query: "plans"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// CLIP is up, so only the bge-m3 text arm degraded: exactly keyword-only.
	if resp.GetDegraded() != degradedKeywordOnly {
		t.Errorf("Degraded = %q, want %q", resp.GetDegraded(), degradedKeywordOnly)
	}
	body := env.vespa.bodyForProfile(t, "keyword")
	if !strings.HasPrefix(body["yql"].(string), "select * from sources * where userQuery()") {
		t.Errorf("yql = %v, want plain userQuery() clause", body["yql"])
	}
	if _, ok := body["input.query(q)"]; ok {
		t.Error("degraded keyword query still carries a query vector")
	}
	// Degraded responses are not cached: a 60s TTL must not pin keyword-only
	// results past the TEI blip.
	if _, sets := env.cache.counts(); sets != 0 {
		t.Errorf("cache sets = %d, want 0 for degraded response", sets)
	}
}

func TestTEIDownVectorModeErrors(t *testing.T) {
	env := newQueryEnv(t, withTEIDown())

	_, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{
		Query: "plans",
		Mode:  queryv1.SearchMode_VECTOR,
	})
	wantCode(t, err, codes.Unavailable)
	if got := len(env.vespa.recorded()); got != 0 {
		t.Errorf("vespa calls = %d, want 0 (VECTOR without a vector must not search)", got)
	}
}

func TestEmbeddingDimMismatchDegrades(t *testing.T) {
	// TEI serves 3-dim vectors while the service is configured for 4
	// (ADR-005 misconfiguration): hybrid degrades, never feeds a wrong-size
	// vector to Vespa.
	env := newQueryEnv(t, withTEIDim(testDim-1))

	resp, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{Query: "plans"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.GetDegraded() != degradedKeywordOnly {
		t.Errorf("Degraded = %q, want %q", resp.GetDegraded(), degradedKeywordOnly)
	}
	if _, ok := env.vespa.bodyForProfile(t, "keyword")["input.query(q)"]; ok {
		t.Error("wrong-dimension vector reached Vespa")
	}
}

func TestVespaHybridFailureRetriesKeyword(t *testing.T) {
	env := newQueryEnv(t)
	env.vespa.failProfile("hybrid")

	resp, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{Query: "plans"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// CLIP is up, so the text arm degraded only: exactly keyword-only.
	if resp.GetDegraded() != degradedKeywordOnly {
		t.Errorf("Degraded = %q, want %q", resp.GetDegraded(), degradedKeywordOnly)
	}
	// The text arm tries hybrid (fails) then retries keyword; the CLIP arm runs
	// once in parallel — three calls total, with the hybrid attempt preceding
	// the keyword retry.
	bodies := env.vespa.recorded()
	profiles := make([]string, len(bodies))
	for i, b := range bodies {
		profiles[i] = b["ranking.profile"].(string)
	}
	hybridIdx, keywordIdx, clipCount := -1, -1, 0
	for i, p := range profiles {
		switch p {
		case "hybrid":
			hybridIdx = i
		case "keyword":
			keywordIdx = i
		case "clip":
			clipCount++
		}
	}
	if hybridIdx < 0 || keywordIdx < 0 || hybridIdx > keywordIdx {
		t.Errorf("profiles = %v, want a hybrid attempt before a keyword retry", profiles)
	}
	if clipCount != 1 {
		t.Errorf("clip arm calls = %d, want 1", clipCount)
	}
	if len(resp.GetHits()) == 0 {
		t.Error("keyword retry returned no hits")
	}
}

func TestVespaKeywordFailureErrors(t *testing.T) {
	env := newQueryEnv(t)
	env.vespa.failProfile("keyword")

	_, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{
		Query: "plans",
		Mode:  queryv1.SearchMode_KEYWORD,
	})
	wantCode(t, err, codes.Unavailable)
	if got := len(env.vespa.recorded()); got != 1 {
		t.Errorf("vespa calls = %d, want 1 (keyword failure has no further rung)", got)
	}
}

func TestVectorModeVespaFailureDoesNotRetryKeyword(t *testing.T) {
	env := newQueryEnv(t)
	env.vespa.failProfile("hybrid") // vector-only retrieval uses the hybrid profile

	_, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{
		Query: "plans",
		Mode:  queryv1.SearchMode_VECTOR,
	})
	wantCode(t, err, codes.Unavailable)
	if got := len(env.vespa.recorded()); got != 1 {
		t.Errorf("vespa calls = %d, want 1 (VECTOR mode opts out of the keyword rung)", got)
	}
}

func TestFilterOnlySearch(t *testing.T) {
	env := newQueryEnv(t)

	resp, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{
		Query: "type:email from:alice",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.GetDegraded() != "" {
		t.Errorf("Degraded = %q, want empty (filter-only is not a degradation)", resp.GetDegraded())
	}
	if got := env.teiCalls.Load(); got != 0 {
		t.Errorf("TEI calls = %d, want 0 for empty residual", got)
	}
	body := env.vespa.lastBody(t)
	wantYQL := `select * from sources * where true and type contains "EMAIL" and participants contains ({substring:true}"alice")`
	if body["yql"] != wantYQL {
		t.Errorf("yql =\n  %v\nwant\n  %s", body["yql"], wantYQL)
	}
	if body["ranking.profile"] != "keyword" {
		t.Errorf("ranking.profile = %v, want keyword", body["ranking.profile"])
	}
	if _, ok := body["query"]; ok {
		t.Error("filter-only search sent a query= parameter")
	}
}

func TestEmptyQueryWithoutFiltersRejected(t *testing.T) {
	env := newQueryEnv(t)
	for _, q := range []string{"", "   "} {
		_, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{Query: q})
		wantCode(t, err, codes.InvalidArgument)
	}
}

func TestInvalidParticipantFilterRejected(t *testing.T) {
	env := newQueryEnv(t)
	_, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{
		Query:       "plans",
		Participant: "alice\nor true",
	})
	wantCode(t, err, codes.InvalidArgument)
	if got := len(env.vespa.recorded()); got != 0 {
		t.Errorf("vespa calls = %d, want 0 for invalid filter", got)
	}
}

func TestQueryTextNeverInterpolatedIntoYQL(t *testing.T) {
	env := newQueryEnv(t)
	hostile := `evil" or true or acl contains "everyone\`

	_, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{Query: hostile})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Hostile text must not reach the YQL of EITHER arm (the CLIP arm has no
	// userQuery() at all).
	for _, profile := range []string{"hybrid", "clip"} {
		body := env.vespa.bodyForProfile(t, profile)
		yql := body["yql"].(string)
		if strings.Contains(yql, "evil") || strings.Contains(yql, "acl") {
			t.Errorf("[%s] hostile query text leaked into YQL: %s", profile, yql)
		}
	}
	// The free text rides ONLY the text arm's query= parameter; the CLIP arm
	// carries no query= (no userQuery() clause).
	if got := env.vespa.bodyForProfile(t, "hybrid")["query"]; got != hostile {
		t.Errorf("query param = %v, want the raw text %q", got, hostile)
	}
	if _, ok := env.vespa.bodyForProfile(t, "clip")["query"]; ok {
		t.Error("CLIP arm carried a query= parameter (no userQuery() clause)")
	}
}

func TestParticipantQuotesEscapedOverTheWire(t *testing.T) {
	env := newQueryEnv(t)
	_, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{
		Query:       "plans",
		Participant: `ali"ce`,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	yql := env.vespa.lastBody(t)["yql"].(string)
	if !strings.Contains(yql, `participants contains ({substring:true}"ali\"ce")`) {
		t.Errorf("participant quote not escaped in YQL: %s", yql)
	}
}

func TestLimitOffsetClampingOverTheWire(t *testing.T) {
	env := newQueryEnv(t)
	// KEYWORD mode keeps a single arm with Vespa-level pagination, so the
	// clamped limit/offset reach the wire directly (the merged-arm path applies
	// pagination after the merge — see TestClipMergePagination).
	_, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{
		Query:  "plans",
		Mode:   queryv1.SearchMode_KEYWORD,
		Limit:  9999,
		Offset: 5000,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	body := env.vespa.lastBody(t)
	if got := body["hits"].(float64); got != maxLimit {
		t.Errorf("hits = %v, want clamped %d", got, maxLimit)
	}
	if got := body["offset"].(float64); got != maxOffset {
		t.Errorf("offset = %v, want clamped %d", got, maxOffset)
	}
}

func TestMissingTenantFailsClosed(t *testing.T) {
	env := newQueryEnv(t)

	// No metadata at all: rejected server-side by the interceptor.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := queryv1.NewQueryServiceClient(env.rawConn).Search(ctx, &queryv1.SearchRequest{Query: "plans"})
	wantCode(t, err, codes.Unauthenticated)
	if got := len(env.vespa.recorded()); got != 0 {
		t.Errorf("vespa calls = %d, want 0 for tenant-less request", got)
	}

	// Tenant-less context through the interceptor-equipped client: blocked
	// client-side before it reaches the wire.
	_, err = env.client.Search(ctx, &queryv1.SearchRequest{Query: "plans"})
	wantCode(t, err, codes.FailedPrecondition)
}

func TestSearchDirectCallWithoutTenantContext(t *testing.T) {
	// Defense in depth: even if the interceptor were bypassed, Search itself
	// fails closed without a tenancy.Context.
	srv := newServer(nil, nil, nil, newFakeCache(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := srv.Search(context.Background(), &queryv1.SearchRequest{Query: "plans"})
	wantCode(t, err, codes.Unauthenticated)
}

// --- CLIP text->image arm (ADR-013) -----------------------------------------

// TestClipArmIssuesTextEmbedAndNearestNeighbor: a HYBRID text query runs the
// CLIP arm alongside the text arm — a clip /embed/text plus a clip
// nearestNeighbor over clip_embedding carrying input.query(qclip), scoped to
// the same tenant streaming group as the text arm (isolation).
func TestClipArmIssuesTextEmbedAndNearestNeighbor(t *testing.T) {
	env := newQueryEnv(t)
	env.vespa.setProfileFixture("clip", clipFixture)

	resp, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{Query: "sunset over the sea"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.GetDegraded() != "" {
		t.Errorf("Degraded = %q, want empty (both arms healthy)", resp.GetDegraded())
	}
	if got := env.clipCalls.Load(); got != 1 {
		t.Errorf("clip /embed/text calls = %d, want 1", got)
	}

	clip := env.vespa.bodyForProfile(t, "clip")
	if clip["yql"] != `select * from sources * where ({targetHits:100}nearestNeighbor(clip_embedding,qclip))` {
		t.Errorf("clip YQL = %v", clip["yql"])
	}
	if clip["ranking.profile"] != "clip" {
		t.Errorf("clip ranking.profile = %v, want clip", clip["ranking.profile"])
	}
	qclip, ok := clip["input.query(qclip)"].([]any)
	if !ok || len(qclip) != testDim {
		t.Errorf("input.query(qclip) = %v, want %d floats", clip["input.query(qclip)"], testDim)
	}
	if _, ok := clip["input.query(q)"]; ok {
		t.Error("clip arm carried the bge-m3 input.query(q)")
	}

	// THE isolation property: both arms scope to the SAME context tenant group.
	textGroup := env.vespa.bodyForProfile(t, "hybrid")["streaming.groupname"]
	clipGroup := clip["streaming.groupname"]
	if textGroup != "tenant-a" || clipGroup != "tenant-a" {
		t.Errorf("groupnames = text:%v clip:%v, want both tenant-a", textGroup, clipGroup)
	}
}

// TestClipArmIsolationNeverFromRequest: request content that names another
// tenant must not move either arm's streaming group off the verified tenant.
func TestClipArmIsolationNeverFromRequest(t *testing.T) {
	env := newQueryEnv(t)
	env.vespa.setProfileFixture("clip", clipFixture)

	_, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{
		Query: "tenant-b private photos from:tenant-b",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, profile := range []string{"hybrid", "clip"} {
		if got := env.vespa.bodyForProfile(t, profile)["streaming.groupname"]; got != "tenant-a" {
			t.Errorf("[%s] streaming.groupname = %v, want tenant-a", profile, got)
		}
	}
}

// TestClipArmMergeDedupeByDocID: a doc matched by BOTH arms appears once with
// the higher (blended) score; a purely-visual doc still appears. The CLIP
// match supplies media deep-link fields the text arm lacked.
func TestClipArmMergeDedupeByDocID(t *testing.T) {
	env := newQueryEnv(t)
	// The CLIP arm returns doc-1 (also returned by the text arm, with a higher
	// score) and doc-img (visual-only). doc-1 must dedupe to one hit.
	const clipOverlap = `{
      "root": {
        "fields": {"totalCount": 2},
        "children": [
          {
            "relevance": 0.99,
            "fields": {
              "doc_id": "doc-1",
              "type": "EMAIL",
              "title": "Quarterly planning",
              "thumbnail_key": "thumb/doc-1.jpg",
              "chunk_modalities": ["caption"],
              "chunk_starts_ms": [0],
              "chunk_ends_ms": [0],
              "summaryfeatures": {"closest(clip_embedding)": {"cells": {"0": 1.0}}}
            }
          },
          {
            "relevance": 0.50,
            "fields": {
              "doc_id": "doc-img",
              "type": "IMAGE",
              "title": "Beach sunset.jpg",
              "thumbnail_key": "thumb/doc-img.jpg",
              "chunk_modalities": ["caption"],
              "chunk_starts_ms": [0],
              "chunk_ends_ms": [0],
              "summaryfeatures": {"closest(clip_embedding)": {"cells": {"0": 1.0}}}
            }
          }
        ]
      }
    }`
	env.vespa.setProfileFixture("clip", clipOverlap)

	resp, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{Query: "quarterly sunset"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Text arm returned doc-1, doc-2; CLIP arm returned doc-1, doc-img. Union
	// is {doc-1, doc-2, doc-img} — doc-1 deduped.
	seen := map[string]int{}
	var docOne *queryv1.Hit
	for _, h := range resp.GetHits() {
		seen[h.GetDocId()]++
		if h.GetDocId() == "doc-1" {
			docOne = h
		}
	}
	if seen["doc-1"] != 1 {
		t.Errorf("doc-1 appeared %d times, want 1 (deduped across arms)", seen["doc-1"])
	}
	if seen["doc-img"] != 1 {
		t.Error("purely-visual doc-img did not appear in the merged result")
	}
	if docOne == nil {
		t.Fatal("doc-1 missing from merged result")
	}
	// doc-1 matched both arms: keeps the higher (CLIP 0.99 > text 0.87) score.
	if docOne.GetScore() != 0.99 {
		t.Errorf("doc-1 score = %v, want the higher blended 0.99", docOne.GetScore())
	}
	// The CLIP match supplied media fields the text hit lacked.
	if docOne.GetModality() != "caption" || docOne.GetThumbnailKey() != "thumb/doc-1.jpg" {
		t.Errorf("doc-1 media fields = %q/%q, want caption/thumb/doc-1.jpg from the CLIP match",
			docOne.GetModality(), docOne.GetThumbnailKey())
	}
	// Highest score first.
	if resp.GetHits()[0].GetDocId() != "doc-1" {
		t.Errorf("top hit = %s, want doc-1 (highest blended score)", resp.GetHits()[0].GetDocId())
	}
}

// TestClipDownDegradesButTextResultsReturn: a clip service outage drops the
// CLIP arm (degraded includes "clip-unavailable") yet text results still come
// back — never fail closed (ADR-006).
func TestClipDownDegradesButTextResultsReturn(t *testing.T) {
	env := newQueryEnv(t, withClipDown())

	resp, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{Query: "sunset photos"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.GetDegraded() != degradedClipUnavailable {
		t.Errorf("Degraded = %q, want %q", resp.GetDegraded(), degradedClipUnavailable)
	}
	if len(resp.GetHits()) == 0 {
		t.Error("clip-down search returned no text hits (failed closed)")
	}
	// The text arm still ran (one hybrid call); no clip nearestNeighbor issued.
	if b := env.vespa.bodyForProfile(t, "hybrid"); b["ranking.profile"] != "hybrid" {
		t.Error("text/hybrid arm did not run when clip was down")
	}
	for _, b := range env.vespa.recorded() {
		if b["ranking.profile"] == "clip" {
			t.Error("a clip nearestNeighbor was issued despite clip being down")
		}
	}
	// Degraded responses are never cached.
	if _, sets := env.cache.counts(); sets != 0 {
		t.Errorf("cache sets = %d, want 0 for degraded response", sets)
	}
}

// TestClipDownVespaArm covers the other clip failure point: the clip embed
// succeeds but the clip nearestNeighbor (Vespa 'clip' profile) fails — the arm
// is still dropped, text results still served.
func TestClipDownVespaArm(t *testing.T) {
	env := newQueryEnv(t)
	env.vespa.failProfile("clip")

	resp, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{Query: "sunset photos"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.GetDegraded() != degradedClipUnavailable {
		t.Errorf("Degraded = %q, want %q", resp.GetDegraded(), degradedClipUnavailable)
	}
	if len(resp.GetHits()) == 0 {
		t.Error("clip-arm Vespa failure dropped text results too (failed closed)")
	}
}

// TestClipDimMismatchDropsArm: the clip service returns the wrong dimension
// (operator error per ADR-013) — the arm is dropped, the search still serves.
func TestClipDimMismatchDropsArm(t *testing.T) {
	env := newQueryEnv(t, withClipDim(testDim-1))

	resp, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{Query: "sunset photos"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.GetDegraded() != degradedClipUnavailable {
		t.Errorf("Degraded = %q, want %q", resp.GetDegraded(), degradedClipUnavailable)
	}
	for _, b := range env.vespa.recorded() {
		if _, ok := b["input.query(qclip)"]; ok {
			t.Error("a wrong-dimension CLIP vector reached Vespa")
		}
	}
}

// TestClipPlusTeiDownComposesDegraded: both the bge-m3 (TEI) and CLIP signals
// fail — the degraded marker composes both reasons.
func TestClipPlusTeiDownComposesDegraded(t *testing.T) {
	env := newQueryEnv(t, withTEIDown(), withClipDown())

	resp, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{Query: "sunset photos"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Deterministic order: keyword-only (bge-m3 text arm) before clip-unavailable.
	if got := resp.GetDegraded(); got != degradedKeywordOnly+","+degradedClipUnavailable {
		t.Errorf("Degraded = %q, want %q", got, degradedKeywordOnly+","+degradedClipUnavailable)
	}
	if len(resp.GetHits()) == 0 {
		t.Error("dual degradation returned no hits (failed closed)")
	}
}

// TestTeiDownClipVespaArmDownComposesDegraded exercises the other ordering
// path: TEI down (text arm -> keyword), CLIP embed succeeds but the CLIP
// nearestNeighbor fails inside the merge. The composed marker must still be
// "keyword-only,clip-unavailable".
func TestTeiDownClipVespaArmDownComposesDegraded(t *testing.T) {
	env := newQueryEnv(t, withTEIDown())
	env.vespa.failProfile("clip")

	resp, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{Query: "sunset photos"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := resp.GetDegraded(); got != degradedKeywordOnly+","+degradedClipUnavailable {
		t.Errorf("Degraded = %q, want %q", got, degradedKeywordOnly+","+degradedClipUnavailable)
	}
	if len(resp.GetHits()) == 0 {
		t.Error("returned no hits (failed closed)")
	}
}

// TestKeywordModeSkipsClipArm: KEYWORD/VECTOR are caller-constrained — the
// CLIP arm must not run, preserving M2 behavior exactly.
func TestKeywordModeSkipsClipArm(t *testing.T) {
	for _, mode := range []queryv1.SearchMode{queryv1.SearchMode_KEYWORD, queryv1.SearchMode_VECTOR} {
		t.Run(mode.String(), func(t *testing.T) {
			env := newQueryEnv(t)
			_, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{
				Query: "sunset photos",
				Mode:  mode,
			})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if got := env.clipCalls.Load(); got != 0 {
				t.Errorf("clip calls = %d in %s mode, want 0", got, mode)
			}
			for _, b := range env.vespa.recorded() {
				if b["ranking.profile"] == "clip" {
					t.Errorf("clip arm ran in %s mode", mode)
				}
			}
		})
	}
}

// TestClipMergePagination: with the CLIP arm active, offset/limit apply to the
// MERGED ranking — each arm is fetched from offset 0 up to offset+limit.
func TestClipMergePagination(t *testing.T) {
	env := newQueryEnv(t)
	env.vespa.setProfileFixture("clip", clipFixture)

	_, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{
		Query:  "sunset",
		Limit:  5,
		Offset: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Each arm must request the full [0, offset+limit) prefix at offset 0.
	for _, profile := range []string{"hybrid", "clip"} {
		b := env.vespa.bodyForProfile(t, profile)
		if got := b["hits"].(float64); got != 15 {
			t.Errorf("[%s] hits = %v, want offset+limit=15", profile, got)
		}
		if got := b["offset"].(float64); got != 0 {
			t.Errorf("[%s] offset = %v, want 0 (paginated after merge)", profile, got)
		}
	}
}

func TestMergeHelpers(t *testing.T) {
	hit := func(id string, score float64) *queryv1.Hit {
		return &queryv1.Hit{DocId: id, Score: score}
	}

	t.Run("mergeHits orders by score and keeps the higher on overlap", func(t *testing.T) {
		text := []*queryv1.Hit{hit("a", 0.5), hit("b", 0.9)}
		clip := []*queryv1.Hit{hit("a", 0.8), hit("c", 0.7)}
		got := mergeHits(text, clip)
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3 (a deduped)", len(got))
		}
		if got[0].GetDocId() != "b" || got[1].GetDocId() != "a" || got[2].GetDocId() != "c" {
			t.Errorf("order = %s,%s,%s, want b,a,c", got[0].GetDocId(), got[1].GetDocId(), got[2].GetDocId())
		}
		if got[1].GetScore() != 0.8 {
			t.Errorf("a score = %v, want the higher 0.8", got[1].GetScore())
		}
	})

	t.Run("pageHits offset beyond results yields nil", func(t *testing.T) {
		hits := []*queryv1.Hit{hit("a", 1), hit("b", 1)}
		if got := pageHits(hits, 5, 10); got != nil {
			t.Errorf("pageHits past the end = %v, want nil", got)
		}
		if got := pageHits(hits, 1, 10); len(got) != 1 || got[0].GetDocId() != "b" {
			t.Errorf("pageHits(offset 1) = %v, want [b]", got)
		}
	})

	t.Run("mergedTotal never undercounts an arm", func(t *testing.T) {
		text := vespaResult{Total: 7}
		clip := vespaResult{Total: 3}
		if got := mergedTotal(text, clip, 2); got != 7 {
			t.Errorf("mergedTotal = %d, want 7 (the dominant text arm)", got)
		}
		if got := mergedTotal(vespaResult{Total: 1}, vespaResult{Total: 1}, 5); got != 5 {
			t.Errorf("mergedTotal = %d, want 5 (distinct observed exceeds arm totals)", got)
		}
	})
}

// TestClipCacheKeyDistinguishesArm: a result computed with the CLIP arm is not
// served for a request that would not plan it (different cache namespace).
func TestClipCacheKeyDistinguishesArm(t *testing.T) {
	env := newQueryEnv(t)
	env.vespa.setProfileFixture("clip", clipFixture)
	ctx := tenantCtx(t, "tenant-a")

	// HYBRID populates the CLIP-arm cache namespace.
	if _, err := env.client.Search(ctx, &queryv1.SearchRequest{Query: "sunset", Mode: queryv1.SearchMode_HYBRID}); err != nil {
		t.Fatalf("hybrid search: %v", err)
	}
	vespaCalls := len(env.vespa.recorded())

	// KEYWORD mode plans no CLIP arm: a distinct key, so it must MISS the cache
	// and hit Vespa again rather than serve the CLIP-arm entry.
	if _, err := env.client.Search(ctx, &queryv1.SearchRequest{Query: "sunset", Mode: queryv1.SearchMode_KEYWORD}); err != nil {
		t.Fatalf("keyword search: %v", err)
	}
	if got := len(env.vespa.recorded()); got <= vespaCalls {
		t.Error("keyword query was served the CLIP-arm cache entry (key did not distinguish the arm)")
	}
}
