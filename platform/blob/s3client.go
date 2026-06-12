package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	sigAlgorithm  = "AWS4-HMAC-SHA256"
	s3Service     = "s3"
	s3Region      = "us-east-1" // MinIO's default; minio-go assumes the same
	amzDateFormat = "20060102T150405Z"
	amzDateStamp  = "20060102"
	errBodyLimit  = 512
)

// s3Client is a minimal S3-compatible object client speaking AWS Signature
// Version 4 over net/http with path-style addressing. It implements exactly
// the objectAPI surface Store needs.
type s3Client struct {
	endpoint  string // host[:port], no scheme
	accessKey string
	secretKey string
	useSSL    bool
	httpc     *http.Client
	now       func() time.Time
}

// newS3Client returns an s3Client for endpoint (host:port, no scheme).
func newS3Client(endpoint, accessKey, secretKey string, useSSL bool) *s3Client {
	return &s3Client{
		endpoint:  endpoint,
		accessKey: accessKey,
		secretKey: secretKey,
		useSSL:    useSSL,
		httpc:     &http.Client{Timeout: 60 * time.Second},
		now:       time.Now,
	}
}

// ensureBucket checks for bucket and creates it when missing. A 409 on
// creation (lost race / already owned) counts as success.
func (c *s3Client) ensureBucket(ctx context.Context, bucket string) error {
	resp, err := c.do(ctx, http.MethodHead, bucket, "", "", nil)
	if err != nil {
		return fmt.Errorf("head bucket %q: %w", bucket, err)
	}
	drainClose(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		// Bucket is missing: create it below.
	default:
		return fmt.Errorf("head bucket %q: unexpected status %s", bucket, resp.Status)
	}

	resp, err = c.do(ctx, http.MethodPut, bucket, "", "", nil)
	if err != nil {
		return fmt.Errorf("create bucket %q: %w", bucket, err)
	}
	defer drainClose(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusConflict:
		return nil
	default:
		return fmt.Errorf("create bucket %q: status %s: %s", bucket, resp.Status, errExcerpt(resp.Body))
	}
}

// putObject stores data under bucket/key.
func (c *s3Client) putObject(ctx context.Context, bucket, key, contentType string, data []byte) error {
	resp, err := c.do(ctx, http.MethodPut, bucket, key, contentType, data)
	if err != nil {
		return fmt.Errorf("put object %q: %w", key, err)
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("put object %q: status %s: %s", key, resp.Status, errExcerpt(resp.Body))
	}
	return nil
}

// getObject fetches bucket/key, mapping 404 to ErrNotFound.
func (c *s3Client) getObject(ctx context.Context, bucket, key string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, bucket, key, "", nil)
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	defer drainClose(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("get object %q: read body: %w", key, err)
		}
		return data, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("get object %q: %w", key, ErrNotFound)
	default:
		return nil, fmt.Errorf("get object %q: status %s: %s", key, resp.Status, errExcerpt(resp.Body))
	}
}

// do builds, signs, and sends one path-style S3 request. key may be empty
// for bucket-level operations.
func (c *s3Client) do(ctx context.Context, method, bucket, key, contentType string, body []byte) (*http.Response, error) {
	scheme := "http"
	if c.useSSL {
		scheme = "https"
	}
	canonicalPath := "/" + uriEncode(bucket)
	decodedPath := "/" + bucket
	if key != "" {
		canonicalPath += "/" + uriEncodePath(key)
		decodedPath += "/" + key
	}

	req, err := http.NewRequestWithContext(ctx, method, scheme+"://"+c.endpoint+"/", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Set Path and a matching RawPath so the wire path is byte-identical to
	// the canonical path that gets signed.
	req.URL.Path = decodedPath
	req.URL.RawPath = canonicalPath
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.sign(req, canonicalPath, body)
	return c.httpc.Do(req)
}

// sign computes the AWS SigV4 authorization for req in place. canonicalPath
// must be the URI-encoded path exactly as it will appear on the wire.
func (c *s3Client) sign(req *http.Request, canonicalPath string, body []byte) {
	now := c.now().UTC()
	amzDate := now.Format(amzDateFormat)
	dateStamp := now.Format(amzDateStamp)
	payloadHash := hexSHA256(body)

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	signed := map[string]string{
		"host":                 req.URL.Host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		signed["content-type"] = ct
	}
	names := make([]string, 0, len(signed))
	for n := range signed {
		names = append(names, n)
	}
	sort.Strings(names)

	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(strings.TrimSpace(signed[n]))
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalPath,
		req.URL.RawQuery, // no query parameters are used today
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, s3Region, s3Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		sigAlgorithm,
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+c.secretKey), dateStamp)
	key = hmacSHA256(key, s3Region)
	key = hmacSHA256(key, s3Service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigAlgorithm, c.accessKey, scope, signedHeaders, signature))
}

// uriEncodePath AWS-URI-encodes an object key, keeping "/" separators.
func uriEncodePath(s string) string {
	segs := strings.Split(s, "/")
	for i, seg := range segs {
		segs[i] = uriEncode(seg)
	}
	return strings.Join(segs, "/")
}

// uriEncode percent-encodes everything except the RFC 3986 unreserved set,
// per the AWS SigV4 UriEncode rules.
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

// hexSHA256 returns the lowercase hex SHA-256 of b.
func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hmacSHA256 returns HMAC-SHA256(key, msg).
func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// drainClose discards (a bounded amount of) the body and closes it so the
// HTTP connection can be reused.
func drainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 4096))
	_ = rc.Close()
}

// errExcerpt reads a short excerpt of an error response body for messages.
func errExcerpt(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, errBodyLimit))
	return strings.TrimSpace(string(b))
}
