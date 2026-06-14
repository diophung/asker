package crypto

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/asker/asker/platform/tenancy"
)

// fakeTransit emulates the subset of the Vault Transit engine vaultKEK uses:
// POST /v1/transit/encrypt/<key> and /v1/transit/decrypt/<key>, with derived
// (convergent) keys keyed by the request "context". A ciphertext records the
// context it was produced under; decrypt only succeeds when the supplied
// context matches — the same per-tenant guarantee a real derived Transit key
// gives, and the analog of the fileKEK AAD binding.
type fakeTransit struct {
	t       *testing.T
	token   string
	keyName string

	mu          sync.Mutex
	encryptReqs int
	decryptReqs int
	lastToken   string
}

func newFakeTransit(t *testing.T) *fakeTransit {
	t.Helper()
	return &fakeTransit{t: t, token: "test-token", keyName: "asker-kek"}
}

func (f *fakeTransit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastToken = r.Header.Get("X-Vault-Token")
	f.mu.Unlock()

	if r.Method != http.MethodPost {
		f.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.writeErr(w, http.StatusBadRequest, "read body")
		return
	}
	var req map[string]string
	if err := json.Unmarshal(body, &req); err != nil {
		f.writeErr(w, http.StatusBadRequest, "bad json")
		return
	}

	switch r.URL.Path {
	case "/v1/transit/encrypt/" + f.keyName:
		f.mu.Lock()
		f.encryptReqs++
		f.mu.Unlock()
		f.encrypt(w, req)
	case "/v1/transit/decrypt/" + f.keyName:
		f.mu.Lock()
		f.decryptReqs++
		f.mu.Unlock()
		f.decrypt(w, req)
	default:
		f.writeErr(w, http.StatusNotFound, "no handler for "+r.URL.Path)
	}
}

// encrypt mirrors Transit encrypt: it base64-decodes plaintext, then returns a
// "vault:v1:<token>" ciphertext that embeds both the context and the plaintext
// so the fake can verify the context on decrypt.
func (f *fakeTransit) encrypt(w http.ResponseWriter, req map[string]string) {
	pt, err := base64.StdEncoding.DecodeString(req["plaintext"])
	if err != nil {
		f.writeErr(w, http.StatusBadRequest, "invalid plaintext base64")
		return
	}
	// ctx||"|"||plaintext, all base64; the fake's "derived key" is just the ctx.
	inner := req["context"] + "|" + base64.StdEncoding.EncodeToString(pt)
	ct := "vault:v1:" + base64.StdEncoding.EncodeToString([]byte(inner))
	f.writeJSON(w, map[string]any{"data": map[string]string{"ciphertext": ct}})
}

// decrypt mirrors Transit decrypt with derived keys: it requires the SAME
// context the ciphertext was produced under, else fails with Vault's 400 the
// way a real derived-key context mismatch does.
func (f *fakeTransit) decrypt(w http.ResponseWriter, req map[string]string) {
	ct := req["ciphertext"]
	raw, ok := strings.CutPrefix(ct, "vault:v1:")
	if !ok {
		f.writeErr(w, http.StatusBadRequest, "invalid ciphertext prefix")
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		f.writeErr(w, http.StatusBadRequest, "invalid ciphertext base64")
		return
	}
	gotCtx, ptB64, ok := strings.Cut(string(decoded), "|")
	if !ok {
		f.writeErr(w, http.StatusBadRequest, "malformed ciphertext")
		return
	}
	if gotCtx != req["context"] {
		// Real Vault: "unable to decrypt: ... wrong context for derived key".
		f.writeErr(w, http.StatusBadRequest, "unable to decrypt ciphertext: wrong context for derived key")
		return
	}
	f.writeJSON(w, map[string]any{"data": map[string]string{"plaintext": ptB64}})
}

func (f *fakeTransit) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("fakeTransit: encode response: %v", err)
	}
}

