package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAccessKey = "test-access"
	testSecretKey = "test-secret"
)

// streamingPayloadSha is the X-Amz-Content-Sha256 value minio-go sets when
// it uploads with the SigV4 streaming (aws-chunked) signature, which it uses
// for PUT object over plain HTTP.
const streamingPayloadSha = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"

// fakeS3 is a minimal in-memory S3-compatible server for the path-style
// requests minio-go issues for the three objectAPI operations: the bucket
// location query, HEAD/PUT bucket, and PUT/GET object (decoding aws-chunked
// upload bodies back to the raw payload).
type fakeS3 struct {
	t  *testing.T
	mu sync.Mutex

	buckets      map[string]bool
	objects      map[string][]byte
	contentTypes map[string]string // "<bucket>/<key>" -> Content-Type from the PUT

	bucketPuts  int
	headMissing bool   // force HEAD bucket -> 404 (simulates a creation race)
	failPut     string // S3 error code to return for object PUTs, "" = succeed
	failGet     string // S3 error code to return for GETs, "" = normal
}

func newFakeS3(t *testing.T) *fakeS3 {
	return &fakeS3{
		t:            t,
		buckets:      make(map[string]bool),
		objects:      make(map[string][]byte),
		contentTypes: make(map[string]string),
	}
}

