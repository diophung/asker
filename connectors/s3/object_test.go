package s3

import (
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func TestObjectDocumentMapping(t *testing.T) {
	t.Parallel()
	lm := time.Date(2026, 6, 11, 14, 15, 0, 0, time.UTC)
	o := object{
		key:          "docs/notes.txt",
		etag:         `"notesetag333"`,
		size:         22,
		storageClass: "STANDARD",
		lastModified: lm,
	}
	doc := objectDocument("tenant-x", "my-bucket", o, "body text", "text/plain")

	if got := doc.GetType(); got != askerv1.DocType_FILE {
		t.Errorf("type = %v, want FILE", got)
	}
	if got, want := doc.GetSourceNativeId(), "my-bucket/docs/notes.txt"; got != want {
		t.Errorf("source_native_id = %q, want %q", got, want)
	}
	if got, want := doc.GetDocId(), sdk.DocID(connectorID, "my-bucket/docs/notes.txt"); got != want {
		t.Errorf("doc_id = %q, want %q", got, want)
	}
	if got, want := doc.GetTitle(), "notes.txt"; got != want {
		t.Errorf("title = %q, want basename %q", got, want)
	}
	if got, want := doc.GetBodyText(), "body text"; got != want {
		t.Errorf("body_text = %q, want %q", got, want)
	}
	if got, want := doc.GetVersionEtag(), "notesetag333"; got != want {
		t.Errorf("version_etag = %q, want unquoted etag %q", got, want)
	}
	md := doc.GetMetadata()
	for k, want := range map[string]string{
		"bucket":        "my-bucket",
		"key":           "docs/notes.txt",
		"etag":          "notesetag333",
		"size":          "22",
		"storage_class": "STANDARD",
		"content_type":  "text/plain",
	} {
		if md[k] != want {
			t.Errorf("metadata[%q] = %q, want %q", k, md[k], want)
		}
	}
	if doc.GetTs().GetCreated().AsTime().UTC() != lm {
		t.Errorf("ts.created = %v, want %v", doc.GetTs().GetCreated().AsTime(), lm)
	}
	if doc.GetTs().GetModified().AsTime().UTC() != lm {
		t.Errorf("ts.modified = %v, want %v", doc.GetTs().GetModified().AsTime(), lm)
	}
	// S3 is single-owner from the credential's view: no ACL captured.
	if doc.GetAcl() != nil {
		t.Errorf("acl = %v, want nil for a single-owner bucket", doc.GetAcl())
	}
}

func TestObjectDocumentContentTypeFallbackFromKey(t *testing.T) {
	t.Parallel()
	o := object{key: "a/b/file.json", etag: "e", size: 3}
	doc := objectDocument("t", "bkt", o, "", "")
	if got, want := doc.GetMetadata()["content_type"], "application/json"; got != want {
		t.Errorf("content_type fallback = %q, want %q", got, want)
	}
}

func TestObjectDocumentNoTimestampWhenZero(t *testing.T) {
	t.Parallel()
	o := object{key: "k", etag: "e", size: 1}
	doc := objectDocument("t", "b", o, "", "text/plain")
	if doc.GetTs() != nil {
		t.Errorf("ts = %v, want nil when LastModified is zero", doc.GetTs())
	}
}

func TestTombstoneDocument(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	c := New(withClock(func() time.Time { return fixed })).(*Connector)
	doc := c.tombstoneDocument("tenant-x", "my-bucket", "docs/gone.txt")

	if !doc.GetTombstone().GetDeleted() {
		t.Error("tombstone.deleted = false, want true")
	}
	if doc.GetBodyText() != "" {
		t.Errorf("tombstone body_text = %q, want empty", doc.GetBodyText())
	}
	if len(doc.GetChunks()) != 0 {
		t.Errorf("tombstone has %d chunks, want 0", len(doc.GetChunks()))
	}
	if doc.GetVersionEtag() == "" {
		t.Error("tombstone version_etag is empty")
	}
	if got, want := doc.GetDocId(), sdk.DocID(connectorID, "my-bucket/docs/gone.txt"); got != want {
		t.Errorf("doc_id = %q, want %q", got, want)
	}
	if !doc.GetTombstone().GetDeletedAt().AsTime().Equal(fixed) {
		t.Errorf("deleted_at = %v, want %v", doc.GetTombstone().GetDeletedAt().AsTime(), fixed)
	}
}

func TestVersionEtagFallback(t *testing.T) {
	t.Parallel()
	// With no etag, a stable hash of key+size keeps version_etag non-empty.
	a := versionEtag(object{key: "k", size: 10})
	b := versionEtag(object{key: "k", size: 10})
	c := versionEtag(object{key: "k", size: 11})
	if a == "" {
		t.Fatal("fallback etag is empty")
	}
	if a != b {
		t.Errorf("fallback etag not stable: %q != %q", a, b)
	}
	if a == c {
		t.Error("fallback etag did not change with size")
	}
}

func TestShouldFetchBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		o    object
		want bool
	}{
		{"small text", object{key: "a.txt", size: 100}, true},
		{"small json", object{key: "a.json", size: 100}, true},
		{"too large text", object{key: "a.txt", size: bodyCap + 1}, false},
		{"zero size", object{key: "a.txt", size: 0}, false},
		{"binary pdf", object{key: "a.pdf", size: 100}, false},
		{"no extension", object{key: "README", size: 100}, false},
		{"image", object{key: "a.png", size: 100}, false},
	}
	for _, tc := range cases {
		if got := shouldFetchBody(tc.o); got != tc.want {
			t.Errorf("%s: shouldFetchBody = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestContentTypeForKey(t *testing.T) {
	t.Parallel()
	if got := contentTypeForKey("noext"); got != "" {
		t.Errorf("contentTypeForKey(noext) = %q, want empty", got)
	}
	if got := contentTypeForKey("a.unknownext999"); got != "" {
		t.Errorf("contentTypeForKey(unknown) = %q, want empty", got)
	}
	if got := contentTypeForKey("a.txt"); got != "text/plain" {
		t.Errorf("contentTypeForKey(.txt) = %q, want text/plain (no charset)", got)
	}
}

func TestIsTextContentType(t *testing.T) {
	t.Parallel()
	for ct, want := range map[string]bool{
		"text/plain":         true,
		"text/csv":           true,
		"application/json":   true,
		"application/xml":    true,
		"application/x-yaml": true,
		"":                   false,
		"image/png":          false,
		"application/pdf":    false,
	} {
		if got := isTextContentType(ct); got != want {
			t.Errorf("isTextContentType(%q) = %v, want %v", ct, got, want)
		}
	}
}
