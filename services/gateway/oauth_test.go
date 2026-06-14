package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/asker/asker/platform/oauth"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
)

// GetConnectorInstance is a test-only method on fakeControlPlane (defined here
// to keep the OAuth surface in this file). It is tenant-scoped exactly like the
// other fake RPCs, so a cross-tenant id returns NotFound.
func (f *fakeControlPlane) GetConnectorInstance(ctx context.Context, req *controlplanev1.GetConnectorInstanceRequest) (*controlplanev1.GetConnectorInstanceResponse, error) {
	tenant, err := fakeCallerTenant(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[tenant][req.GetId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return &controlplanev1.GetConnectorInstanceResponse{Instance: inst}, nil
}

// fakeStateStore is an in-memory oauthStateStore that records every Set and
// gives GetDel its single-use semantics (a second read of the same key fails).
type fakeStateStore struct {
	mu      sync.Mutex
	vals    map[string]string
	setErr  error
	getErr  error
	setKeys []string
}

func newFakeStateStore() *fakeStateStore {
	return &fakeStateStore{vals: map[string]string{}}
}

func (s *fakeStateStore) Set(_ context.Context, key, value string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	s.vals[key] = value
	s.setKeys = append(s.setKeys, key)
	return nil
}

func (s *fakeStateStore) GetDel(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return "", s.getErr
	}
	v, ok := s.vals[key]
	if !ok {
		return "", errStateNotFound
	}
	delete(s.vals, key) // single use
	return v, nil
}

func (s *fakeStateStore) storedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.setKeys...)
}

func (s *fakeStateStore) onlyValue(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.vals) != 1 {
		t.Fatalf("state store holds %d values, want 1", len(s.vals))
	}
	for _, v := range s.vals {
		return v
	}
	return ""
}

// fakeOAuth is a deterministic oauthService: it records what it was asked and
// returns canned values. It never touches the network.
type fakeOAuth struct {
	mu sync.Mutex

	// AuthCodeURL inputs/outputs.
	authProvider    oauth.Provider
	authScopes      []string
	authRedirectURI string
	authState       string
	authChallenge   string
	authURL         string
	authErr         error

	// Exchange inputs/outputs.
	exchProvider    oauth.Provider
	exchCode        string
	exchRedirectURI string
	exchVerifier    string
	exchToken       oauth.Token
	exchErr         error
}

func (f *fakeOAuth) AuthCodeURL(p oauth.Provider, scopes []string, redirectURI, state, challenge string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authProvider, f.authScopes, f.authRedirectURI, f.authState, f.authChallenge =
		p, scopes, redirectURI, state, challenge
	if f.authErr != nil {
		return "", f.authErr
	}
	u := f.authURL
	if u == "" {
		u = "https://provider.example/authorize"
	}
	return appendQueryParam(u, "state", state), nil
}

func (f *fakeOAuth) Exchange(_ context.Context, p oauth.Provider, code, redirectURI, verifier string) (oauth.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exchProvider, f.exchCode, f.exchRedirectURI, f.exchVerifier = p, code, redirectURI, verifier
	if f.exchErr != nil {
		return oauth.Token{}, f.exchErr
	}
	tok := f.exchToken
	if tok.AccessToken == "" {
		tok.AccessToken = "fake-access-token"
	}
	return tok, nil
}

// withOAuth is a newTestEnv option that wires the OAuth fakes + config.
func withOAuth(svc *fakeOAuth, store *fakeStateStore) func(cfg *gatewayConfig, d *deps) {
	return func(_ *gatewayConfig, d *deps) {
		d.oauth = svc
		d.oauthState = store
		// Mark Google + Slack configured so the "configured" gate passes.
		d.oauthCfg = oauth.Config{
			Google: oauth.ProviderConfig{ClientID: "id", ClientSecret: "secret"},
			Slack:  oauth.ProviderConfig{ClientID: "id", ClientSecret: "secret"},
		}
		d.gatewayPublicURL = "https://gw.example"
		d.webAppURL = "https://web.example"
	}
}

