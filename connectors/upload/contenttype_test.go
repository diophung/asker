package upload

import (
	"context"
	"strings"
	"testing"

	"github.com/asker/asker/platform/mimeguard"
)

// The MIME-type rules themselves are unit-tested in platform/mimeguard; this
// asserts the connector actually routes the client-supplied type through them.
// The upload MIME type comes from the client's multipart part header and is
// echoed back as the Content-Type of GET /v1/media, so a renderable type
// stored here would be stored XSS in the gateway's origin.
func TestHandleUploadStoresNeutralizedContentType(t *testing.T) {
	for _, declared := range []string{
		"image/svg+xml",
		"text/html",
		// The structured-suffix family a flat denylist misses.
		"application/rss+xml",
		"text/xsl",
	} {
		blobs := &fakeBlobs{}
		payload := `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`

		doc, err := HandleUpload(
			context.Background(),
			testDeps(blobs),
			testTenant(t, "tenant-a"),
			strings.NewReader(payload),
			"payload.svg",
			"payload",
			declared,
			int64(len(payload)),
		)
		if err != nil {
			t.Fatalf("HandleUpload(%q): %v", declared, err)
		}
		want := mimeguard.DefaultContentType
		if blobs.last.contentType != want {
			t.Errorf("declared %q: stored blob content type = %q, want %q",
				declared, blobs.last.contentType, want)
		}
		// The Document metadata is what downstream stages read; it must agree.
		if got := doc.GetMetadata()["content_type"]; got != want {
			t.Errorf("declared %q: document content_type metadata = %q, want %q", declared, got, want)
		}
		if got := doc.GetOriginal().GetContentType(); got != want {
			t.Errorf("declared %q: BlobRef content type = %q, want %q", declared, got, want)
		}
	}
}

// Ordinary uploads must keep their real type — the doc-type classification and
// text extraction downstream read this value.
func TestHandleUploadPreservesBenignContentType(t *testing.T) {
	blobs := &fakeBlobs{}
	content := "hello\n"
	if _, err := HandleUpload(
		context.Background(),
		testDeps(blobs),
		testTenant(t, "tenant-a"),
		strings.NewReader(content),
		"notes.txt",
		"notes",
		"text/plain; charset=utf-8",
		int64(len(content)),
	); err != nil {
		t.Fatalf("HandleUpload: %v", err)
	}
	if want := "text/plain; charset=utf-8"; blobs.last.contentType != want {
		t.Errorf("stored content type = %q, want %q (charset must survive)", blobs.last.contentType, want)
	}
}
