package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/platform/oauth"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	"github.com/asker/asker/platform/safehttp"
	"github.com/asker/asker/platform/tenancy"
)

// fakeRefresher is an injectable tokenRefresher: it records the call and
// returns a scripted result. It lets the no-refresh / refresh-error cases be
// asserted without a provider HTTP endpoint.
type fakeRefresher struct {
	calls atomic.Int32
	out   oauth.Token
	err   error
}

func (f *fakeRefresher) Refresh(_ context.Context, _ oauth.Provider, t oauth.Token) (oauth.Token, error) {
	f.calls.Add(1)
	if f.err != nil {
		return oauth.Token{}, f.err
	}
	if f.out.AccessToken == "" {
		return t, nil
	}
	return f.out, nil
}

// tctxFor builds a tenant-scoped context the way syncOnce does, so fetchToken's
// GetToken/PutToken calls carry x-asker-tenant for tenantA.
func tctxFor(t *testing.T) context.Context {
	t.Helper()
	return tenancy.WithContext(context.Background(), mustTenant(t, tenantA))
}

// mustMarshal serializes an oauth.Token for vault seeding.
func mustMarshal(t *testing.T, tok oauth.Token) []byte {
	t.Helper()
	b, err := oauth.Marshal(tok)
	if err != nil {
		t.Fatalf("oauth.Marshal: %v", err)
	}
	return b
}

