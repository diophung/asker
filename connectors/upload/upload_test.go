package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

var fixedNow = time.Date(2026, 6, 12, 10, 30, 0, 0, time.UTC)

// fakeBlobs records Put calls and fabricates BlobRefs the way
// platform/blob.Store does (plaintext size and sha256, tenant-prefixed key).
type fakeBlobs struct {
	puts   int
	lastTC tenancy.Context
	last   struct {
		key, contentType string
		data             []byte
	}
	err error
}

func (f *fakeBlobs) Put(_ context.Context, tc tenancy.Context, key, contentType string, data []byte) (*askerv1.BlobRef, error) {
	f.puts++
	f.lastTC = tc
	f.last.key, f.last.contentType, f.last.data = key, contentType, bytes.Clone(data)
	if f.err != nil {
		return nil, f.err
	}
	sum := sha256.Sum256(data)
	return &askerv1.BlobRef{
		Bucket:      "asker-blobs",
		Key:         string(tc.TenantID()) + "/" + key,
		SizeBytes:   int64(len(data)),
		ContentType: contentType,
		Sha256:      hex.EncodeToString(sum[:]),
	}, nil
}

func testDeps(blobs *fakeBlobs) Deps {
	return Deps{Blobs: blobs, Now: func() time.Time { return fixedNow }}
}

func TestHandleUploadTextGolden(t *testing.T) {
	ctx := context.Background()
	tenant := testTenant(t, "tenant-a")
	blobs := &fakeBlobs{}
	content := []byte("Meeting notes: ship the blob store.\n")

	doc, err := HandleUpload(ctx, testDeps(blobs), tenant,
		bytes.NewReader(content), "notes.txt", "Q2 notes", "text/plain; charset=utf-8", int64(len(content)))
	if err != nil {
		t.Fatalf("HandleUpload: %v", err)
	}

	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	blobKey := "upload/" + digest
	want := &askerv1.Document{
		TenantId:       "tenant-a",
		DocId:          sdk.DocID("upload", blobKey),
		ConnectorId:    "upload",
		SourceNativeId: blobKey,
		Type:           askerv1.DocType_FILE,
		Title:          "Q2 notes",
		BodyText:       string(content),
		Metadata: map[string]string{
			"filename":     "notes.txt",
			"content_type": "text/plain; charset=utf-8",
			"size":         "36",
		},
		Ts: &askerv1.Timestamps{Created: timestamppb.New(fixedNow)},
		Original: &askerv1.BlobRef{
			Bucket:      "asker-blobs",
			Key:         "tenant-a/" + blobKey,
			SizeBytes:   36,
			ContentType: "text/plain; charset=utf-8",
			Sha256:      digest,
		},
		VersionEtag: digest,
	}
	if !proto.Equal(doc, want) {
		t.Errorf("HandleUpload document mismatch:\n got: %v\nwant: %v", doc, want)
	}
	if blobs.last.key != blobKey {
		t.Errorf("blob key = %q, want %q", blobs.last.key, blobKey)
	}
	if !bytes.Equal(blobs.last.data, content) {
		t.Errorf("blob store received %q, want the raw content", blobs.last.data)
	}

	connectortest.ValidateDocument(t, testConfig(t), doc)
}

func TestHandleUploadBinaryGolden(t *testing.T) {
	ctx := context.Background()
	tenant := testTenant(t, "tenant-a")
	blobs := &fakeBlobs{}
	content := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff}

	doc, err := HandleUpload(ctx, testDeps(blobs), tenant,
		bytes.NewReader(content), "pic.png", "", "image/png", int64(len(content)))
	if err != nil {
		t.Fatalf("HandleUpload: %v", err)
	}

	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	blobKey := "upload/" + digest
	want := &askerv1.Document{
		TenantId:       "tenant-a",
		DocId:          sdk.DocID("upload", blobKey),
		ConnectorId:    "upload",
		SourceNativeId: blobKey,
		Type:           askerv1.DocType_FILE,
		Title:          "pic.png", // falls back to the filename
		BodyText:       "",        // binary: empty body until M3 extraction
		Metadata: map[string]string{
			"filename":     "pic.png",
			"content_type": "image/png",
			"size":         "10",
		},
		Ts: &askerv1.Timestamps{Created: timestamppb.New(fixedNow)},
		Original: &askerv1.BlobRef{
			Bucket:      "asker-blobs",
			Key:         "tenant-a/" + blobKey,
			SizeBytes:   10,
			ContentType: "image/png",
			Sha256:      digest,
		},
		VersionEtag: digest,
	}
	if !proto.Equal(doc, want) {
		t.Errorf("HandleUpload document mismatch:\n got: %v\nwant: %v", doc, want)
	}
	connectortest.ValidateDocument(t, testConfig(t), doc)
}

