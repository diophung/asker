package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	testIssuer   = "http://localhost:8081/realms/asker"
	testAudience = "asker-web"
	testSubject  = "user-123"
	testEmail    = "alice@example.com"
)

// testIdP is a fake identity provider: an RSA key, a JWT signer, and an
// httptest server publishing the matching JWKS.
type testIdP struct {
	signer jose.Signer
	jwks   *httptest.Server
}

func newTestIdP(t *testing.T) *testIdP {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithHeader("kid", "test-key").WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("create signer: %v", err)
	}
	jwksJSON, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       key.Public(),
		KeyID:     "test-key",
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}}})
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksJSON)
	}))
	t.Cleanup(srv.Close)

	return &testIdP{signer: signer, jwks: srv}
}

func (idp *testIdP) mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	sig, err := idp.signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	raw, err := sig.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize token: %v", err)
	}
	return raw
}

// baseClaims returns a valid claim set; tests mutate copies of it.
func baseClaims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":   testIssuer,
		"aud":   testAudience,
		"sub":   testSubject,
		"email": testEmail,
		"iat":   now.Add(-time.Minute).Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
}

func newTestHandler(t *testing.T, jwksURL string) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := testGatewayConfig(jwksURL)
	auth := newAuthenticator(t.Context(), cfg.OIDCIssuer, cfg.OIDCJWKSURL, cfg.OIDCAudience, logger)
	return newHandler(cfg, auth, newFakeDeps(t))
}

func doRequest(t *testing.T, h http.Handler, method, path string, header http.Header) (int, map[string]string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s %s: Content-Type = %q, want application/json", method, path, ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s %s: response is not a JSON object: %v (body: %q)", method, path, err, rec.Body.String())
	}
	return rec.Code, body
}