func (f *fakeTransit) writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(map[string][]string{"errors": {msg}}); err != nil {
		f.t.Errorf("fakeTransit: encode error: %v", err)
	}
}

// newTestVaultKEK starts a fake Transit server and returns a vaultKEK pointed at
// it plus the fake (for assertions). The server is torn down at test end.
func newTestVaultKEK(t *testing.T) (KEKProvider, *fakeTransit) {
	t.Helper()
	fake := newFakeTransit(t)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	kek, err := NewVaultKEK(VaultConfig{
		Addr:       srv.URL,
		Token:      fake.token,
		KeyName:    fake.keyName,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewVaultKEK: %v", err)
	}
	return kek, fake
}

func TestNewVaultKEKValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  VaultConfig
	}{
		{"empty addr", VaultConfig{Token: "t", KeyName: "k"}},
		{"empty token", VaultConfig{Addr: "http://vault:8200", KeyName: "k"}},
		{"empty keyName", VaultConfig{Addr: "http://vault:8200", Token: "t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewVaultKEK(tc.cfg); err == nil {
				t.Fatalf("NewVaultKEK(%+v) = nil error, want error", tc.cfg)
			}
		})
	}
}

func TestNewVaultKEKDefaultsHTTPClient(t *testing.T) {
	kek, err := NewVaultKEK(VaultConfig{Addr: "http://vault:8200/", Token: "t", KeyName: "k"})
	if err != nil {
		t.Fatalf("NewVaultKEK: %v", err)
	}
	vk, ok := kek.(*vaultKEK)
	if !ok {
		t.Fatalf("NewVaultKEK returned %T, want *vaultKEK", kek)
	}
	if vk.client == nil {
		t.Fatal("default HTTP client is nil")
	}
	// Trailing slash in Addr must not double up in the path.
	if !strings.HasSuffix(vk.encryptURL, "/v1/transit/encrypt/k") {
		t.Errorf("encryptURL = %q, want suffix /v1/transit/encrypt/k", vk.encryptURL)
	}
}

func TestVaultKEKWrapUnwrapRoundTrip(t *testing.T) {
	kek, fake := newTestVaultKEK(t)
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")
	dek := newDEK(t)

	wrapped, err := kek.WrapDEK(ctx, tid, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if bytes.Contains(wrapped, dek) {
		t.Fatal("wrapped blob contains the plaintext DEK")
	}
	if !bytes.HasPrefix(wrapped, []byte("vault:v1:")) {
		t.Fatalf("wrapped blob = %q, want Vault ciphertext prefix", wrapped)
	}
	got, err := kek.UnwrapDEK(ctx, tid, wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Error("round-tripped DEK differs")
	}
	if fake.encryptReqs != 1 || fake.decryptReqs != 1 {
		t.Errorf("request counts: encrypt=%d decrypt=%d, want 1/1", fake.encryptReqs, fake.decryptReqs)
	}
}

func TestVaultKEKSendsToken(t *testing.T) {
	kek, fake := newTestVaultKEK(t)
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")

	if _, err := kek.WrapDEK(ctx, tid, newDEK(t)); err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if fake.lastToken != fake.token {
		t.Errorf("X-Vault-Token header = %q, want %q", fake.lastToken, fake.token)
	}
}

// TestVaultKEKSendsTenantContext asserts the tenant ID is sent as the Transit
// context (base64), so per-tenant derived keys are used.
func TestVaultKEKSendsTenantContext(t *testing.T) {
	var gotEncCtx string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotEncCtx = req["context"]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"ciphertext": "vault:v1:abc"},
		})
	}))
	t.Cleanup(srv.Close)
	kek, err := NewVaultKEK(VaultConfig{Addr: srv.URL, Token: "t", KeyName: "asker-kek", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewVaultKEK: %v", err)
	}

	tid := testTenantID(t, "tenant-a")
	if _, err := kek.WrapDEK(context.Background(), tid, newDEK(t)); err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	want := base64.StdEncoding.EncodeToString([]byte(tid))
	if gotEncCtx != want {
		t.Errorf("Transit context = %q, want base64(tenant) %q", gotEncCtx, want)
	}
}

