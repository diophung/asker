package blob

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// minioClient adapts a minio-go/v7 client to the objectAPI seam. minio-go
// uses path-style addressing by default for raw host:port endpoints, which
// is what MinIO expects.
type minioClient struct {
	mc *minio.Client
}

// newMinioClient builds a minio-go client for endpoint (host:port, no
// scheme) with static V4 credentials.
func newMinioClient(endpoint, accessKey, secretKey string, useSSL bool) (*minioClient, error) {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("new minio client for %q: %w", endpoint, err)
	}
	return &minioClient{mc: mc}, nil
}

// ensureBucket checks for bucket and creates it when missing. Losing a
// creation race (the bucket already exists by the time we PUT) counts as
// success.
func (c *minioClient) ensureBucket(ctx context.Context, bucket string) error {
	exists, err := c.mc.BucketExists(ctx, bucket)
	if err != nil {
		return fmt.Errorf("head bucket %q: %w", bucket, err)
	}
	if exists {
		return nil
	}
	if err := c.mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		switch minio.ToErrorResponse(err).Code {
		case minio.BucketAlreadyOwnedByYou, minio.BucketAlreadyExists:
			return nil
		}
		return fmt.Errorf("create bucket %q: %w", bucket, err)
	}
	return nil
}

// putObject stores data under bucket/key.
func (c *minioClient) putObject(ctx context.Context, bucket, key, contentType string, data []byte) error {
	_, err := c.mc.PutObject(ctx, bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("put object %q: %w", key, err)
	}
	return nil
}

// getObject fetches bucket/key, returning the bytes and the object's stored
// Content-Type, and mapping a missing object to ErrNotFound.
func (c *minioClient) getObject(ctx context.Context, bucket, key string) ([]byte, string, error) {
	obj, err := c.mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("get object %q: %w", key, err)
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(obj)
	if err != nil {
		// GetObject is lazy: server-side errors (including a missing key)
		// surface on the first read.
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			return nil, "", fmt.Errorf("get object %q: %w", key, ErrNotFound)
		}
		return nil, "", fmt.Errorf("get object %q: %w", key, err)
	}
	// Stat shares the same lazily fetched object handle, so the Content-Type
	// is already available without a second round trip. A Stat error here is
	// non-fatal: the bytes decrypted fine, so fall back to no content-type
	// (the caller defaults it) rather than failing the read.
	contentType := ""
	if info, statErr := obj.Stat(); statErr == nil {
		contentType = info.ContentType
	}
	return data, contentType, nil
}

// removePrefix lists and deletes every object under bucket/prefix, returning
// the number removed. Empty prefix removes nothing (the caller never passes
// one; defensive belt-and-braces). Listing is recursive so nested keys
// (originals + thumbnails/keyframes) are all reached.
func (c *minioClient) removePrefix(ctx context.Context, bucket, prefix string) (int, error) {
	if prefix == "" {
		// Refuse a bucket-wide wipe: callers always scope to a tenant prefix.
		return 0, fmt.Errorf("remove prefix: refusing empty prefix for bucket %q", bucket)
	}
	objCh := c.mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})
	// RemoveObjects consumes a channel of keys and streams back any per-object
	// errors. We re-fan the listing into the removal channel and count as we go.
	removeCh := make(chan minio.ObjectInfo)
	errCh := c.mc.RemoveObjects(ctx, bucket, removeCh, minio.RemoveObjectsOptions{})

	var count int
	var listErr error
	go func() {
		defer close(removeCh)
		for obj := range objCh {
			if obj.Err != nil {
				listErr = obj.Err
				return
			}
			select {
			case removeCh <- obj:
				count++
			case <-ctx.Done():
				listErr = ctx.Err()
				return
			}
		}
	}()

	// Drain removal errors; the first one fails the operation (the caller's
	// verification step re-checks emptiness, so a partial delete is detected).
	var rmErr error
	for e := range errCh {
		if e.Err != nil && rmErr == nil {
			rmErr = fmt.Errorf("remove object %q: %w", e.ObjectName, e.Err)
		}
	}
	if listErr != nil {
		return count, fmt.Errorf("list objects under %q: %w", prefix, listErr)
	}
	if rmErr != nil {
		return count, rmErr
	}
	return count, nil
}
