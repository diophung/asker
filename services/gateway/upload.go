package main

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/asker/asker/platform/tenancy"
	"github.com/asker/asker/platform/tenancy/tenancygrpc"
)

// hubTenantHeader is the internal HTTP hop tenant header (same wire name and
// value syntax as the gRPC metadata key; the hub re-validates it with
// tenancy.FromHeaderValue and fails closed).
const hubTenantHeader = tenancygrpc.MetadataKey

// handleUpload proxies POST /v1/upload to the connector-hub. The multipart
// body is streamed through unmodified (never buffered fully) with the size
// cap enforced by http.MaxBytesReader as it flows; the tenant — derived from
// the verified JWT by the auth middleware — is forwarded on the internal hop
// as the x-asker-tenant header. The hub's status and JSON body pass through
// verbatim (202 {"doc_id":"..."} on success).
func (d *deps) handleUpload(w http.ResponseWriter, r *http.Request) {
	tc, err := tenancy.FromContext(r.Context())
	if err != nil {
		// Unreachable behind the auth middleware; fail closed regardless.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	contentType := r.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(contentType); err != nil || mt != "multipart/form-data" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected multipart/form-data"})
		return
	}
	if r.ContentLength > d.maxUploadBytes {
		writeUploadTooLarge(w)
		return
	}
	body := http.MaxBytesReader(w, r.Body, d.maxUploadBytes)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, d.hubURL+"/upload", body)
	if err != nil {
		d.internalError(w, r, "build hub upload request", err)
		return
	}
	req.Header.Set("Content-Type", contentType) // preserves the multipart boundary
	req.Header.Set(hubTenantHeader, tenancy.HeaderValue(tc))
	if r.ContentLength >= 0 {
		req.ContentLength = r.ContentLength
	}

	resp, err := d.hubClient.Do(req)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) || strings.Contains(err.Error(), "request body too large") {
			writeUploadTooLarge(w)
			return
		}
		d.logger.Error("upload proxy failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upload unavailable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeUploadTooLarge(w http.ResponseWriter) {
	writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
}