func TestHandleUploadMarkdownByExtension(t *testing.T) {
	// A .md file with a generic content type is still treated as text.
	content := []byte("# Heading\n\nbody")
	doc, err := HandleUpload(context.Background(), testDeps(&fakeBlobs{}), testTenant(t, "tenant-a"),
		bytes.NewReader(content), "README.MD", "", "application/octet-stream", int64(len(content)))
	if err != nil {
		t.Fatalf("HandleUpload: %v", err)
	}
	if doc.GetBodyText() != string(content) {
		t.Errorf("body_text = %q, want the markdown content", doc.GetBodyText())
	}
}

func TestHandleUploadDefaultsContentType(t *testing.T) {
	doc, err := HandleUpload(context.Background(), testDeps(&fakeBlobs{}), testTenant(t, "tenant-a"),
		strings.NewReader("x"), "blob.bin", "t", "", 1)
	if err != nil {
		t.Fatalf("HandleUpload: %v", err)
	}
	if got := doc.GetMetadata()["content_type"]; got != "application/octet-stream" {
		t.Errorf("content_type = %q, want application/octet-stream", got)
	}
	if doc.GetBodyText() != "" {
		t.Errorf("body_text = %q, want empty for unknown binary", doc.GetBodyText())
	}
}

func TestHandleUploadCapDeclaredSize(t *testing.T) {
	blobs := &fakeBlobs{}
	_, err := HandleUpload(context.Background(), testDeps(blobs), testTenant(t, "tenant-a"),
		strings.NewReader("tiny"), "big.bin", "", "application/octet-stream", MaxUploadBytes+1)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if blobs.puts != 0 {
		t.Error("blob store was called despite the size cap")
	}
}

func TestHandleUploadCapStreaming(t *testing.T) {
	// An unknown declared size must still be capped while reading.
	blobs := &fakeBlobs{}
	oversized := io.LimitReader(zeroReader{}, MaxUploadBytes+1)
	_, err := HandleUpload(context.Background(), testDeps(blobs), testTenant(t, "tenant-a"),
		oversized, "big.bin", "", "application/octet-stream", -1)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if blobs.puts != 0 {
		t.Error("blob store was called despite the read cap")
	}
}

func TestHandleUploadAtCapSucceeds(t *testing.T) {
	exactly := io.LimitReader(zeroReader{}, MaxUploadBytes)
	doc, err := HandleUpload(context.Background(), testDeps(&fakeBlobs{}), testTenant(t, "tenant-a"),
		exactly, "cap.bin", "", "application/octet-stream", MaxUploadBytes)
	if err != nil {
		t.Fatalf("HandleUpload at exactly the cap: %v", err)
	}
	if got := doc.GetMetadata()["size"]; got != "33554432" {
		t.Errorf("size metadata = %q, want 33554432", got)
	}
}

// zeroReader yields an endless stream of zero bytes.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestHandleUploadDedupe(t *testing.T) {
	// Same content => same blob key, doc_id, and version_etag, regardless of
	// filename, title, or content type.
	ctx := context.Background()
	tenant := testTenant(t, "tenant-a")
	blobs := &fakeBlobs{}
	content := []byte("identical bytes")

	a, err := HandleUpload(ctx, testDeps(blobs), tenant, bytes.NewReader(content), "a.txt", "first", "text/plain", -1)
	if err != nil {
		t.Fatalf("HandleUpload a: %v", err)
	}
	b, err := HandleUpload(ctx, testDeps(blobs), tenant, bytes.NewReader(content), "b.md", "second", "text/markdown", -1)
	if err != nil {
		t.Fatalf("HandleUpload b: %v", err)
	}

	if a.GetDocId() != b.GetDocId() {
		t.Errorf("doc_id differs for identical content: %q vs %q", a.GetDocId(), b.GetDocId())
	}
	if a.GetVersionEtag() != b.GetVersionEtag() {
		t.Errorf("version_etag differs for identical content: %q vs %q", a.GetVersionEtag(), b.GetVersionEtag())
	}
	if a.GetSourceNativeId() != b.GetSourceNativeId() {
		t.Errorf("source_native_id differs: %q vs %q", a.GetSourceNativeId(), b.GetSourceNativeId())
	}

	c, err := HandleUpload(ctx, testDeps(blobs), tenant, strings.NewReader("different bytes"), "a.txt", "first", "text/plain", -1)
	if err != nil {
		t.Fatalf("HandleUpload c: %v", err)
	}
	if c.GetDocId() == a.GetDocId() {
		t.Error("different content produced the same doc_id")
	}
}