// createInstance creates a connector instance of the given type and returns its id.
func createInstance(t *testing.T, env *testEnv, connectorID, configJSON string) string {
	t.Helper()
	body := `{"connector_id":"` + connectorID + `"`
	if configJSON != "" {
		body += `,"config":` + configJSON
	}
	body += `}`
	rec := env.do(http.MethodPost, "/v1/connectors", strings.NewReader(body), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s status = %d, body = %s", connectorID, rec.Code, rec.Body.String())
	}
	return decodeObject(t, rec)["id"].(string)
}

func TestOAuthStartSuccess(t *testing.T) {
	svc := &fakeOAuth{authURL: "https://accounts.example/o/oauth2/auth"}
	store := newFakeStateStore()
	env := newTestEnv(t, withOAuth(svc, store))

	id := createInstance(t, env, "gmail", `{"user_email":"alice@example.com"}`)

	rec := env.do(http.MethodGet, "/v1/connectors/"+id+"/oauth/start", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	authURL := decodeObject(t, rec)["authorize_url"].(string)
	if authURL == "" {
		t.Fatal("authorize_url is empty")
	}

	// The state must have been persisted under exactly one key, and that key
	// must be the `state` carried in the authorize URL.
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("authorize_url not a URL: %v", err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatal("authorize_url carries no state")
	}
	keys := store.storedKeys()
	if len(keys) != 1 || keys[0] != statePrefix+state {
		t.Fatalf("stored keys = %v, want [%s]", keys, statePrefix+state)
	}

	// The persisted flow state must carry the JWT tenant (testSubject), the
	// instance id, the provider, and a PKCE verifier — never anything from a
	// request body.
	var flow oauthFlowState
	if err := json.Unmarshal([]byte(store.onlyValue(t)), &flow); err != nil {
		t.Fatalf("state value not JSON: %v", err)
	}
	if flow.Tenant != testSubject {
		t.Errorf("state tenant = %q, want %q", flow.Tenant, testSubject)
	}
	if flow.InstanceID != id || flow.ConnectorID != "gmail" || flow.Provider != oauth.Google {
		t.Errorf("state = %+v, want instance %s / gmail / google", flow, id)
	}
	if flow.Verifier == "" {
		t.Error("state has no PKCE verifier")
	}

	// AuthCodeURL must have been called with the gmail scope, the gateway's
	// redirect_uri, and an S256 challenge derived from that verifier.
	if svc.authProvider != oauth.Google {
		t.Errorf("AuthCodeURL provider = %q, want google", svc.authProvider)
	}
	if svc.authRedirectURI != "https://gw.example/v1/oauth/callback" {
		t.Errorf("redirect_uri = %q", svc.authRedirectURI)
	}
	if svc.authChallenge == "" {
		t.Error("AuthCodeURL got empty PKCE challenge")
	}
	if len(svc.authScopes) == 0 || !strings.Contains(svc.authScopes[0], "gmail.readonly") {
		t.Errorf("scopes = %v, want gmail.readonly", svc.authScopes)
	}
	// Google connectors with a user_email config get a login_hint on the URL.
	if got := u.Query().Get("login_hint"); got != "alice@example.com" {
		t.Errorf("login_hint = %q, want alice@example.com", got)
	}
}

// TestOAuthStartNonOAuthConnector proves a known but non-OAuth connector 400s
// and persists no state.
func TestOAuthStartNonOAuthConnector(t *testing.T) {
	svc := &fakeOAuth{}
	store := newFakeStateStore()
	env := newTestEnv(t, withOAuth(svc, store))

	id := createInstance(t, env, "upload", "")

	rec := env.do(http.MethodGet, "/v1/connectors/"+id+"/oauth/start", nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if keys := store.storedKeys(); len(keys) != 0 {
		t.Errorf("non-OAuth start persisted state %v, want none", keys)
	}
}

// TestOAuthStartUnknownInstance proves a missing/cross-tenant instance 404s.
func TestOAuthStartUnknownInstance(t *testing.T) {
	svc := &fakeOAuth{}
	store := newFakeStateStore()
	env := newTestEnv(t, withOAuth(svc, store))

	rec := env.do(http.MethodGet, "/v1/connectors/no-such/oauth/start", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if keys := store.storedKeys(); len(keys) != 0 {
		t.Errorf("persisted state for unknown instance: %v", keys)
	}
}

// TestOAuthStartProviderNotConfigured proves a connector whose provider lacks
// client creds 400s.
func TestOAuthStartProviderNotConfigured(t *testing.T) {
	svc := &fakeOAuth{}
	store := newFakeStateStore()
	env := newTestEnv(t, func(cfg *gatewayConfig, d *deps) {
		withOAuth(svc, store)(cfg, d)
		// Drop Google's creds so it is unconfigured.
		d.oauthCfg = oauth.Config{}
	})

	id := createInstance(t, env, "gmail", "")
	rec := env.do(http.MethodGet, "/v1/connectors/"+id+"/oauth/start", nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestOAuthStartRequiresAuth(t *testing.T) {
	svc := &fakeOAuth{}
	store := newFakeStateStore()
	env := newTestEnv(t, withOAuth(svc, store))

	req := httptest.NewRequest(http.MethodGet, "/v1/connectors/inst-1/oauth/start", nil)
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("start without bearer status = %d, want 401", rec.Code)
	}
}

// startFlow runs /oauth/start and returns the issued state + the instance id.
func startFlow(t *testing.T, env *testEnv, store *fakeStateStore, connectorID string) (state, instanceID string) {
	t.Helper()
	instanceID = createInstance(t, env, connectorID, "")
	rec := env.do(http.MethodGet, "/v1/connectors/"+instanceID+"/oauth/start", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d", rec.Code)
	}
	u, err := url.Parse(decodeObject(t, rec)["authorize_url"].(string))
	if err != nil {
		t.Fatalf("authorize_url: %v", err)
	}
	return u.Query().Get("state"), instanceID
}

// callback issues an UNAUTHENTICATED GET to the public callback (no bearer).
func callback(t *testing.T, env *testEnv, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/oauth/callback?"+query, nil)
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

func TestOAuthCallbackSuccess(t *testing.T) {
	svc := &fakeOAuth{exchToken: oauth.Token{AccessToken: "fake-gmail-token:alice@example.com", RefreshToken: "r1"}}
	store := newFakeStateStore()
	env := newTestEnv(t, withOAuth(svc, store))

	state, id := startFlow(t, env, store, "gmail")

	rec := callback(t, env, "state="+state+"&code=auth-code-xyz")
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302 (body: %s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "https://web.example/connectors?oauth=connected" {
		t.Fatalf("redirect = %q, want connected", loc)
	}

	// Exchange must have received the code + the PKCE verifier captured at start.
	if svc.exchCode != "auth-code-xyz" {
		t.Errorf("exchange code = %q", svc.exchCode)
	}
	if svc.exchVerifier == "" {
		t.Error("exchange got no PKCE verifier")
	}

	// CRUX: the token must be stored under the STATE's tenant (testSubject), and
	// the stored blob must be an Asker OAuth token marked with the connector.
	env.control.mu.Lock()
	raw := env.control.tokens[testSubject+"/"+id]
	env.control.mu.Unlock()
	if raw == nil {
		t.Fatalf("no token stored under %s/%s; stored keys = %v", testSubject, id, tokenKeys(env))
	}
	tok, ok := oauth.Parse(raw)
	if !ok {
		t.Fatalf("stored token is not an Asker OAuth blob: %q", raw)
	}
	if tok.ConnectorID != "gmail" || tok.Provider != oauth.Google {
		t.Errorf("stored token = %+v, want gmail/google", tok)
	}
	if tok.AccessToken != "fake-gmail-token:alice@example.com" {
		t.Errorf("stored access token = %q", tok.AccessToken)
	}

	// State was single-use: it is gone now.
	if keys := store.storedKeys(); len(store.vals) != 0 {
		t.Errorf("state survived callback: keys=%v vals=%v", keys, store.vals)
	}
}

// TestOAuthCallbackTenantFromStateNotRequest is the leakage-suite assertion:
// the token lands under the state's tenant even though the unauthenticated
// callback request supplies NO tenant and could not be trusted for one.
func TestOAuthCallbackTenantFromStateNotRequest(t *testing.T) {
	svc := &fakeOAuth{}
	store := newFakeStateStore()
	env := newTestEnv(t, withOAuth(svc, store))

	// Start the flow as a DIFFERENT tenant ("tenant-victim") by minting that
	// token for the start call, so the state captures tenant-victim.
	instanceID := createInstanceAs(t, env, "gmail", "tenant-victim")
	startRec := doAs(t, env, http.MethodGet, "/v1/connectors/"+instanceID+"/oauth/start", "tenant-victim")
	if startRec.Code != http.StatusOK {
		t.Fatalf("start status = %d", startRec.Code)
	}
	u, _ := url.Parse(decodeObject(t, startRec)["authorize_url"].(string))
	state := u.Query().Get("state")

	// The callback is unauthenticated: there is no way for it to name a tenant.
	rec := callback(t, env, "state="+state+"&code=c")
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", rec.Code)
	}

	// The token must be under tenant-victim (from the state), and NOT under
	// testSubject (the default request identity the callback never presented).
	env.control.mu.Lock()
	defer env.control.mu.Unlock()
	if env.control.tokens["tenant-victim/"+instanceID] == nil {
		t.Errorf("token not stored under state tenant 'tenant-victim'; tokens=%v", keysOf(env.control.tokens))
	}
	if env.control.tokens[testSubject+"/"+instanceID] != nil {
		t.Error("token leaked under request-derived tenant — tenant MUST come from state")
	}
}

func TestOAuthCallbackBadState(t *testing.T) {
	for name, q := range map[string]string{
		"missing state":   "code=c",
		"unknown state":   "state=never-issued&code=c",
		"missing code":    "state=ISSUED",
		"provider denied": "state=ISSUED&error=access_denied",
	} {
		t.Run(name, func(t *testing.T) {
			svc := &fakeOAuth{}
			store := newFakeStateStore()
			env := newTestEnv(t, withOAuth(svc, store))

			query := q
			if strings.Contains(q, "ISSUED") {
				state, _ := startFlow(t, env, store, "gmail")
				query = strings.ReplaceAll(q, "ISSUED", state)
			}

			rec := callback(t, env, query)
			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302", rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != "https://web.example/connectors?oauth=error" {
				t.Fatalf("redirect = %q, want error", loc)
			}
			// Nothing stored on any error path.
			env.control.mu.Lock()
			n := len(env.control.tokens)
			env.control.mu.Unlock()
			if n != 0 {
				t.Errorf("error path stored %d tokens, want 0", n)
			}
		})
	}
}

// TestOAuthCallbackReplay proves a state is single-use: the second callback
// with the same state fails and stores nothing the second time.
func TestOAuthCallbackReplay(t *testing.T) {
	svc := &fakeOAuth{}
	store := newFakeStateStore()
	env := newTestEnv(t, withOAuth(svc, store))

	state, id := startFlow(t, env, store, "gmail")

	rec1 := callback(t, env, "state="+state+"&code=c1")
	if rec1.Code != http.StatusFound || rec1.Header().Get("Location") != "https://web.example/connectors?oauth=connected" {
		t.Fatalf("first callback = %d %q", rec1.Code, rec1.Header().Get("Location"))
	}

	rec2 := callback(t, env, "state="+state+"&code=c2")
	if loc := rec2.Header().Get("Location"); loc != "https://web.example/connectors?oauth=error" {
		t.Fatalf("replay redirect = %q, want error", loc)
	}
	// Still exactly one stored token (the replay stored nothing).
	env.control.mu.Lock()
	defer env.control.mu.Unlock()
	if env.control.tokens[testSubject+"/"+id] == nil || len(env.control.tokens) != 1 {
		t.Errorf("replay changed token store: %v", keysOf(env.control.tokens))
	}
}

// TestOAuthCallbackExchangeFailure proves a failed token exchange redirects to
// error and stores nothing (the state is still consumed, so no replay).
func TestOAuthCallbackExchangeFailure(t *testing.T) {
	svc := &fakeOAuth{exchErr: status.Error(codes.Unavailable, "provider down")}
	store := newFakeStateStore()
	env := newTestEnv(t, withOAuth(svc, store))

	state, _ := startFlow(t, env, store, "gmail")
	rec := callback(t, env, "state="+state+"&code=c")
	if loc := rec.Header().Get("Location"); loc != "https://web.example/connectors?oauth=error" {
		t.Fatalf("redirect = %q, want error", loc)
	}
	env.control.mu.Lock()
	defer env.control.mu.Unlock()
	if len(env.control.tokens) != 0 {
		t.Errorf("exchange failure stored a token: %v", keysOf(env.control.tokens))
	}
}

// --- small test helpers for the cross-tenant assertions ---

// createInstanceAs creates an instance authenticated as the given tenant
// (sub claim) and returns its id.
func createInstanceAs(t *testing.T, env *testEnv, connectorID, sub string) string {
	t.Helper()
	rec := doAs(t, env, http.MethodPost, "/v1/connectors", sub, `{"connector_id":"`+connectorID+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create as %s status = %d", sub, rec.Code)
	}
	return decodeObject(t, rec)["id"].(string)
}

// doAs sends an authed request as the given subject (tenant). The optional last
// arg is the request body.
func doAs(t *testing.T, env *testEnv, method, path, sub string, body ...string) *httptest.ResponseRecorder {
	t.Helper()
	var r *strings.Reader
	if len(body) > 0 {
		r = strings.NewReader(body[0])
	} else {
		r = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", env.bearerFor(sub))
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func tokenKeys(env *testEnv) []string {
	env.control.mu.Lock()
	defer env.control.mu.Unlock()
	return keysOf(env.control.tokens)
}

// TestStartIssuesUniqueState proves two starts mint distinct states (entropy).
func TestStartIssuesUniqueState(t *testing.T) {
	svc := &fakeOAuth{}
	store := newFakeStateStore()
	env := newTestEnv(t, withOAuth(svc, store))
	id := createInstance(t, env, "gmail", "")

	seen := map[string]bool{}
	for range 5 {
		rec := env.do(http.MethodGet, "/v1/connectors/"+id+"/oauth/start", nil, nil)
		u, _ := url.Parse(decodeObject(t, rec)["authorize_url"].(string))
		s := u.Query().Get("state")
		if s == "" || seen[s] {
			t.Fatalf("state %q repeated or empty", s)
		}
		seen[s] = true
	}
}

// Ensure the fake instance created in the cross-tenant test really is scoped to
// the right tenant (sanity that the helper minted the intended sub).
func TestCreateInstanceAsScopesTenant(t *testing.T) {
	env := newTestEnv(t)
	id := createInstanceAs(t, env, "gmail", "tenant-x")
	env.control.mu.Lock()
	defer env.control.mu.Unlock()
	if _, ok := env.control.instances["tenant-x"][id]; !ok {
		t.Errorf("instance %s not under tenant-x", id)
	}
}
