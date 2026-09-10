package main

import "net/http"

// The gateway is a JSON API plus a media byte-serving endpoint. It never
// returns HTML a browser should render and is never a legitimate frame
// target, so it can afford a maximally restrictive header set:
//
//   - X-Content-Type-Options: nosniff — stops a browser from MIME-sniffing a
//     JSON or octet-stream body into text/html and executing it. This is the
//     one that matters most here, because /v1/media reflects a stored
//     Content-Type that originally came from an upload's multipart header.
//   - X-Frame-Options / frame-ancestors — the UI drives OAuth grants and
//     destructive data controls (DELETE /v1/me/data), so a framed click has
//     real consequence. Both are sent: frame-ancestors is the modern rule,
//     X-Frame-Options still covers older browsers.
//   - default-src 'none' — an API response has no legitimate subresources, so
//     nothing may load if a response is ever rendered as a document.
//   - Referrer-Policy — search URLs can carry the query string; without this
//     the full URL leaks to third-party sites in the Referer header.
//
// HSTS is deliberately NOT set here. The dev stack is plain HTTP on
// 127.0.0.1, and sending Strict-Transport-Security would pin localhost to
// HTTPS in the developer's browser and break the stack in a way that is
// painful to undo. It belongs at the TLS-terminating ingress in production.
var securityHeaders = map[string]string{
	"X-Content-Type-Options":  "nosniff",
	"X-Frame-Options":         "DENY",
	"Referrer-Policy":         "no-referrer",
	"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'",
}

// securityHeadersMiddleware sets the baseline security headers on every
// response. It is applied outermost so error paths, 404s, redirects, and
// preflight responses carry them too — those are exactly the responses a
// handler-level implementation tends to miss.
//
// Headers are set before next.ServeHTTP so a handler that writes its own
// (media.go tightens Content-Security-Policy to a sandbox and adds
// Content-Disposition) can override any of them.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		for k, v := range securityHeaders {
			h.Set(k, v)
		}
		next.ServeHTTP(w, r)
	})
}