// TestVaultKEKUnwrapWrongTenantFails is the Vault analog of the fileKEK
// cross-tenant test: a DEK wrapped under tenant A's context cannot be unwrapped
// under tenant B (derived-key context mismatch).
func TestVaultKEKUnwrapWrongTenantFails(t *testing.T) {
	kek, _ := newTestVaultKEK(t)
	ctx := context.Background()

	wrapped, err := kek.WrapDEK(ctx, testTenantID(t, "tenant-a"), newDEK(t))
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if _, err := kek.UnwrapDEK(ctx, testTenantID(t, "tenant-b"), wrapped); err == nil {
		t.Error("unwrap under a different tenant succeeded, want failure")
	}
}

func TestVaultKEKHTTPErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string][]string{"errors": {"permission denied"}})
	}))
	t.Cleanup(srv.Close)
	kek, err := NewVaultKEK(VaultConfig{Addr: srv.URL, Token: "bad", KeyName: "asker-kek", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewVaultKEK: %v", err)
	}
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")

	_, werr := kek.WrapDEK(ctx, tid, newDEK(t))
	if werr == nil {
		t.Fatal("WrapDEK against a 403 endpoint succeeded, want error")
	}
	if !strings.Contains(werr.Error(), "permission denied") {
		t.Errorf("WrapDEK error %q does not surface the Vault error", werr)
	}
	if _, uerr := kek.UnwrapDEK(ctx, tid, []byte("vault:v1:abc")); uerr == nil {
		t.Error("UnwrapDEK against a 403 endpoint succeeded, want error")
	}
}

func TestVaultKEKEmptyCiphertextInResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"ciphertext": ""}})
	}))
	t.Cleanup(srv.Close)
	kek, err := NewVaultKEK(VaultConfig{Addr: srv.URL, Token: "t", KeyName: "k", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewVaultKEK: %v", err)
	}
	if _, err := kek.WrapDEK(context.Background(), testTenantID(t, "tenant-a"), newDEK(t)); err == nil {
		t.Error("WrapDEK accepted an empty ciphertext response, want error")
	}
}

func TestVaultKEKUnwrapBadBase64Plaintext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"plaintext": "not!base64!"}})
	}))
	t.Cleanup(srv.Close)
	kek, err := NewVaultKEK(VaultConfig{Addr: srv.URL, Token: "t", KeyName: "k", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewVaultKEK: %v", err)
	}
	if _, err := kek.UnwrapDEK(context.Background(), testTenantID(t, "tenant-a"), []byte("vault:v1:abc")); err == nil {
		t.Error("UnwrapDEK accepted non-base64 plaintext, want error")
	}
}

func TestVaultKEKUnwrapWrongKeySize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 16 bytes base64, not the 32-byte DEK size.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"plaintext": base64.StdEncoding.EncodeToString(make([]byte, 16))},
		})
	}))
	t.Cleanup(srv.Close)
	kek, err := NewVaultKEK(VaultConfig{Addr: srv.URL, Token: "t", KeyName: "k", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewVaultKEK: %v", err)
	}
	if _, err := kek.UnwrapDEK(context.Background(), testTenantID(t, "tenant-a"), []byte("vault:v1:abc")); err == nil {
		t.Error("UnwrapDEK accepted a 16-byte key, want error")
	}
}

