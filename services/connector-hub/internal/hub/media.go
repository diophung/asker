package hub

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/asker/asker/platform/blob"
	"github.com/asker/asker/platform/tenancy"
)

// maxMediaBytes bounds /internal/media bodies in BOTH directions (200 MiB, M3
// in-memory cap). platform/blob.Get/Put are []byte-based, so a fetch or store
// is fully buffered in the hub; this cap keeps a single media object from
// exhausting hub memory. Streaming (range reads, chunked decrypt) is an M4
// follow-up tracked with the mTLS hardening of this endpoint.
const maxMediaBytes = 200 << 20

// mediaTimeout bounds a single decrypt/encrypt + object-store round trip.
const mediaTimeout = 60 * time.Second

// defaultMediaContentType is returned when neither the request nor the stored
// BlobRef names a MIME type.
const defaultMediaContentType = "application/octet-stream"

// mediaBlobRef is the JSON shape returned by PUT /internal/media — the same
// fields the enrich worker writes into Document.media (thumbnail/keyframe
// BlobRef). Mirrors askerv1.BlobRef so the worker round-trips it without the
// proto.
type mediaBlobRef struct {
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	SizeBytes   int64  `json:"size_bytes"`
	ContentType string `json:"content_type"`
	Sha256      string `json:"sha256"`
}

// handleMediaGet returns the DECRYPTED bytes of a blob for the calling tenant
// (ADR-013). Two callers use it: the Python enrich worker fetches original
// media here (the envelope crypto stays in Go), and the gateway proxies the
// browser's GET /v1/media here to serve a search Hit's thumbnail/keyframe.
//
// Contract: GET /internal/media?key=<objectKey> with header x-asker-tenant:
// <tenant>. Only the object key is needed — the gateway/UI hold a
// thumbnail_key but never the plaintext sha256, so the read goes through
// blob.GetByKey, whose tenant-bound AEAD authenticates the ciphertext (a
// separate digest check would add nothing). The key MUST be within the
// tenant's "<tenant>/" prefix AND free of "."/".." traversal segments;
// GetByKey fails closed on either, so a different tenant's header — or a
// crafted key — can never read this object. GetByKey returns the object's
// stored Content-Type, which becomes the response Content-Type (octet-stream
// when the store has none).
//
// 401 missing/invalid tenant · 400 missing key · 404 absent or
// not-owned-by-tenant (the two are deliberately indistinguishable, so a probe
// cannot tell a foreign object from a missing one) · 503 when no blob store is
// wired · 200 with the decrypted body otherwise.
func (h *httpAPI) handleMediaGet(w http.ResponseWriter, r *http.Request) {
	tc, ok := h.tenantFromHeader(w, r)
	if !ok {
		return
	}
	if h.mediaBlobs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "media store unavailable"})
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query parameter \"key\" is required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mediaTimeout)
	defer cancel()
	tctx := tenancy.WithContext(ctx, tc)

	data, contentType, err := h.mediaBlobs.GetByKey(tctx, tc, key)
	if err != nil {
		// ErrNotFound AND ErrTenantMismatch both collapse to 404: a foreign
		// tenant's header (or a crafted/traversal key, which GetByKey reports
		// as ErrTenantMismatch) must not be able to distinguish "exists but
		// not yours" from "absent" (the sacred no-cross-tenant-read property).
		if errors.Is(err, blob.ErrNotFound) || errors.Is(err, blob.ErrTenantMismatch) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		if errors.Is(err, tenancy.ErrNoTenant) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid tenant"})
			return
		}
		// Tenant is never logged as content; key is an internal handle, safe.
		h.logger.ErrorContext(tctx, "media get failed", "tenant", tc.TenantID(), "key", key, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "media fetch failed"})
		return
	}

	if contentType == "" {
		contentType = defaultMediaContentType
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	// A write error means the client went away mid-stream; nothing to recover.
	_, _ = w.Write(data)
}

// handleMediaPut stores request-body bytes ENCRYPTED under the calling
// tenant's DEK and returns the resulting BlobRef as JSON (ADR-013). This is
// how the enrich worker persists thumbnails and keyframes; the worker writes
// the returned BlobRef into Document.media.
//
// Contract: PUT /internal/media?key=<objectKey>[&content_type=<mime>] with
// header x-asker-tenant: <tenant> and the raw bytes as the body. blob.Put
// prefixes the key with "<tenant>/", so a tenant can only ever write into its
// own namespace.
//
// 401 missing/invalid tenant · 400 missing key · 413 body over the cap · 503
// no store · 201 with the BlobRef JSON otherwise.
func (h *httpAPI) handleMediaPut(w http.ResponseWriter, r *http.Request) {
	tc, ok := h.tenantFromHeader(w, r)
	if !ok {
		return
	}
	if h.mediaBlobs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "media store unavailable"})
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query parameter \"key\" is required"})
		return
	}
	contentType := r.URL.Query().Get("content_type")
	if contentType == "" {
		contentType = defaultMediaContentType
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxMediaBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "media body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read media body"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mediaTimeout)
	defer cancel()
	tctx := tenancy.WithContext(ctx, tc)

	ref, err := h.mediaBlobs.Put(tctx, tc, key, contentType, data)
	if err != nil {
		h.logger.ErrorContext(tctx, "media put failed", "tenant", tc.TenantID(), "key", key, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "media store failed"})
		return
	}

	writeJSON(w, http.StatusCreated, mediaBlobRef{
		Bucket:      ref.GetBucket(),
		Key:         ref.GetKey(),
		SizeBytes:   ref.GetSizeBytes(),
		ContentType: ref.GetContentType(),
		Sha256:      ref.GetSha256(),
	})
}