func TestAuthAndMe(t *testing.T) {
	idp := newTestIdP(t)
	handler := newTestHandler(t, idp.jwks.URL)

	mutate := func(fn func(map[string]any)) map[string]any {
		c := baseClaims()
		fn(c)
		return c
	}

	// forger holds a different RSA key but mints tokens with the same kid the
	// trusted JWKS serves, so rejection must come from signature verification,
	// not from a failed key lookup.
	forger := newTestIdP(t)

	// hs256Token signs the claims with a symmetric key (alg confusion probe).
	hmacSigner, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: []byte("0123456789abcdef0123456789abcdef")},
		(&jose.SignerOptions{}).WithHeader("kid", "test-key").WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("create HS256 signer: %v", err)
	}
	hs256Token := func(claims map[string]any) string {
		payload, err := json.Marshal(claims)
		if err != nil {
			t.Fatalf("marshal claims: %v", err)
		}
		sig, err := hmacSigner.Sign(payload)
		if err != nil {
			t.Fatalf("sign HS256 token: %v", err)
		}
		raw, err := sig.CompactSerialize()
		if err != nil {
			t.Fatalf("serialize HS256 token: %v", err)
		}
		return raw
	}

	// noneToken builds an unsigned JWT with header alg=none.
	noneToken := func(claims map[string]any) string {
		payload, err := json.Marshal(claims)
		if err != nil {
			t.Fatalf("marshal claims: %v", err)
		}
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
		return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
	}

	tests := []struct {
		name       string
		authHeader string
		extraHdr   http.Header
		wantStatus int
		wantBody   map[string]string // exact-match subset of response fields
	}{
		{
			name:       "valid token uses sub as tenant",
			authHeader: "Bearer " + idp.mint(t, baseClaims()),
			wantStatus: http.StatusOK,
			wantBody:   map[string]string{"tenant_id": testSubject, "subject": testSubject, "email": testEmail},
		},
		{
			name:       "tenant_id claim overrides sub",
			authHeader: "Bearer " + idp.mint(t, mutate(func(c map[string]any) { c["tenant_id"] = "tenant-42" })),
			wantStatus: http.StatusOK,
			wantBody:   map[string]string{"tenant_id": "tenant-42", "subject": testSubject},
		},
		{
			name:       "tenant never read from request headers",
			authHeader: "Bearer " + idp.mint(t, baseClaims()),
			extraHdr:   http.Header{"X-Tenant-Id": []string{"attacker-tenant"}},
			wantStatus: http.StatusOK,
			wantBody:   map[string]string{"tenant_id": testSubject},
		},
		{
			name:       "missing authorization header",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
			wantBody:   map[string]string{"error": "unauthorized"},
		},
		{
			name:       "wrong auth scheme",
			authHeader: "Basic dXNlcjpwYXNz",
			wantStatus: http.StatusUnauthorized,
			wantBody:   map[string]string{"error": "unauthorized"},
		},
		{
			name:       "bearer with empty token",
			authHeader: "Bearer   ",
			wantStatus: http.StatusUnauthorized,
			wantBody:   map[string]string{"error": "unauthorized"},
		},
		{
			name:       "garbage token",
			authHeader: "Bearer not-a-jwt",
			wantStatus: http.StatusUnauthorized,
			wantBody:   map[string]string{"error": "unauthorized"},
		},
		{
			name: "expired token",
			authHeader: "Bearer " + idp.mint(t, mutate(func(c map[string]any) {
				c["exp"] = time.Now().Add(-time.Hour).Unix()
			})),
			wantStatus: http.StatusUnauthorized,
			wantBody:   map[string]string{"error": "unauthorized"},
		},
		{
			name: "wrong issuer",
			authHeader: "Bearer " + idp.mint(t, mutate(func(c map[string]any) {
				c["iss"] = "http://evil.example.com/realms/asker"
			})),
			wantStatus: http.StatusUnauthorized,
			wantBody:   map[string]string{"error": "unauthorized"},
		},
		{
			name: "wrong audience",
			authHeader: "Bearer " + idp.mint(t, mutate(func(c map[string]any) {
				c["aud"] = "some-other-client"
			})),
			wantStatus: http.StatusUnauthorized,
			wantBody:   map[string]string{"error": "unauthorized"},
		},
		{
			name: "valid signature but no tenant claims",
			authHeader: "Bearer " + idp.mint(t, mutate(func(c map[string]any) {
				delete(c, "sub")
			})),
			wantStatus: http.StatusUnauthorized,
			wantBody:   map[string]string{"error": "unauthorized"},
		},
		{
			name:       "forged signature with trusted kid rejected",
			authHeader: "Bearer " + forger.mint(t, baseClaims()),
			wantStatus: http.StatusUnauthorized,
			wantBody:   map[string]string{"error": "unauthorized"},
		},
		{
			name:       "alg=none token rejected",
			authHeader: "Bearer " + noneToken(baseClaims()),
			wantStatus: http.StatusUnauthorized,
			wantBody:   map[string]string{"error": "unauthorized"},
		},
		{
			name:       "HS256 token rejected",
			authHeader: "Bearer " + hs256Token(baseClaims()),
			wantStatus: http.StatusUnauthorized,
			wantBody:   map[string]string{"error": "unauthorized"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hdr := http.Header{}
			if tc.authHeader != "" {
				hdr.Set("Authorization", tc.authHeader)
			}
			for k, vs := range tc.extraHdr {
				hdr[k] = vs
			}
			status, body := doRequest(t, handler, http.MethodGet, "/v1/me", hdr)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %v)", status, tc.wantStatus, body)
			}
			for k, want := range tc.wantBody {
				if got := body[k]; got != want {
					t.Errorf("body[%q] = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestHealthzNoAuth(t *testing.T) {
	idp := newTestIdP(t)
	handler := newTestHandler(t, idp.jwks.URL)

	status, body := doRequest(t, handler, http.MethodGet, "/healthz", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body["status"] != "ok" {
		t.Errorf("body = %v, want status ok", body)
	}
}

func TestReadyz(t *testing.T) {
	idp := newTestIdP(t)

	t.Run("200 when JWKS reachable", func(t *testing.T) {
		handler := newTestHandler(t, idp.jwks.URL)
		status, _ := doRequest(t, handler, http.MethodGet, "/readyz", nil)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
	})

	t.Run("503 when JWKS down", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		deadURL := dead.URL
		dead.Close() // port is now refused
		handler := newTestHandler(t, deadURL)
		status, _ := doRequest(t, handler, http.MethodGet, "/readyz", nil)
		if status != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", status)
		}
	})

	t.Run("503 when JWKS returns server error", func(t *testing.T) {
		failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(failing.Close)
		handler := newTestHandler(t, failing.URL)
		status, _ := doRequest(t, handler, http.MethodGet, "/readyz", nil)
		if status != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", status)
		}
	})
}

func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	idp := newTestIdP(t)
	handler := newTestHandler(t, idp.jwks.URL)

	status, body := doRequest(t, handler, http.MethodGet, "/no-such-route", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if body["error"] != "not found" {
		t.Errorf("body = %v, want error 'not found'", body)
	}

	status, body = doRequest(t, handler, http.MethodPost, "/healthz", nil)
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", status)
	}
	if body["error"] != "method not allowed" {
		t.Errorf("body = %v, want error 'method not allowed'", body)
	}

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+idp.mint(t, baseClaims()))
	status, _ = doRequest(t, handler, http.MethodDelete, "/v1/me", hdr)
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("authed DELETE /v1/me status = %d, want 405", status)
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("GATEWAY_ADDR", ":9999")
	t.Setenv("OIDC_ISSUER", "http://issuer.test/realms/x")
	t.Setenv("OIDC_JWKS_URL", "http://jwks.test/certs")
	t.Setenv("OIDC_AUDIENCE", "aud-x")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "otel:4317")
	t.Setenv("QUERY_GRPC_ADDR", "dns:///q.test:1")
	t.Setenv("CONTROL_PLANE_GRPC_ADDR", "dns:///cp.test:2")
	t.Setenv("HUB_HTTP_URL", "http://hub.test:3")
	t.Setenv("REDIS_ADDR", "redis.test:4")
	t.Setenv("RATE_LIMIT_PER_MINUTE", "42")
	t.Setenv("CORS_ALLOWED_ORIGINS", "http://a.test,http://b.test")
	t.Setenv("MAX_UPLOAD_MB", "7")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := gatewayConfig{
		Addr:                 ":9999",
		OIDCIssuer:           "http://issuer.test/realms/x",
		OIDCJWKSURL:          "http://jwks.test/certs",
		OIDCAudience:         "aud-x",
		OTLPEndpoint:         "otel:4317",
		QueryGRPCAddr:        "dns:///q.test:1",
		ControlPlaneGRPCAddr: "dns:///cp.test:2",
		HubHTTPURL:           "http://hub.test:3",
		RedisAddr:            "redis.test:4",
		RateLimitPerMinute:   42,
		CORSAllowedOrigins:   "http://a.test,http://b.test",
		MaxUploadMB:          7,
	}
	if cfg != want {
		t.Errorf("cfg = %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.QueryGRPCAddr != "dns:///query:9200" {
		t.Errorf("QueryGRPCAddr default = %q", cfg.QueryGRPCAddr)
	}
	if cfg.ControlPlaneGRPCAddr != "dns:///control-plane:9100" {
		t.Errorf("ControlPlaneGRPCAddr default = %q", cfg.ControlPlaneGRPCAddr)
	}
	if cfg.HubHTTPURL != "http://connector-hub:9300" {
		t.Errorf("HubHTTPURL default = %q", cfg.HubHTTPURL)
	}
	if cfg.RedisAddr != "redis:6379" {
		t.Errorf("RedisAddr default = %q", cfg.RedisAddr)
	}
	if cfg.RateLimitPerMinute != 600 {
		t.Errorf("RateLimitPerMinute default = %d", cfg.RateLimitPerMinute)
	}
	if cfg.CORSAllowedOrigins != "http://localhost:3000" {
		t.Errorf("CORSAllowedOrigins default = %q", cfg.CORSAllowedOrigins)
	}
	if cfg.MaxUploadMB != 32 {
		t.Errorf("MaxUploadMB default = %d", cfg.MaxUploadMB)
	}
}

func TestRunHealthcheck(t *testing.T) {
	idp := newTestIdP(t)
	srv := httptest.NewServer(newTestHandler(t, idp.jwks.URL))
	addr := srv.Listener.Addr().String()

	if code := runHealthcheck(addr); code != 0 {
		t.Fatalf("runHealthcheck against live server = %d, want 0", code)
	}
	srv.Close()
	if code := runHealthcheck(addr); code != 1 {
		t.Fatalf("runHealthcheck against closed server = %d, want 1", code)
	}
}

// TestStartupWithoutIdP confirms the authenticator construction performs no
// eager JWKS fetch: the gateway must start (and serve /healthz) while Keycloak
// is unreachable, and only token verification should fail.
func TestStartupWithoutIdP(t *testing.T) {
	handler := newTestHandler(t, "http://127.0.0.1:1/certs") // nothing listens here

	status, _ := doRequest(t, handler, http.MethodGet, "/healthz", nil)
	if status != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", status)
	}

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+newTestIdP(t).mint(t, baseClaims()))
	status, body := doRequest(t, handler, http.MethodGet, "/v1/me", hdr)
	if status != http.StatusUnauthorized {
		t.Fatalf("/v1/me status = %d, want 401 (body: %v)", status, body)
	}
}
