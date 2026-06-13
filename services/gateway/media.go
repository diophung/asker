package main

import (
	"io"
	"net/http"
	"net/url"

	"github.com/asker/asker/platform/tenancy"
)

// handleMedia proxies GET /v1/media?key=<blobKey> to the connector-hub's
// internal media endpoint so the browser can fetch a search Hit's
// thumbnail_key. The hub endpoint is internal-only; the gateway is the public,
// authenticated front door for it.
//
// Isolation (defense in depth): the tenant is derived ONLY from the verified
// JWT by the auth middleware and forwarded as the x-asker-tenant header on the
// internal hop — NEVER from the query string or any client header. The blob
// key is forwarded as-is; the hub fail-closes by enforcing that the key lies
// under the requesting tenant's prefix, so a user can only ever read blobs in
// their own tenant. The two layers together mean a forged key cannot escape
// the JWT tenant.
func (d *deps) handleMedia(w http.ResponseWriter, r *http.Request) {
	tc, err := tenancy.FromContext(r.Context())
	if err != nil {
		// Unreachable behind the auth middleware; fail closed regardless.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key is required"})
		return
	}

	target := d.hubURL + "/internal/media?key=" + url.QueryEscape(key)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		// A bad key can produce a request that cannot be built (e.g. control
		// bytes in the URL); treat it as a client error, not a 500.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid key"})
		return
	}
	req.Header.Set(hubTenantHeader, tenancy.HeaderValue(tc))

	resp, err := d.mediaClient.Do(req)
	if err != nil {
		d.logger.Error("media proxy failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "media unavailable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Pass through the upstream Content-Type and status (200 on success, 404
	// when the blob is absent), then stream the body back under a hard size
	// cap. The cap guards against a misbehaving hub: thumbnails/keyframes are
	// small, and a 404 body (the hub's JSON error) is smaller still.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, d.maxMediaBytes))
}
