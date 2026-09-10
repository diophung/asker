// Package mimeguard normalizes untrusted MIME types before they are stored on
// a blob and later reflected as a response Content-Type.
//
// Every byte-serving path in Asker echoes the stored Content-Type back to the
// caller (GET /v1/media, proxied by the gateway). Those bytes and that type
// both originate outside the system — an upload's multipart part header, or a
// worker-supplied query parameter — so a type a browser renders as an active
// document turns stored content into script running in the serving origin.
//
// This lives in platform/ rather than in one connector because the invariant
// has to hold at EVERY storage boundary. It previously existed only in the
// upload connector, which left PUT /internal/media able to store a renderable
// type; keeping one implementation means a new write path cannot quietly opt
// out of it.
package mimeguard

import (
	"mime"
	"strings"
)

// DefaultContentType is the inert type used for empty, malformed, or
// actively-dangerous MIME types.
const DefaultContentType = "application/octet-stream"

// activeContentTypes are media types a browser will execute script from when
// it renders the bytes as a top-level document.
//
// This is a denylist of *renderable* types rather than an allowlist of safe
// ones on purpose: uploads are arbitrary user files (PDFs, Office documents,
// archives, source code), and an allowlist would silently degrade all of them
// to octet-stream, breaking the doc-type classification and text extraction
// that read this value.
//
// A flat denylist is spelling-dependent, though, so SanitizeContentType also
// applies the structural rules below — that is what closes the open-ended
// "+xml" family rather than requiring every vendor spelling to be enumerated
// here.
var activeContentTypes = map[string]bool{
	"text/html":                              true,
	"text/x-html":                            true,
	"application/xhtml+xml":                  true,
	"application/xhtml":                      true,
	"image/svg+xml":                          true,
	"image/svg":                              true,
	"application/svg+xml":                    true,
	"application/xml":                        true,
	"text/xml":                               true,
	"text/xml-external-parsed-entity":        true,
	"application/xml-external-parsed-entity": true,
	"application/xml-dtd":                    true,
	"application/xslt+xml":                   true,
	"text/xsl":                               true,
	"text/javascript":                        true,
	"application/javascript":                 true,
	"application/x-javascript":               true,
	"application/ecmascript":                 true,
	"text/ecmascript":                        true,
	"application/x-shockwave-flash":          true,
}

// SanitizeContentType normalizes an untrusted MIME type into one that is safe
// to store and later reflect as a response Content-Type. Anything unparseable
// or script-bearing becomes DefaultContentType; otherwise the type is
// re-serialized from the parsed media type and parameters, which normalizes
// casing and whitespace and drops trailing junk after the parameter list.
// Legitimate parameters are preserved, so "text/plain; charset=utf-8" survives
// intact.
//
// PDF is deliberately NOT neutralized: browsers render it in a sandboxed
// viewer that cannot script the embedding origin, and downgrading it would
// break the common case of storing documents.
func SanitizeContentType(contentType string) string {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return DefaultContentType
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))

	// ParseMediaType accepts a bare token with no "/" (e.g. "attachment"),
	// which is not a usable Content-Type; require type/subtype so nothing
	// malformed is ever stored and later reflected to a browser.
	slash := strings.IndexByte(mediaType, '/')
	if slash <= 0 || slash == len(mediaType)-1 {
		return DefaultContentType
	}
	if isActive(mediaType) {
		return DefaultContentType
	}
	// FormatMediaType re-quotes parameters and returns "" if the result would
	// be malformed; fall back to the bare media type in that case.
	if formatted := mime.FormatMediaType(mediaType, params); formatted != "" {
		return formatted
	}
	return mediaType
}

// isActive reports whether a normalized media type may render as an active
// document. The structural rules matter more than the name list: the "+xml"
// structured suffix is open-ended (application/rss+xml, application/atom+xml,
// application/mathml+xml, application/vnd.mozilla.xul+xml, ...), and browsers
// render several of those as documents that honor XML processing instructions
// or drive XSLT, which can emit HTML into the origin. Enumerating every
// spelling is a losing game, so the whole family is neutralized.
func isActive(mediaType string) bool {
	if activeContentTypes[mediaType] {
		return true
	}
	if strings.HasSuffix(mediaType, "+xml") {
		return true
	}
	// multipart/x-mixed-replace streams documents the browser renders in
	// sequence; no legitimate stored blob needs it.
	return strings.HasPrefix(mediaType, "multipart/")
}
