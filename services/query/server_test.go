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

// vespaStub records every /search/ body and can fail selected ranking
// profiles with HTTP 500. It also serves /state/v1/health for readiness.
type vespaStub struct {
	srv *httptest.Server

	mu           sync.Mutex
	bodies       []map[string]any
	failProfiles map[string]bool
}

func newVespaStub(t *testing.T) *vespaStub {
	t.Helper()
	v := &vespaStub{failProfiles: make(map[string]bool)}
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
		v.mu.Unlock()
		if fail {
			http.Error(w, "injected backend failure", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vespaFixture))
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

func (v *vespaStub) recorded() []map[string]any {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]map[string]any(nil), v.bodies...)
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
	client   queryv1.QueryServiceClient
	rawConn  *grpc.ClientConn // no client interceptor: sends no tenant metadata
	vespa    *vespaStub
	teiCalls *atomic.Int32
	cache    *fakeCache
	teiURL   string
}

// option mutates the environment before the server is built.
type envOption func(*envConfig)

type envConfig struct {
	teiDown bool
	teiDim  int
}

func withTEIDown() envOption       { return func(c *envConfig) { c.teiDown = true } }
func withTEIDim(dim int) envOption { return func(c *envConfig) { c.teiDim = dim } }

func newQueryEnv(t *testing.T, opts ...envOption) *queryEnv {
	t.Helper()
	ec := envConfig{teiDim: testDim}
	for _, o := range opts {
		o(&ec)
	}

	tei, teiCalls := newTEIStub(t, ec.teiDim)
	teiURL := tei.URL
	if ec.teiDown {
		tei.Close() // refuses connections from here on
	}
	vespa := newVespaStub(t)
	cache := newFakeCache()

	srv := newServer(
		newTEIEmbedder(teiURL, testDim, 500*time.Millisecond),
		newVespaClient(vespa.srv.URL),
		cache,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

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
		client:   queryv1.NewQueryServiceClient(conn),
		rawConn:  rawConn,
		vespa:    vespa,
		teiCalls: teiCalls,
		cache:    cache,
		teiURL:   teiURL,
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

	body := env.vespa.lastBody(t)
	wantYQL := `select * from sources * where (userQuery() or ({targetHits:100}nearestNeighbor(embedding,q))) and type contains "EMAIL" and participants contains ({substring:true}"alice")`
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
	if resp.GetDegraded() != degradedKeywordOnly {
		t.Errorf("Degraded = %q, want %q", resp.GetDegraded(), degradedKeywordOnly)
	}
	body := env.vespa.lastBody(t)
	if body["ranking.profile"] != "keyword" {
		t.Errorf("ranking.profile = %v, want keyword", body["ranking.profile"])
	}
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
	if _, ok := env.vespa.lastBody(t)["input.query(q)"]; ok {
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
	if resp.GetDegraded() != degradedKeywordOnly {
		t.Errorf("Degraded = %q, want %q", resp.GetDegraded(), degradedKeywordOnly)
	}
	bodies := env.vespa.recorded()
	if len(bodies) != 2 {
		t.Fatalf("vespa calls = %d, want 2 (hybrid then keyword retry)", len(bodies))
	}
	if bodies[0]["ranking.profile"] != "hybrid" || bodies[1]["ranking.profile"] != "keyword" {
		t.Errorf("profiles = %v,%v, want hybrid,keyword", bodies[0]["ranking.profile"], bodies[1]["ranking.profile"])
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
	body := env.vespa.lastBody(t)
	yql := body["yql"].(string)
	if strings.Contains(yql, "evil") || strings.Contains(yql, "acl") {
		t.Errorf("hostile query text leaked into YQL: %s", yql)
	}
	if body["query"] != hostile {
		t.Errorf("query param = %v, want the raw text %q", body["query"], hostile)
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
	_, err := env.client.Search(tenantCtx(t, "tenant-a"), &queryv1.SearchRequest{
		Query:  "plans",
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
	srv := newServer(nil, nil, newFakeCache(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := srv.Search(context.Background(), &queryv1.SearchRequest{Query: "plans"})
	wantCode(t, err, codes.Unauthenticated)
}
