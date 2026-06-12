// Package blob is the MinIO/S3-backed encrypted blob store for document
// originals. Objects live under the key "<tenant_id>/<key>" inside a single
// bucket and every payload is envelope-encrypted with the tenant's DEK
// (platform/crypto) BEFORE it reaches the object store, so the store only
// ever holds ciphertext. Reads fail closed: a Get whose BlobRef key is not
// prefixed with the caller's tenant is rejected before any network I/O, the
// tenant-bound AEAD refuses cross-tenant ciphertext, and the decrypted
// plaintext must match the BlobRef's sha256.
//
// The wire transport is a minimal S3 SigV4 client over net/http (s3client.go)
// because the frozen go.mod does not carry minio-go/v7; the unexported
// objectAPI seam is exactly the three calls a minio-go client would provide
// (MakeBucket/BucketExists, PutObject, GetObject), so swapping the transport
// later does not touch Store semantics.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/asker/asker/platform/crypto"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// defaultBucket is used when Config.Bucket is empty.
const defaultBucket = "asker-blobs"

var (
	// ErrNotFound is returned by Get when the referenced object does not
	// exist in the bucket.
	ErrNotFound = errors.New("blob: object not found")
	// ErrTenantMismatch is returned by Get when the BlobRef's object key is
	// not under the calling tenant's prefix. Cross-tenant reads never reach
	// the object store.
	ErrTenantMismatch = errors.New("blob: blob ref does not belong to the tenant")
	// ErrChecksumMismatch is returned by Get when the decrypted plaintext
	// does not hash to the BlobRef's sha256.
	ErrChecksumMismatch = errors.New("blob: plaintext sha256 does not match blob ref")
)

// Config is the blob store configuration, loaded from the environment via
// platform/config (no prefix).
type Config struct {
	// Endpoint is the object store host:port (no scheme). A http:// or
	// https:// prefix is tolerated and overrides UseSSL.
	Endpoint string `env:"MINIO_ENDPOINT" envDefault:"minio:9000"`
	// AccessKey authenticates to the object store.
	AccessKey string `env:"MINIO_ACCESS_KEY"`
	// SecretKey authenticates to the object store.
	SecretKey string `env:"MINIO_SECRET_KEY"`
	// Bucket is the single bucket holding all tenants' blobs.
	Bucket string `env:"BLOB_BUCKET" envDefault:"asker-blobs"`
	// UseSSL selects https for the object store connection.
	UseSSL bool `env:"MINIO_USE_SSL" envDefault:"false"`
}

// objectAPI is the seam between Store semantics and the object-store wire
// client: the three calls Store needs (mirroring minio-go's
// BucketExists/MakeBucket, PutObject, and GetObject). Tests fake it; the
// real implementation is s3Client.
type objectAPI interface {
	// ensureBucket makes sure bucket exists, creating it when missing.
	// It is idempotent and safe to race.
	ensureBucket(ctx context.Context, bucket string) error
	// putObject stores data (already ciphertext) under bucket/key.
	putObject(ctx context.Context, bucket, key, contentType string, data []byte) error
	// getObject fetches bucket/key, returning ErrNotFound (possibly
	// wrapped) when the object does not exist.
	getObject(ctx context.Context, bucket, key string) ([]byte, error)
}

// Store is a tenant-encrypted blob store over one S3-compatible bucket.
// It is safe for concurrent use.
type Store struct {
	api    objectAPI
	bucket string
	cipher *crypto.TenantCipher
}

