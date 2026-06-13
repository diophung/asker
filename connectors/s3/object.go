package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"path"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// nativeID is the object's stable native identifier within this connector:
// "<bucket>/<key>". It is what doc_id is derived from, so two objects with the
// same key in different buckets never collide.
func nativeID(bucket, key string) string {
	return bucket + "/" + key
}

// objectDocument maps one S3 object to the canonical FILE Document. body is the
// already-extracted text (empty for large/binary objects). contentType is the
// object's MIME type when known (from a body fetch), else derived from the key
// extension. S3 buckets are single-owner from the connecting credential's view,
// so acl is left unset (ADR-012): there is no per-object principal list to
// capture.
func objectDocument(tenant, bucket string, o object, body, contentType string) *askerv1.Document {
	nat := nativeID(bucket, o.key)
	doc := &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, nat),
		ConnectorId:    connectorID,
		SourceNativeId: nat,
		Type:           askerv1.DocType_FILE,
		Title:          path.Base(o.key),
		BodyText:       body,
		Metadata:       objectMetadata(bucket, o, contentType),
		VersionEtag:    versionEtag(o),
	}
	if !o.lastModified.IsZero() {
		// S3 exposes only a last-modified time; created is unknown, so mirror it.
		ts := timestamppb.New(o.lastModified.UTC())
		doc.Ts = &askerv1.Timestamps{Created: ts, Modified: ts}
	}
	return doc
}

// tombstoneDocument builds the deletion Document for one vanished object:
// identity fields plus tombstone.deleted and deleted_at — no body, no chunks.
// etag is a value newer than any prior upsert (the deletion time, monotonic) so
// the delete wins the idempotent (doc_id, version_etag) merge.
func (c *Connector) tombstoneDocument(tenant, bucket, key string) *askerv1.Document {
	nat := nativeID(bucket, key)
	deletedAt := c.now().UTC()
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, nat),
		ConnectorId:    connectorID,
		SourceNativeId: nat,
		Type:           askerv1.DocType_FILE,
		VersionEtag:    "deleted-" + strconv.FormatInt(deletedAt.UnixNano(), 10),
		Tombstone: &askerv1.Tombstone{
			Deleted:   true,
			DeletedAt: timestamppb.New(deletedAt),
		},
	}
}

// versionEtag derives a content-changing etag. S3's per-object ETag (the MD5,
// or a multipart composite) changes whenever the bytes change, so it is the
// natural version. A last-resort hash of key+size keeps the field non-empty for
// the rare object whose ETag a backend omits.
func versionEtag(o object) string {
	if e := strings.Trim(o.etag, `"`); e != "" {
		return e
	}
	sum := sha256.Sum256([]byte(o.key + ":" + strconv.FormatInt(o.size, 10)))
	return hex.EncodeToString(sum[:])
}

// objectMetadata builds the flat metadata map. Empty values are omitted.
func objectMetadata(bucket string, o object, contentType string) map[string]string {
	md := make(map[string]string, 6)
	put := func(k, v string) {
		if v != "" {
			md[k] = v
		}
	}
	put("bucket", bucket)
	put("key", o.key)
	put("etag", strings.Trim(o.etag, `"`))
	put("size", strconv.FormatInt(o.size, 10))
	put("storage_class", o.storageClass)
	if contentType == "" {
		contentType = contentTypeForKey(o.key)
	}
	put("content_type", contentType)
	return md
}

// contentTypeForKey guesses a MIME type from a key's extension. It is a
// best-effort fallback when the object was not body-fetched (so no response
// Content-Type is available).
func contentTypeForKey(key string) string {
	ext := path.Ext(key)
	if ext == "" {
		return ""
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		// Strip any "; charset=..." parameter for a stable metadata value.
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			return strings.TrimSpace(ct[:i])
		}
		return ct
	}
	return ""
}

// shouldFetchBody reports whether an object is small enough and likely text, so
// its bytes are worth pulling into body_text within the size cap. Larger or
// binary objects are left body-empty for M3 extraction.
func shouldFetchBody(o object) bool {
	if o.size <= 0 || o.size > bodyCap {
		return false
	}
	return isTextContentType(contentTypeForKey(o.key))
}

// isTextContentType reports whether a MIME type names directly-indexable text.
// An empty type (unknown extension) is treated as non-text so the connector
// never slurps an unknown binary into body_text.
func isTextContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	switch {
	case ct == "":
		return false
	case strings.HasPrefix(ct, "text/"):
		return true
	case ct == "application/json",
		ct == "application/xml",
		ct == "application/yaml",
		ct == "application/x-yaml",
		ct == "application/csv":
		return true
	default:
		return false
	}
}