func TestHandleUploadBodyTextCap(t *testing.T) {
	// 1 MiB - 1 of ASCII followed by a 2-byte rune straddling the cap: the
	// body must be cut at the rune boundary, not mid-rune.
	content := append(bytes.Repeat([]byte{'a'}, maxBodyTextBytes-1), []byte("é")...)
	doc, err := HandleUpload(context.Background(), testDeps(&fakeBlobs{}), testTenant(t, "tenant-a"),
		bytes.NewReader(content), "big.txt", "", "text/plain", int64(len(content)))
	if err != nil {
		t.Fatalf("HandleUpload: %v", err)
	}
	body := doc.GetBodyText()
	if len(body) != maxBodyTextBytes-1 {
		t.Errorf("body length = %d, want %d (cut at the rune boundary)", len(body), maxBodyTextBytes-1)
	}
	if !utf8.ValidString(body) {
		t.Error("body_text is not valid UTF-8")
	}
	if strings.ContainsRune(body, 'é') {
		t.Error("the straddling rune survived the cap")
	}
}

func TestHandleUploadInvalidUTF8Text(t *testing.T) {
	// A .txt upload with invalid UTF-8 must still produce a valid proto
	// string body (invalid sequences dropped), or marshaling would fail
	// downstream in the hub.
	content := []byte("ok \xff\xfe bytes")
	doc, err := HandleUpload(context.Background(), testDeps(&fakeBlobs{}), testTenant(t, "tenant-a"),
		bytes.NewReader(content), "weird.txt", "", "text/plain", int64(len(content)))
	if err != nil {
		t.Fatalf("HandleUpload: %v", err)
	}
	if !utf8.ValidString(doc.GetBodyText()) {
		t.Errorf("body_text %q is not valid UTF-8", doc.GetBodyText())
	}
	if _, err := proto.Marshal(doc); err != nil {
		t.Errorf("document does not marshal: %v", err)
	}
	// The original blob keeps the exact bytes regardless.
	if doc.GetVersionEtag() == "" {
		t.Error("version_etag missing")
	}
}

func TestHandleUploadFailClosed(t *testing.T) {
	ctx := context.Background()
	blobs := &fakeBlobs{}
	deps := testDeps(blobs)
	tenant := testTenant(t, "tenant-a")

	if _, err := HandleUpload(ctx, Deps{Now: deps.Now}, tenant, strings.NewReader("x"), "f", "", "", 1); err == nil {
		t.Error("HandleUpload succeeded without a blob store")
	}
	if _, err := HandleUpload(ctx, deps, tenancy.Context{}, strings.NewReader("x"), "f", "", "", 1); !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("HandleUpload with zero tenant: err = %v, want ErrNoTenant", err)
	}
	if _, err := HandleUpload(ctx, deps, tenant, nil, "f", "", "", 1); err == nil {
		t.Error("HandleUpload succeeded with a nil reader")
	}
	if blobs.puts != 0 {
		t.Error("blob store was called on a failed-closed path")
	}
}

func TestHandleUploadBlobErrorPropagates(t *testing.T) {
	blobs := &fakeBlobs{err: errors.New("minio down")}
	_, err := HandleUpload(context.Background(), testDeps(blobs), testTenant(t, "tenant-a"),
		strings.NewReader("x"), "f.txt", "", "text/plain", 1)
	if err == nil || !strings.Contains(err.Error(), "minio down") {
		t.Errorf("err = %v, want wrapped blob store error", err)
	}
}

func TestHandleUploadReadErrorPropagates(t *testing.T) {
	r := io.MultiReader(strings.NewReader("partial"), failingReader{})
	_, err := HandleUpload(context.Background(), testDeps(&fakeBlobs{}), testTenant(t, "tenant-a"),
		r, "f.txt", "", "text/plain", -1)
	if err == nil || !strings.Contains(err.Error(), "read file") {
		t.Errorf("err = %v, want read error", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("source vanished") }

func TestHandleUploadNowDefaults(t *testing.T) {
	before := time.Now()
	doc, err := HandleUpload(context.Background(), Deps{Blobs: &fakeBlobs{}}, testTenant(t, "tenant-a"),
		strings.NewReader("x"), "f.txt", "", "text/plain", 1)
	if err != nil {
		t.Fatalf("HandleUpload: %v", err)
	}
	created := doc.GetTs().GetCreated().AsTime()
	if created.Before(before.Add(-time.Second)) || created.After(time.Now().Add(time.Second)) {
		t.Errorf("ts.created = %v, want ~now", created)
	}
	if doc.GetTs().GetIngested() != nil {
		t.Error("ts.ingested is set; the hub stamps it")
	}
}