func TestVaultKEKInputValidation(t *testing.T) {
	kek, _ := newTestVaultKEK(t)
	ctx := context.Background()
	tid := testTenantID(t, "tenant-a")

	if _, err := kek.WrapDEK(ctx, "", newDEK(t)); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("WrapDEK with empty tenant: err = %v, want ErrNoTenant", err)
	}
	if _, err := kek.WrapDEK(ctx, tid, make([]byte, 16)); err == nil {
		t.Error("WrapDEK accepted a 16-byte DEK")
	}
	if _, err := kek.UnwrapDEK(ctx, "", []byte("vault:v1:abc")); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("UnwrapDEK with empty tenant: err = %v, want ErrNoTenant", err)
	}
	if _, err := kek.UnwrapDEK(ctx, tid, nil); err == nil {
		t.Error("UnwrapDEK accepted an empty blob")
	}
}

func TestVaultKEKContextCanceled(t *testing.T) {
	kek, _ := newTestVaultKEK(t)
	tid := testTenantID(t, "tenant-a")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := kek.WrapDEK(canceled, tid, newDEK(t)); !errors.Is(err, context.Canceled) {
		t.Errorf("WrapDEK with canceled ctx: err = %v, want context.Canceled", err)
	}
	if _, err := kek.UnwrapDEK(canceled, tid, []byte("vault:v1:abc")); !errors.Is(err, context.Canceled) {
		t.Errorf("UnwrapDEK with canceled ctx: err = %v, want context.Canceled", err)
	}
}

// TestVaultKEKErrorBodyFallbacks covers vaultErrors when Vault returns a non-2xx
// status with a body that is NOT the {"errors":[...]} shape (plain text and
// empty), exercising the fallback message paths.
func TestVaultKEKErrorBodyFallbacks(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"plain text body", "Service Unavailable", "Service Unavailable"},
		{"empty body", "", "(no body)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			kek, err := NewVaultKEK(VaultConfig{Addr: srv.URL, Token: "t", KeyName: "k", HTTPClient: srv.Client()})
			if err != nil {
				t.Fatalf("NewVaultKEK: %v", err)
			}
			_, werr := kek.WrapDEK(context.Background(), testTenantID(t, "tenant-a"), newDEK(t))
			if werr == nil {
				t.Fatal("WrapDEK against a 500 endpoint succeeded, want error")
			}
			if !strings.Contains(werr.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", werr, tc.want)
			}
		})
	}
}

// TestVaultKEKDecodeResponseError covers the body-decode failure path: a 2xx
// response whose body is not valid JSON.
func TestVaultKEKDecodeResponseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{not json")
	}))
	t.Cleanup(srv.Close)
	kek, err := NewVaultKEK(VaultConfig{Addr: srv.URL, Token: "t", KeyName: "k", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewVaultKEK: %v", err)
	}
	if _, err := kek.WrapDEK(context.Background(), testTenantID(t, "tenant-a"), newDEK(t)); err == nil {
		t.Error("WrapDEK accepted an undecodable 2xx response, want error")
	}
}

// TestVaultKEKConnectionError covers the transport-level failure path (no
// server listening).
func TestVaultKEKConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close() // nothing listening now

	kek, err := NewVaultKEK(VaultConfig{Addr: addr, Token: "t", KeyName: "k"})
	if err != nil {
		t.Fatalf("NewVaultKEK: %v", err)
	}
	if _, err := kek.WrapDEK(context.Background(), testTenantID(t, "tenant-a"), newDEK(t)); err == nil {
		t.Error("WrapDEK against a dead server succeeded, want error")
	}
}

// TestVaultKEKRoundTripThroughTenantCipher exercises the vaultKEK behind the
// TenantCipher envelope flow, confirming the same interface drops in for FileKEK.
func TestVaultKEKRoundTripThroughTenantCipher(t *testing.T) {
	kek, _ := newTestVaultKEK(t)
	cipher := NewTenantCipher(kek, NewMemDEKStore())
	ctx := context.Background()
	tc := testTenant(t, "tenant-a")

	plaintext := []byte("secret OAuth refresh token")
	ct, err := cipher.Encrypt(ctx, tc, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := cipher.Decrypt(ctx, tc, ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Error("round-tripped plaintext differs through vault-backed TenantCipher")
	}
}
