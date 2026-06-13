package crypto

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/asker/asker/platform/tenancy"
)

// maxVaultRespBody caps how much of a Vault HTTP response we read, so a
// misbehaving or hostile endpoint cannot exhaust memory. Transit encrypt/decrypt
// replies are small JSON objects.
const maxVaultRespBody = 1 << 20 // 1 MiB

// VaultConfig configures a Vault Transit-backed KEKProvider.
//
// Addr is the Vault server base URL (e.g. "https://vault:8200"); the scheme and
// host are used verbatim. Token is the Vault token sent in the X-Vault-Token
// header on every request. KeyName is the name of the Transit key used to wrap
// DEKs; it MUST be created as a derived (convergent) key (derived=true) so the
// per-tenant context produces a tenant-scoped wrapping. HTTPClient is optional;
// when nil a client with a sane timeout is used.
type VaultConfig struct {
	Addr       string
	Token      string
	KeyName    string
	HTTPClient *http.Client
}

// vaultKEK is the production KEKProvider: it wraps and unwraps tenant DEKs via a
// HashiCorp Vault Transit engine over the Vault HTTP API. The KEK itself never
// leaves Vault — only wrap/unwrap calls cross the wire, so service memory never
// holds the key (unlike fileKEK).
//
// Tenant binding: the tenant ID is sent as the Transit "context" (base64) on
// both encrypt and decrypt. With a derived Transit key, Vault derives a distinct
// per-context key, so a DEK wrapped for tenant A cannot be unwrapped under
// tenant B — the same cross-tenant guarantee fileKEK gets from binding the
// tenant ID as GCM AAD (see doc.go).
type vaultKEK struct {
	client     *http.Client
	encryptURL string
	decryptURL string
	token      string
}

// NewVaultKEK returns a KEKProvider backed by the Vault Transit engine at
// cfg.Addr, wrapping DEKs with the Transit key cfg.KeyName. The Transit key must
// already exist in Vault and be configured as derived (convergent) so the
// per-tenant context yields per-tenant key derivation. Addr/Token/KeyName are
// required.
func NewVaultKEK(cfg VaultConfig) (KEKProvider, error) {
	addr := strings.TrimRight(strings.TrimSpace(cfg.Addr), "/")
	if addr == "" {
		return nil, errors.New("crypto: vault KEK: empty Addr")
	}
	if _, err := url.Parse(addr); err != nil {
		return nil, fmt.Errorf("crypto: vault KEK: invalid Addr: %w", err)
	}
	if cfg.Token == "" {
		return nil, errors.New("crypto: vault KEK: empty Token")
	}
	if cfg.KeyName == "" {
		return nil, errors.New("crypto: vault KEK: empty KeyName")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	keySeg := url.PathEscape(cfg.KeyName)
	return &vaultKEK{
		client:     client,
		encryptURL: addr + "/v1/transit/encrypt/" + keySeg,
		decryptURL: addr + "/v1/transit/decrypt/" + keySeg,
		token:      cfg.Token,
	}, nil
}

// WrapDEK encrypts a 32-byte DEK with the Transit key, binding the tenant ID as
// the Transit context. The returned blob is the opaque Vault ciphertext token
// ("vault:v1:...") as bytes; UnwrapDEK reverses it. Vault must be reachable.
func (k *vaultKEK) WrapDEK(ctx context.Context, tenantID tenancy.TenantID, dek []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tenantID == "" {
		return nil, fmt.Errorf("crypto: vault wrap DEK: %w", tenancy.ErrNoTenant)
	}
	if len(dek) != dekSize {
		return nil, fmt.Errorf("crypto: vault wrap DEK: key must be %d bytes, got %d", dekSize, len(dek))
	}
	reqBody := map[string]string{
		"plaintext": base64.StdEncoding.EncodeToString(dek),
		"context":   tenantContext(tenantID),
	}
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := k.do(ctx, k.encryptURL, reqBody, &out); err != nil {
		return nil, fmt.Errorf("crypto: vault wrap DEK for tenant %q: %w", tenantID, err)
	}
	if out.Data.Ciphertext == "" {
		return nil, fmt.Errorf("crypto: vault wrap DEK for tenant %q: empty ciphertext in response", tenantID)
	}
	return []byte(out.Data.Ciphertext), nil
}

// UnwrapDEK decrypts a wrapped DEK produced by WrapDEK for the same tenant. A
// blob wrapped for a different tenant fails because the Transit context differs
// (derived key mismatch), mirroring the fileKEK AAD check. Vault must be
// reachable.
func (k *vaultKEK) UnwrapDEK(ctx context.Context, tenantID tenancy.TenantID, wrapped []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tenantID == "" {
		return nil, fmt.Errorf("crypto: vault unwrap DEK: %w", tenancy.ErrNoTenant)
	}
	if len(wrapped) == 0 {
		return nil, errors.New("crypto: vault unwrap DEK: empty blob")
	}
	reqBody := map[string]string{
		"ciphertext": string(wrapped),
		"context":    tenantContext(tenantID),
	}
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := k.do(ctx, k.decryptURL, reqBody, &out); err != nil {
		return nil, fmt.Errorf("crypto: vault unwrap DEK for tenant %q: %w", tenantID, err)
	}
	dek, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("crypto: vault unwrap DEK for tenant %q: decode plaintext: %w", tenantID, err)
	}
	if len(dek) != dekSize {
		return nil, fmt.Errorf("crypto: vault unwrap DEK for tenant %q: unexpected key size %d", tenantID, len(dek))
	}
	return dek, nil
}

// tenantContext is the per-tenant Transit "context" (base64-encoded tenant ID)
// used for derived-key wrapping. It is the Vault analog of the fileKEK GCM
// AAD: the same tenant string binds the wrapping to the tenant.
func tenantContext(tenantID tenancy.TenantID) string {
	return base64.StdEncoding.EncodeToString([]byte(tenantID))
}

// do POSTs body as JSON to urlStr with the Vault token header and decodes a
// successful (2xx) JSON response into out. Non-2xx responses (including Vault's
// 403 on a cross-tenant/derived-key mismatch and 400 on a bad ciphertext) are
// turned into errors carrying Vault's reported error strings.
func (k *vaultKEK) do(ctx context.Context, urlStr string, body map[string]string, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-Vault-Token", k.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("vault request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxVaultRespBody))
	if err != nil {
		return fmt.Errorf("read vault response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("vault returned HTTP %d: %s", resp.StatusCode, vaultErrors(respBody))
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode vault response: %w", err)
	}
	return nil
}

// vaultErrors extracts the "errors" array Vault returns on failure, falling back
// to a trimmed copy of the raw body. The result is purely for error messages.
func vaultErrors(body []byte) string {
	var v struct {
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(body, &v); err == nil && len(v.Errors) > 0 {
		return strings.Join(v.Errors, "; ")
	}
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "(no body)"
	}
	return s
}