// New connects to the object store described by cfg, ensures cfg.Bucket
// exists (idempotent), and returns a Store that encrypts every payload with
// cipher before storage.
func New(ctx context.Context, cfg Config, cipher *crypto.TenantCipher) (*Store, error) {
	endpoint, useSSL := cfg.Endpoint, cfg.UseSSL
	if rest, ok := strings.CutPrefix(endpoint, "http://"); ok {
		endpoint, useSSL = rest, false
	} else if rest, ok := strings.CutPrefix(endpoint, "https://"); ok {
		endpoint, useSSL = rest, true
	}
	if endpoint == "" {
		return nil, errors.New("blob: endpoint is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("blob: access key and secret key are required")
	}
	return newStore(ctx, cfg.Bucket, cipher, newS3Client(endpoint, cfg.AccessKey, cfg.SecretKey, useSSL))
}

// newStore wires a Store over any objectAPI, ensuring the bucket exists.
func newStore(ctx context.Context, bucket string, cipher *crypto.TenantCipher, api objectAPI) (*Store, error) {
	if cipher == nil {
		return nil, errors.New("blob: nil tenant cipher")
	}
	if bucket == "" {
		bucket = defaultBucket
	}
	if err := api.ensureBucket(ctx, bucket); err != nil {
		return nil, fmt.Errorf("blob: ensure bucket %q: %w", bucket, err)
	}
	slog.DebugContext(ctx, "blob store ready", "bucket", bucket)
	return &Store{api: api, bucket: bucket, cipher: cipher}, nil
}

// Put encrypts data for the tenant in tc and stores it under
// "<tenant_id>/<key>". The returned BlobRef describes the PLAINTEXT
// (size_bytes and sha256 are computed before encryption) and carries the
// full object key, so Get can enforce tenant ownership from the ref alone.
func (s *Store) Put(ctx context.Context, tc tenancy.Context, key string, contentType string, data []byte) (*askerv1.BlobRef, error) {
	if tc.TenantID() == "" {
		return nil, fmt.Errorf("blob: put: %w", tenancy.ErrNoTenant)
	}
	if !validKey(key) {
		return nil, fmt.Errorf("blob: put: invalid object key %q", key)
	}
	objectKey := string(tc.TenantID()) + "/" + key
	sum := sha256.Sum256(data)

	ciphertext, err := s.cipher.Encrypt(ctx, tc, data)
	if err != nil {
		return nil, fmt.Errorf("blob: put %q: %w", objectKey, err)
	}
	if err := s.api.putObject(ctx, s.bucket, objectKey, contentType, ciphertext); err != nil {
		return nil, fmt.Errorf("blob: put %q: %w", objectKey, err)
	}
	return &askerv1.BlobRef{
		Bucket:      s.bucket,
		Key:         objectKey,
		SizeBytes:   int64(len(data)),
		ContentType: contentType,
		Sha256:      hex.EncodeToString(sum[:]),
	}, nil
}

// Get fetches and decrypts the blob behind ref for the tenant in tc. It
// fails closed before any I/O when ref's key is not under tc's tenant
// prefix or ref points at a different bucket, and it verifies the decrypted
// plaintext against ref's sha256.
func (s *Store) Get(ctx context.Context, tc tenancy.Context, ref *askerv1.BlobRef) ([]byte, error) {
	if tc.TenantID() == "" {
		return nil, fmt.Errorf("blob: get: %w", tenancy.ErrNoTenant)
	}
	if ref == nil || ref.GetKey() == "" {
		return nil, errors.New("blob: get: nil or empty blob ref")
	}
	if ref.GetBucket() != "" && ref.GetBucket() != s.bucket {
		return nil, fmt.Errorf("blob: get: ref bucket %q does not match store bucket %q", ref.GetBucket(), s.bucket)
	}
	if ref.GetSha256() == "" {
		return nil, errors.New("blob: get: blob ref carries no sha256")
	}
	if prefix := string(tc.TenantID()) + "/"; !strings.HasPrefix(ref.GetKey(), prefix) {
		return nil, fmt.Errorf("blob: get %q for tenant %q: %w", ref.GetKey(), tc.TenantID(), ErrTenantMismatch)
	}

	ciphertext, err := s.api.getObject(ctx, s.bucket, ref.GetKey())
	if err != nil {
		return nil, fmt.Errorf("blob: get %q: %w", ref.GetKey(), err)
	}
	plaintext, err := s.cipher.Decrypt(ctx, tc, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("blob: get %q: %w", ref.GetKey(), err)
	}
	sum := sha256.Sum256(plaintext)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), ref.GetSha256()) {
		return nil, fmt.Errorf("blob: get %q: %w", ref.GetKey(), ErrChecksumMismatch)
	}
	return plaintext, nil
}

// validKey accepts non-empty, relative, slash-separated keys with no empty,
// "." or ".." segments — keys become URL path segments, so anything that
// could re-anchor or traverse the path is rejected.
func validKey(key string) bool {
	if key == "" {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
