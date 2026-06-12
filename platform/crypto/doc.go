// Package crypto implements per-tenant envelope encryption for Asker.
//
// # Envelope model
//
// Every tenant has exactly one Data Encryption Key (DEK): a random 32-byte
// AES-256 key. Tenant data (blobs in MinIO, OAuth tokens in Postgres) is
// encrypted with the tenant's DEK using AES-256-GCM. The DEK itself is never
// persisted in plaintext: it is wrapped (encrypted) by a Key Encryption Key
// (KEK) via a KEKProvider, and only the wrapped form is stored in a DEKStore.
//
//	plaintext --AES-256-GCM(DEK)--> data ciphertext   (stored with the data)
//	DEK       --AES-256-GCM(KEK)--> wrapped DEK       (stored in DEKStore)
//
// The tenant ID is bound as GCM additional authenticated data (AAD) at BOTH
// layers. A data ciphertext copied into another tenant's namespace, or a
// wrapped DEK moved across tenant rows, fails authentication on decrypt —
// cross-tenant blob movement is cryptographically detected, not just
// access-controlled.
//
// # Wire formats
//
// Both stored blob kinds carry a 1-byte version prefix so formats can evolve:
//
//	data ciphertext: 0x01 || 12-byte nonce || GCM(DEK, plaintext, AAD=tenantID)
//	wrapped DEK:     0x01 || 12-byte nonce || GCM(KEK, DEK,       AAD=tenantID)
//
// Decryption rejects unknown version bytes outright.
//
// # TenantCipher and the first-encrypt race
//
// TenantCipher lazily provisions a tenant's DEK on first Encrypt: generate,
// wrap, persist via DEKStore.PutWrappedDEK. PutWrappedDEK is defined as
// FIRST-WRITER-WINS and IDEMPOTENT: when a wrapped DEK already exists for the
// tenant, implementations keep the existing value (returning nil or an error;
// either is tolerated). TenantCipher always re-reads the store after a Put and
// uses whatever is stored, so concurrent first-encrypts for the same tenant
// converge on a single DEK regardless of which goroutine or process won.
// Unwrapped DEKs are cached in memory after first use; Decrypt never creates
// a DEK and returns ErrDEKNotFound for an unprovisioned tenant.
//
// # Dev shim vs. production (M4 Vault path)
//
// NewFileKEK is the development KEK: a single 32-byte key stored at a local
// path (created atomically with 0600 permissions; group/world-accessible key
// files are refused). In M4 it is replaced by a HashiCorp Vault transit-backed
// implementation of the SAME KEKProvider interface — wrap/unwrap calls move
// server-side and the KEK never touches service memory, while callers and the
// stored wrapped-DEK lifecycle are unchanged. The control plane persists
// wrapped DEKs in Postgres behind the DEKStore interface; NewMemDEKStore is
// for tests.
package crypto
