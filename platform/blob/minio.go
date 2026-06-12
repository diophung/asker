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

// getObject fetches bucket/key, mapping a missing object to ErrNotFound.
func (c *minioClient) getObject(ctx context.Context, bucket, key string) ([]byte, error) {
	obj, err := c.mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(obj)
	if err != nil {
		// GetObject is lazy: server-side errors (including a missing key)
		// surface on the first read.
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			return nil, fmt.Errorf("get object %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	return data, nil
}
