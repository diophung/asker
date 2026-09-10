package upload

import (
	"context"
	"strings"
	"testing"
)

// The upload MIME type comes straight from the client's multipart part header
// and is echoed back as the Content-Type of GET /v1/media. A stored
// "image/svg+xml" or "text/html" would therefore render as an active document
// in the gateway's origin — stored XSS. These types must be neutralized at
// ingest.
func TestSanitizeContentTypeNeutralizesActiveTypes(t *testing.T) {
	for _, in := range []string{
		"text/html",
		"TEXT/HTML",
		"text/html; charset=utf-8",
		"  text/html  ",
		"image/svg+xml",
		"application/xhtml+xml",
		"application/xml",
		"text/xml",
		"application/xslt+xml",
		"text/javascript",
		"application/javascript",
		"application/x-javascript",
		"application/ecmascript",
		"text/ecmascript",
		"application/x-shockwave-flash",
	} {
		if got := sanitizeContentType(in); got != defaultContentType {
			t.Errorf("sanitizeContentType(%q) = %q, want %q", in, got, defaultContentType)
		}
	}
}

func TestSanitizeContentTypeRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"not-a-media-type",
		"text/html/extra",
		"text/plain; charset",    // malformed parameter
		"image/png\x00text/html", // NUL smuggling
		"image/png, text/html",   // list form
	} {
		if got := sanitizeContentType(in); got != defaultContentType {
			t.Errorf("sanitizeContentType(%q) = %q, want %q", in, got, defaultContentType)
		}
	}
}

func TestSanitizeContentTypePreservesBenignTypes(t *testing.T) {
	cases := map[string]string{
		"image/png":                 "image/png",
		"IMAGE/PNG":                 "image/png",
		"image/jpeg":                "image/jpeg",
		"video/mp4":                 "video/mp4",
		"audio/mpeg":                "audio/mpeg",
		"application/pdf":           "application/pdf",
		"text/plain":                "text/plain",
		"text/plain; charset=utf-8": "text/plain; charset=utf-8",
		"text/markdown":             "text/markdown",
		"application/octet-stream":  "application/octet-stream",
		// Office documents must keep working — an allowlist would have
		// degraded these to octet-stream and broken doc classification.
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	}
	for in, want := range cases {
		if got := sanitizeContentType(in); got != want {
			t.Errorf("sanitizeContentType(%q) = %q, want %q", in, got, want)
		}
	}
}

// End-to-end: the type an attacker declares must not reach the blob store.
func TestHandleUploadStoresNeutralizedContentType(t *testing.T) {
	blobs := &fakeBlobs{}
	payload := `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`

	doc, err := HandleUpload(
		context.Background(),
		testDeps(blobs),
		testTenant(t, "tenant-a"),
		strings.NewReader(payload),
		"payload.svg",
		"payload",
		"image/svg+xml",
		int64(len(payload)),
	)
	if err != nil {
		t.Fatalf("HandleUpload: %v", err)
	}
	if blobs.last.contentType != defaultContentType {
		t.Errorf("stored blob content type = %q, want %q (svg must not be stored as a renderable type)",
			blobs.last.contentType, defaultContentType)
	}
	// The Document metadata is what downstream stages read; it must agree.
	if got := doc.GetMetadata()["content_type"]; got != defaultContentType {
		t.Errorf("document content_type metadata = %q, want %q", got, defaultContentType)
	}
	if got := doc.GetOriginal().GetContentType(); got != defaultContentType {
		t.Errorf("BlobRef content type = %q, want %q", got, defaultContentType)
	}
}
