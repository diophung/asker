package s3

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
)

// client is the thin seam over a minio-go client this connector talks to S3
// through. It pins the configured bucket and key prefix so callers work in
// object-key terms.
type client struct {
	mc     *minio.Client
	bucket string
	prefix string
}

// object is the listing view of one S3 object the connector maps to a Document.
// ListObjectsV2 does not return a content type, so contentType is derived later
// (from a body fetch's response header, or from the key extension).
type object struct {
	key          string
	etag         string
	size         int64
	storageClass string
	lastModified time.Time
}

// listFunc receives each listed object in lexicographic (S3 sort) order until
// it returns an error, which aborts the walk.
type listFunc func(object) error

// list walks every object under the configured prefix in key order, invoking fn
// per object. startAfter resumes a lexicographic listing past a key (empty =
// from the beginning); minio-go pages transparently and S3 returns keys sorted,
// so the walk is deterministic and resumable. A per-object listing error
// (ObjectInfo.Err) aborts the walk.
func (c *client) list(ctx context.Context, startAfter string, fn listFunc) error {
	opts := minio.ListObjectsOptions{
		Prefix:     c.prefix,
		Recursive:  true,
		StartAfter: startAfter,
		MaxKeys:    listPageSize,
	}
	for info := range c.mc.ListObjects(ctx, c.bucket, opts) {
		if info.Err != nil {
			return fmt.Errorf("s3: list objects: %w", info.Err)
		}
		obj := object{
			key:          info.Key,
			etag:         info.ETag,
			size:         info.Size,
			storageClass: info.StorageClass,
			lastModified: info.LastModified,
		}
		if err := fn(obj); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// probe makes the cheapest possible authenticated call against the bucket: list
// at most one object under the prefix. It surfaces a credential/bucket error
// without emitting anything. An empty bucket (zero objects) is a valid,
// reachable bucket. The context is canceled on the first result so minio-go's
// listing goroutine does not leak past the single object we want.
func (c *client) probe(ctx context.Context) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	opts := minio.ListObjectsOptions{Prefix: c.prefix, Recursive: true, MaxKeys: 1}
	info, ok := <-c.mc.ListObjects(cctx, c.bucket, opts)
	if !ok {
		// Channel closed with no object: an empty but reachable bucket.
		return ctx.Err()
	}
	return info.Err
}

// fetchBody downloads up to bodyCap bytes of an object and returns the decoded
// text plus the object's content type (from the GET response). It is called
// only for objects the mapper deems small text, so a single GET per object is
// the whole cost. The caller treats any error as "leave the body empty".
func (c *client) fetchBody(ctx context.Context, key string) (body string, contentType string, err error) {
	obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return "", "", fmt.Errorf("s3: get object %q: %w", key, err)
	}
	defer func() { _ = obj.Close() }()

	// Bound the read; GetObject is lazy, so the HTTP GET and any server-side
	// error surface here on the first read.
	data, err := io.ReadAll(io.LimitReader(obj, bodyCap))
	if err != nil {
		return "", "", fmt.Errorf("s3: read object %q: %w", key, err)
	}
	// Stat reflects the response headers (content type) once the body is read.
	if st, serr := obj.Stat(); serr == nil {
		contentType = st.ContentType
	}
	return string(data), contentType, nil
}