// (a) A legacy / manually-pasted opaque token is passed through verbatim — no
// parse, no refresh, no PutToken. This is the fake-gmail / manual-token path
// the m1 + leakage e2e rely on.
func TestFetchTokenLegacyPassthrough(t *testing.T) {
	conn := &fakeConnector{id: "gmail"}
	ref := &fakeRefresher{}
	r := newRig(t, conn, func(o *schedulerOpts) { o.oauth = ref })
	r.cpFake.addInstance(instGmail, tenantA, "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)

	legacy := []byte("fake-gmail-token:alice@example.com")
	r.cpFake.setToken(instGmail, legacy)

	got, err := r.sch.fetchToken(tctxFor(t), instGmail)
	if err != nil {
		t.Fatalf("fetchToken: %v", err)
	}
	if string(got) != string(legacy) {
		t.Errorf("legacy token = %q, want verbatim %q", got, legacy)
	}
	if n := ref.calls.Load(); n != 0 {
		t.Errorf("Refresh called %d times for a legacy token, want 0", n)
	}
	if w := r.cpFake.tokenWrites(); len(w) != 0 {
		t.Errorf("PutToken called %d times for a legacy token, want 0", len(w))
	}
}

// A missing token (AuthNone / not yet connected) yields nil, no refresh.
func TestFetchTokenAbsentIsNil(t *testing.T) {
	conn := &fakeConnector{id: "gmail"}
	ref := &fakeRefresher{}
	r := newRig(t, conn, func(o *schedulerOpts) { o.oauth = ref })
	r.cpFake.addInstance(instGmail, tenantA, "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)

	got, err := r.sch.fetchToken(tctxFor(t), instGmail)
	if err != nil {
		t.Fatalf("fetchToken: %v", err)
	}
	if got != nil {
		t.Errorf("absent token = %q, want nil", got)
	}
	if n := ref.calls.Load(); n != 0 {
		t.Errorf("Refresh called %d times, want 0", n)
	}
}

// (b) A fresh (not-yet-due) OAuth token: its access token is used directly, no
// Refresh, no PutToken.
func TestFetchTokenFreshOAuthNoRefresh(t *testing.T) {
	conn := &fakeConnector{id: "gmail"}
	ref := &fakeRefresher{}
	r := newRig(t, conn, func(o *schedulerOpts) {
		o.oauth = ref
		o.now = func() time.Time { return time.Unix(1_000_000, 0) }
	})
	r.cpFake.addInstance(instGmail, tenantA, "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)

	tok := oauth.Token{
		Provider:     oauth.Google,
		ConnectorID:  "gmail",
		AccessToken:  "access-fresh",
		RefreshToken: "refresh-1",
		TokenType:    "Bearer",
		Expiry:       time.Unix(1_000_000, 0).Add(10 * time.Minute), // well beyond skew
	}
	r.cpFake.setToken(instGmail, mustMarshal(t, tok))

	got, err := r.sch.fetchToken(tctxFor(t), instGmail)
	if err != nil {
		t.Fatalf("fetchToken: %v", err)
	}
	if string(got) != "access-fresh" {
		t.Errorf("bearer = %q, want the stored access token %q", got, "access-fresh")
	}
	if n := ref.calls.Load(); n != 0 {
		t.Errorf("Refresh called %d times for a fresh token, want 0", n)
	}
	if w := r.cpFake.tokenWrites(); len(w) != 0 {
		t.Errorf("PutToken called %d times for a fresh token, want 0", len(w))
	}
}

// (c) An expired OAuth token: Refresh is called, the NEW credential is
// re-stored via PutToken (under the instance tenant), and the NEW access token
// is what reaches the connector. Uses a REAL oauth.Service against an httptest
// token endpoint so the whole refresh path is exercised.
func TestFetchTokenExpiredRefreshesAndRestores(t *testing.T) {
	var refreshCalls atomic.Int32
	// Fake Google token endpoint: standard OAuth2 refresh-grant response.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		refreshCalls.Add(1)
		if err := req.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if g := req.Form.Get("grant_type"); g != "refresh_token" {
			http.Error(w, "want refresh_token grant, got "+g, http.StatusBadRequest)
			return
		}
		if rt := req.Form.Get("refresh_token"); rt != "refresh-1" {
			http.Error(w, "unexpected refresh token", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-REFRESHED",
			"token_type":   "Bearer",
			"expires_in":   3600,
			// No refresh_token field: Google often omits it; the carry-forward
			// keeps refresh-1.
		})
	}))
	t.Cleanup(tokenSrv.Close)

	oauthCfg := oauth.Config{Google: oauth.ProviderConfig{
		ClientID:     "dev-client",
		ClientSecret: "dev-secret",
		AuthURL:      tokenSrv.URL + "/authorize",
		TokenURL:     tokenSrv.URL + "/token",
	}}
	svc := oauth.New(oauthCfg, safehttp.NewClientOrDefault(safehttp.WithAllowPrivate(true)))

	conn := &fakeConnector{id: "gmail"}
	now := time.Unix(2_000_000, 0)
	r := newRig(t, conn, func(o *schedulerOpts) {
		o.oauth = svc
		o.now = func() time.Time { return now }
	})
	r.cpFake.addInstance(instGmail, tenantA, "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)

	expired := oauth.Token{
		Provider:     oauth.Google,
		ConnectorID:  "gmail",
		AccessToken:  "access-STALE",
		RefreshToken: "refresh-1",
		TokenType:    "Bearer",
		Expiry:       now.Add(-time.Minute), // already past
	}
	r.cpFake.setToken(instGmail, mustMarshal(t, expired))

	got, err := r.sch.fetchToken(tctxFor(t), instGmail)
	if err != nil {
		t.Fatalf("fetchToken: %v", err)
	}
	if string(got) != "access-REFRESHED" {
		t.Errorf("bearer = %q, want the refreshed access token", got)
	}
	if n := refreshCalls.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times, want exactly 1", n)
	}

	// PutToken re-stored the refreshed credential, under the instance tenant.
	writes := r.cpFake.tokenWrites()
	if len(writes) != 1 {
		t.Fatalf("PutToken called %d times, want exactly 1", len(writes))
	}
	if writes[0].tenant != tenantA {
		t.Errorf("PutToken tenant = %q, want %q (from trusted tctx)", writes[0].tenant, tenantA)
	}
	stored, ok := oauth.Parse(writes[0].token)
	if !ok {
		t.Fatal("re-stored blob is not an Asker OAuth token")
	}
	if stored.AccessToken != "access-REFRESHED" {
		t.Errorf("re-stored access token = %q, want refreshed", stored.AccessToken)
	}
	if stored.RefreshToken != "refresh-1" {
		t.Errorf("re-stored refresh token = %q, want carried-forward refresh-1", stored.RefreshToken)
	}

	// A second fetch now sees a FRESH token and does NOT refresh again.
	got2, err := r.sch.fetchToken(tctxFor(t), instGmail)
	if err != nil {
		t.Fatalf("fetchToken (2nd): %v", err)
	}
	if string(got2) != "access-REFRESHED" {
		t.Errorf("2nd bearer = %q, want the stored refreshed token", got2)
	}
	if n := refreshCalls.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times after 2nd fetch, want still 1 (no needless refresh)", n)
	}
}

