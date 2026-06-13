package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

const (
	// MaxUploadBytes caps a single upload at 32 MiB.
	MaxUploadBytes = 32 << 20
	// maxBodyTextBytes caps Document.body_text for textual uploads at 1 MiB.
	maxBodyTextBytes = 1 << 20
)

// ErrTooLarge is returned by HandleUpload when the file exceeds
// MaxUploadBytes (by declared size or by actually reading past the cap).
var ErrTooLarge = errors.New("upload: file exceeds the 32 MiB upload limit")

// BlobStore is the slice of platform/blob.Store that HandleUpload needs.
type BlobStore interface {
	Put(ctx context.Context, tc tenancy.Context, key string, contentType string, data []byte) (*askerv1.BlobRef, error)
}

// Deps are the hub-provided collaborators for HandleUpload.
type Deps struct {
	// Blobs stores the original bytes, encrypted per tenant. Required.
	Blobs BlobStore
	// Now stamps ts.created. Defaults to time.Now when nil.
	Now func() time.Time
}

// HandleUpload turns one uploaded file into a stored blob plus the canonical
// Document the hub will emit. The content is read fully (capped at
// MaxUploadBytes), stored under the content-addressed blob key
// "upload/<sha256>" — so identical content deduplicates to the same blob,
// doc_id, and version_etag — and described as a FILE document whose
// body_text is the content itself for textual uploads (text/* media types or
// .txt/.md filenames, capped at 1 MiB); other formats keep an empty body
// until M3 extraction.
//
// HandleUpload does NOT stamp ts.ingested and does NOT emit: the hub does
// both when it routes the returned Document to Kafka.
func HandleUpload(ctx context.Context, deps Deps, tenant tenancy.Context, file io.Reader, filename, title, contentType string, size int64) (*askerv1.Document, error) {
	if deps.Blobs == nil {
		return nil, errors.New("upload: Deps.Blobs is required")
	}
	if tenant.TenantID() == "" {
		return nil, fmt.Errorf("upload: %w", tenancy.ErrNoTenant)
	}
	if file == nil {
		return nil, errors.New("upload: nil file reader")
	}
	if size > MaxUploadBytes {
		return nil, fmt.Errorf("%w: declared size %d", ErrTooLarge, size)
	}

	content, err := io.ReadAll(io.LimitReader(file, MaxUploadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("upload: read file: %w", err)
	}
	if len(content) > MaxUploadBytes {
		return nil, ErrTooLarge
	}
	contentType = strings.ToValidUTF8(contentType, "")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	blobKey := "upload/" + digest

	ref, err := deps.Blobs.Put(ctx, tenant, blobKey, contentType, content)
	if err != nil {
		return nil, fmt.Errorf("upload: store blob: %w", err)
	}

	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	// proto3 string fields must hold valid UTF-8, so everything that came in
	// from the outside is sanitized before it lands in the Document.
	filename = strings.ToValidUTF8(filename, "")
	docTitle := strings.ToValidUTF8(title, "")
	if docTitle == "" {
		docTitle = filename
	}
	var bodyText string
	if isTextual(contentType, filename) {
		bodyText = truncateUTF8([]byte(strings.ToValidUTF8(string(content), "")), maxBodyTextBytes)
	}

	return &askerv1.Document{
		TenantId:       string(tenant.TenantID()),
		DocId:          sdk.DocID(ID, blobKey),
		ConnectorId:    ID,
		SourceNativeId: blobKey,
		Type:           classifyDocType(contentType),
		Title:          docTitle,
		BodyText:       bodyText,
		Metadata: map[string]string{
			"filename":     filename,
			"content_type": contentType,
			"size":         strconv.Itoa(len(content)),
		},
		Ts:          &askerv1.Timestamps{Created: timestamppb.New(now())},
		Original:    ref,
		VersionEtag: digest,
	}, nil
}

// classifyDocType maps the upload's MIME type to a DocType so media uploads
// reach the M3 media-enrich path (OCR/CLIP/ASR) instead of being indexed as a
// plain FILE. image/* -> IMAGE, video/* -> VIDEO, audio/* -> AUDIO; anything
// else (incl. text and unknown) -> FILE.
func classifyDocType(contentType string) askerv1.DocType {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return askerv1.DocType_FILE
	}
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return askerv1.DocType_IMAGE
	case strings.HasPrefix(mediaType, "video/"):
		return askerv1.DocType_VIDEO
	case strings.HasPrefix(mediaType, "audio/"):
		return askerv1.DocType_AUDIO
	default:
		return askerv1.DocType_FILE
	}
}

// isTextual reports whether the upload should have its content indexed as
// body_text in M1: any text/* media type, or a .txt/.md filename.
func isTextual(contentType, filename string) bool {
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil && strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch strings.ToLower(path.Ext(filename)) {
	case ".txt", ".md":
		return true
	}
	return false
}

// truncateUTF8 returns b as a string of at most max bytes, never splitting a
// UTF-8 rune (at most utf8.UTFMax-1 trailing bytes are dropped, so invalid
// binary content is not scanned unboundedly).
func truncateUTF8(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	b = b[:max]
	for i := 0; i < utf8.UTFMax-1 && len(b) > 0; i++ {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size != 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return string(b)
}