func (s *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if auth := r.Header.Get("Authorization"); !strings.Contains(auth, "Credential="+testAccessKey+"/") {
		s.t.Errorf("fakeS3: Authorization %q does not carry access key %q", auth, testAccessKey)
	}
	body := s.readBody(r)

	path := strings.TrimSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	bucket, key, _ := strings.Cut(path, "/")
	if dec, err := url.PathUnescape(key); err == nil {
		key = dec
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Query().Has("location"):
		if !s.buckets[bucket] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`)
	case r.Method == http.MethodHead && key == "":
		if s.buckets[bucket] && !s.headMissing {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	case r.Method == http.MethodPut && key == "":
		s.bucketPuts++
		if s.buckets[bucket] {
			writeS3Error(w, http.StatusConflict, "BucketAlreadyOwnedByYou")
			return
		}
		s.buckets[bucket] = true
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut:
		if s.failPut != "" {
			writeS3Error(w, http.StatusForbidden, s.failPut)
			return
		}
		s.objects[bucket+"/"+key] = bytes.Clone(body)
		s.contentTypes[bucket+"/"+key] = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet:
		if s.failGet != "" {
			writeS3Error(w, http.StatusForbidden, s.failGet)
			return
		}
		data, ok := s.objects[bucket+"/"+key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if ct := s.contentTypes[bucket+"/"+key]; ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		_, _ = w.Write(data)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// readBody reads the request payload, stripping the aws-chunked framing
// minio-go's streaming SigV4 uploads wrap around the raw object bytes.
func (s *fakeS3) readBody(r *http.Request) []byte {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("fakeS3: read body: %v", err)
		return nil
	}
	if r.Header.Get("X-Amz-Content-Sha256") != streamingPayloadSha {
		return body
	}
	// Each chunk is "<hex-size>;chunk-signature=<sig>\r\n<data>\r\n"; a
	// zero-size chunk terminates the stream.
	var out []byte
	rest := body
	for {
		header, after, ok := bytes.Cut(rest, []byte("\r\n"))
		if !ok {
			s.t.Errorf("fakeS3: aws-chunked body missing chunk header terminator")
			return nil
		}
		sizeHex, _, _ := strings.Cut(string(header), ";")
		n, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil {
			s.t.Errorf("fakeS3: bad aws-chunked size %q: %v", sizeHex, err)
			return nil
		}
		if n == 0 {
			return out
		}
		if int64(len(after)) < n+2 {
			s.t.Errorf("fakeS3: truncated aws-chunked chunk: want %d bytes, have %d", n+2, len(after))
			return nil
		}
		out = append(out, after[:n]...)
		rest = after[n+2:]
	}
}

// stored returns the raw bytes the fake holds for bucket/key.
func (s *fakeS3) stored(bucket, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[bucket+"/"+key]
	return data, ok
}

// writeS3Error writes an S3 XML error response with the given code.
func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

func newTestS3(t *testing.T) (*fakeS3, *minioClient) {
	t.Helper()
	srv := newFakeS3(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	endpoint := strings.TrimPrefix(ts.URL, "http://")
	c, err := newMinioClient(endpoint, testAccessKey, testSecretKey, false)
	if err != nil {
		t.Fatalf("newMinioClient: %v", err)
	}
	return srv, c
}

func TestEnsureBucketCreatesOnce(t *testing.T) {
	ctx := context.Background()
	srv, c := newTestS3(t)

	if err := c.ensureBucket(ctx, "asker-blobs"); err != nil {
		t.Fatalf("ensureBucket (missing): %v", err)
	}
	if !srv.buckets["asker-blobs"] {
		t.Fatal("bucket was not created")
	}
	if err := c.ensureBucket(ctx, "asker-blobs"); err != nil {
		t.Fatalf("ensureBucket (existing): %v", err)
	}
	if srv.bucketPuts != 1 {
		t.Errorf("bucket PUTs = %d, want 1 (second ensure must HEAD-skip)", srv.bucketPuts)
	}
}

func TestEnsureBucketConflictIsSuccess(t *testing.T) {
	// Simulate losing a creation race: HEAD reports missing, but the PUT
	// hits an already-existing bucket and returns 409
	// BucketAlreadyOwnedByYou — still a success.
	srv, c := newTestS3(t)
	srv.mu.Lock()
	srv.buckets["bkt"] = true
	srv.headMissing = true
	srv.mu.Unlock()
	if err := c.ensureBucket(context.Background(), "bkt"); err != nil {
		t.Fatalf("ensureBucket with PUT 409: %v", err)
	}
}

func TestPutGetObjectRoundTrip(t *testing.T) {
	ctx := context.Background()
	srv, c := newTestS3(t)
	if err := c.ensureBucket(ctx, "bkt"); err != nil {
		t.Fatalf("ensureBucket: %v", err)
	}

	data := []byte{0x00, 0x01, 0xfe, 0xff, 'h', 'i'}
	key := "tenant-a/upload/deadbeef"
	if err := c.putObject(ctx, "bkt", key, "application/octet-stream", data); err != nil {
		t.Fatalf("putObject: %v", err)
	}
	if stored, ok := srv.stored("bkt", key); !ok || !bytes.Equal(stored, data) {
		t.Errorf("server stored %x, want raw payload %x", stored, data)
	}
	got, contentType, err := c.getObject(ctx, "bkt", key)
	if err != nil {
		t.Fatalf("getObject: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("getObject = %x, want %x", got, data)
	}
	if contentType != "application/octet-stream" {
		t.Errorf("getObject content-type = %q, want application/octet-stream", contentType)
	}
}

func TestPutObjectKeyNeedsEncoding(t *testing.T) {
	// Keys with spaces, '+', and unicode must round-trip unchanged.
	ctx := context.Background()
	_, c := newTestS3(t)
	if err := c.ensureBucket(ctx, "bkt"); err != nil {
		t.Fatalf("ensureBucket: %v", err)
	}
	key := "tenant-a/dir name/file+x é.txt"
	data := []byte("payload")
	if err := c.putObject(ctx, "bkt", key, "text/plain", data); err != nil {
		t.Fatalf("putObject: %v", err)
	}
	got, _, err := c.getObject(ctx, "bkt", key)
	if err != nil {
		t.Fatalf("getObject: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("round trip = %q, want %q", got, data)
	}
}

func TestGetObjectNotFound(t *testing.T) {
	ctx := context.Background()
	_, c := newTestS3(t)
	if err := c.ensureBucket(ctx, "bkt"); err != nil {
		t.Fatalf("ensureBucket: %v", err)
	}
	if _, _, err := c.getObject(ctx, "bkt", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("getObject(missing): err = %v, want ErrNotFound", err)
	}
}

func TestErrorStatusesSurface(t *testing.T) {
	ctx := context.Background()
	srv, c := newTestS3(t)
	if err := c.ensureBucket(ctx, "bkt"); err != nil {
		t.Fatalf("ensureBucket: %v", err)
	}

	srv.mu.Lock()
	srv.failPut = "AccessDenied"
	srv.mu.Unlock()
	if err := c.putObject(ctx, "bkt", "k", "", []byte("x")); err == nil {
		t.Error("putObject succeeded despite 403")
	} else if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("putObject error %q does not carry the S3 error code", err)
	}

	srv.mu.Lock()
	srv.failGet = "AccessDenied"
	srv.mu.Unlock()
	if _, _, err := c.getObject(ctx, "bkt", "k"); err == nil {
		t.Error("getObject succeeded despite 403")
	} else if errors.Is(err, ErrNotFound) {
		t.Errorf("getObject 403 mapped to ErrNotFound: %v", err)
	}
}

func TestEnsureBucketUnexpectedHeadStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(ts.Close)
	c, err := newMinioClient(strings.TrimPrefix(ts.URL, "http://"), testAccessKey, testSecretKey, false)
	if err != nil {
		t.Fatalf("newMinioClient: %v", err)
	}
	if err := c.ensureBucket(context.Background(), "bkt"); err == nil {
		t.Fatal("ensureBucket succeeded despite HEAD 403")
	}
}

func TestEnsureBucketCreateErrorStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			writeS3Error(w, http.StatusForbidden, "AccessDenied")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)
	c, err := newMinioClient(strings.TrimPrefix(ts.URL, "http://"), testAccessKey, testSecretKey, false)
	if err != nil {
		t.Fatalf("newMinioClient: %v", err)
	}
	if err := c.ensureBucket(context.Background(), "bkt"); err == nil {
		t.Fatal("ensureBucket succeeded despite PUT 403")
	}
}

func TestUnreachableEndpoint(t *testing.T) {
	c, err := newMinioClient("127.0.0.1:1", testAccessKey, testSecretKey, false)
	if err != nil {
		t.Fatalf("newMinioClient: %v", err)
	}
	// minio-go retries transport errors with backoff; bound the test with a
	// deadline the retry loop honors.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := c.ensureBucket(ctx, "bkt"); err == nil {
		t.Error("ensureBucket succeeded against closed port")
	}
	if err := c.putObject(ctx, "bkt", "k", "", nil); err == nil {
		t.Error("putObject succeeded against closed port")
	}
	if _, _, err := c.getObject(ctx, "bkt", "k"); err == nil {
		t.Error("getObject succeeded against closed port")
	}
}

// TestNewEndToEnd exercises the exported New (scheme-tolerant endpoint)
// against the fake S3 server with real encryption.
func TestNewEndToEnd(t *testing.T) {
	ctx := context.Background()
	srv := newFakeS3(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	cfg := Config{
		Endpoint:  ts.URL, // http://host:port — New must strip the scheme
		AccessKey: testAccessKey,
		SecretKey: testSecretKey,
		Bucket:    "asker-blobs",
		UseSSL:    true, // overridden to false by the http:// prefix
	}
	store, err := New(ctx, cfg, testCipher(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !srv.buckets["asker-blobs"] {
		t.Fatal("New did not ensure the bucket")
	}

	tc := testTenant(t, "tenant-a")
	plaintext := []byte("end to end body")
	ref, err := store.Put(ctx, tc, "upload/e2e", "text/plain", plaintext)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if stored, _ := srv.stored("asker-blobs", "tenant-a/upload/e2e"); bytes.Contains(stored, plaintext) {
		t.Error("server stored plaintext; payload must be encrypted before the wire")
	}
	got, err := store.Get(ctx, tc, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Get = %q, want %q", got, plaintext)
	}
}