// (d) A refresh failure fails the run with a clear error and does NOT re-store
// or hand back a stale/empty token.
func TestFetchTokenRefreshErrorFailsRun(t *testing.T) {
	conn := &fakeConnector{id: "gmail"}
	ref := &fakeRefresher{err: errors.New("provider said no")}
	now := time.Unix(3_000_000, 0)
	r := newRig(t, conn, func(o *schedulerOpts) {
		o.oauth = ref
		o.now = func() time.Time { return now }
	})
	r.cpFake.addInstance(instGmail, tenantA, "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)

	expired := oauth.Token{
		Provider:     oauth.Google,
		ConnectorID:  "gmail",
		AccessToken:  "access-STALE",
		RefreshToken: "refresh-1",
		Expiry:       now.Add(-time.Minute),
	}
	r.cpFake.setToken(instGmail, mustMarshal(t, expired))

	got, err := r.sch.fetchToken(tctxFor(t), instGmail)
	if err == nil {
		t.Fatalf("fetchToken returned bearer %q, want an error", got)
	}
	if got != nil {
		t.Errorf("bearer on refresh error = %q, want nil (no stale token)", got)
	}
	if n := ref.calls.Load(); n != 1 {
		t.Errorf("Refresh called %d times, want 1", n)
	}
	if w := r.cpFake.tokenWrites(); len(w) != 0 {
		t.Errorf("PutToken called %d times after refresh error, want 0", len(w))
	}
}

// An OAuth token due for refresh with NO refresher configured fails the run
// rather than handing the connector a stale bearer.
func TestFetchTokenNeedsRefreshNoServiceFails(t *testing.T) {
	conn := &fakeConnector{id: "gmail"}
	now := time.Unix(4_000_000, 0)
	r := newRig(t, conn, func(o *schedulerOpts) {
		o.oauth = nil // no provider configured
		o.now = func() time.Time { return now }
	})
	r.cpFake.addInstance(instGmail, tenantA, "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)

	expired := oauth.Token{
		Provider:     oauth.Google,
		AccessToken:  "access-STALE",
		RefreshToken: "refresh-1",
		Expiry:       now.Add(-time.Minute),
	}
	r.cpFake.setToken(instGmail, mustMarshal(t, expired))

	if got, err := r.sch.fetchToken(tctxFor(t), instGmail); err == nil {
		t.Fatalf("fetchToken returned %q, want an error when refresh is due but no service configured", got)
	}
}

// A non-expiring OAuth token (no refresh token, zero expiry — e.g. a Slack user
// token) is used as-is, never refreshed.
func TestFetchTokenNonExpiringUsedAsIs(t *testing.T) {
	conn := &fakeConnector{id: "slack"}
	ref := &fakeRefresher{}
	r := newRig(t, conn, func(o *schedulerOpts) { o.oauth = ref })
	r.cpFake.addInstance(instGmail, tenantA, "slack", nil, controlplanev1.ConnectorStatus_ACTIVE)

	tok := oauth.Token{
		Provider:    oauth.Slack,
		AccessToken: "xoxp-user-token",
		// no refresh token, zero Expiry
	}
	r.cpFake.setToken(instGmail, mustMarshal(t, tok))

	got, err := r.sch.fetchToken(tctxFor(t), instGmail)
	if err != nil {
		t.Fatalf("fetchToken: %v", err)
	}
	if string(got) != "xoxp-user-token" {
		t.Errorf("bearer = %q, want the stored access token", got)
	}
	if n := ref.calls.Load(); n != 0 {
		t.Errorf("Refresh called %d times for a non-expiring token, want 0", n)
	}
}

// The refreshed access token flows all the way into sdk.Config.Token through a
// real sync pass (buildConfig -> FullSync), end to end, with the instance
// tenant intact.
func TestRefreshedTokenReachesConnectorConfig(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-LIVE",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	t.Cleanup(tokenSrv.Close)

	svc := oauth.New(oauth.Config{Google: oauth.ProviderConfig{
		ClientID:     "dev-client",
		ClientSecret: "dev-secret",
		TokenURL:     tokenSrv.URL + "/token",
	}}, safehttp.NewClientOrDefault(safehttp.WithAllowPrivate(true)))

	var seenToken atomic.Value // string
	conn := &fakeConnector{id: "gmail"}
	conn.fullSyncFn = func(_ context.Context, cfg sdk.Config, _ sdk.Emit) (sdk.Cursor, error) {
		seenToken.Store(string(cfg.Token))
		return "done", nil
	}
	now := time.Unix(5_000_000, 0)
	r := newRig(t, conn, func(o *schedulerOpts) {
		o.oauth = svc
		o.now = func() time.Time { return now }
	})
	r.cpFake.addInstance(instGmail, tenantA, "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)
	r.cpFake.setToken(instGmail, mustMarshal(t, oauth.Token{
		Provider:     oauth.Google,
		ConnectorID:  "gmail",
		AccessToken:  "access-STALE",
		RefreshToken: "refresh-1",
		Expiry:       now.Add(-time.Minute),
	}))
	r.start(t)

	waitFor(t, 5*time.Second, func() bool {
		v, _ := seenToken.Load().(string)
		return v == "access-LIVE"
	}, "connector received the refreshed access token in cfg.Token")

	if w := r.cpFake.tokenWrites(); len(w) == 0 || w[0].tenant != tenantA {
		t.Errorf("refreshed token not re-stored under tenant %q: %+v", tenantA, w)
	}
}
