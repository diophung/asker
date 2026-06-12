package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

const (
	testAccessKey = "test-access"
	testSecretKey = "test-secret"
)

// fakeS3 is a minimal in-memory S3-compatible server for path-style
// requests. Every request's SigV4 signature is re-verified from the bytes
// actually received, so a signing/encoding mismatch fails the test the same
// way a real MinIO would reject it.
type fakeS3 struct {
	t  *testing.T
	mu sync.Mutex

	buckets map[string]bool
	objects map[string][]byte

	bucketPuts  int
	headMissing bool // force HEAD bucket -> 404 (simulates a creation race)
	failPut     int  // status to return for object PUTs, 0 = succeed
	failGet     int  // status to return for GETs, 0 = normal
}

func newFakeS3(t *testing.T) *fakeS3 {
	return &fakeS3{
		t:       t,
		buckets: make(map[string]bool),
		objects: make(map[string][]byte),
	}
}

func (s *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("fakeS3: read body: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	s.verifySigV4(r, body)

	path := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	bucket, key, _ := strings.Cut(path, "/")
	if dec, err := unescapePath(key); err == nil {
		key = dec
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodHead && key == "":
		if s.buckets[bucket] && !s.headMissing {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	case r.Method == http.MethodPut && key == "":
		s.bucketPuts++
		if s.buckets[bucket] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		s.buckets[bucket] = true
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut:
		if s.failPut != 0 {
			w.WriteHeader(s.failPut)
			_, _ = io.WriteString(w, "<Error><Code>InternalError</Code></Error>")
			return
		}
		s.objects[bucket+"/"+key] = bytes.Clone(body)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet:
		if s.failGet != 0 {
			w.WriteHeader(s.failGet)
			_, _ = io.WriteString(w, "<Error><Code>InternalError</Code></Error>")
			return
		}
		data, ok := s.objects[bucket+"/"+key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// unescapePath decodes %XX sequences (path flavor: "+" stays "+").
func unescapePath(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", errors.New("truncated escape")
		}
		dec, err := hex.DecodeString(s[i+1 : i+3])
		if err != nil {
			return "", err
		}
		b.WriteByte(dec[0])
		i += 2
	}
	return b.String(), nil
}

var authPattern = regexp.MustCompile(
	`^AWS4-HMAC-SHA256 Credential=([^/]+)/(\d{8})/([^/]+)/s3/aws4_request, SignedHeaders=([a-z0-9;-]+), Signature=([0-9a-f]{64})$`)

// verifySigV4 recomputes the request signature from what was actually
// received on the wire and compares it with the Authorization header.
func (s *fakeS3) verifySigV4(r *http.Request, body []byte) {
	s.t.Helper()
	m := authPattern.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil {
		s.t.Errorf("fakeS3: malformed Authorization header %q", r.Header.Get("Authorization"))
		return
	}
	accessKey, dateStamp, region, signedHeaders, gotSig := m[1], m[2], m[3], m[4], m[5]
	if accessKey != testAccessKey {
		s.t.Errorf("fakeS3: access key = %q, want %q", accessKey, testAccessKey)
	}
	if hash := r.Header.Get("X-Amz-Content-Sha256"); hash != hexSHA256(body) {
		s.t.Errorf("fakeS3: x-amz-content-sha256 = %q does not hash the received body (%q)", hash, hexSHA256(body))
	}

	names := strings.Split(signedHeaders, ";")
	if !sort.StringsAreSorted(names) {
		s.t.Errorf("fakeS3: SignedHeaders %q not sorted", signedHeaders)
	}
	var canonHeaders strings.Builder
	for _, n := range names {
		v := r.Header.Get(n)
		if n == "host" {
			v = r.Host
		}
		canonHeaders.WriteString(n)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(strings.TrimSpace(v))
		canonHeaders.WriteByte('\n')
	}

	canonicalRequest := strings.Join([]string{
		r.Method,
		r.URL.EscapedPath(),
		r.URL.RawQuery,
		canonHeaders.String(),
		signedHeaders,
		r.Header.Get("X-Amz-Content-Sha256"),
	}, "\n")
	scope := strings.Join([]string{dateStamp, region, "s3", "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		r.Header.Get("X-Amz-Date"),
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+testSecretKey), dateStamp)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	wantSig := hex.EncodeToString(hmacSHA256(key, stringToSign))

	if !hmac.Equal([]byte(gotSig), []byte(wantSig)) {
		s.t.Errorf("fakeS3: signature mismatch for %s %s:\n got %s\nwant %s\ncanonical request:\n%s",
			r.Method, r.URL.EscapedPath(), gotSig, wantSig, canonicalRequest)
	}
}

func newTestS3(t *testing.T) (*fakeS3, *s3Client) {
	t.Helper()
	srv := newFakeS3(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	endpoint := strings.TrimPrefix(ts.URL, "http://")
	return srv, newS3Client(endpoint, testAccessKey, testSecretKey, false)
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
	// hits an already-existing bucket and returns 409 — still a success.
	srv, c := newTestS3(t)
	srv.mu.Lock()
	srv.buckets["b"] = true
	srv.headMissing = true
	srv.mu.Unlock()
	if err := c.ensureBucket(context.Background(), "b"); err != nil {
		t.Fatalf("ensureBucket with PUT 409: %v", err)
	}
}

func TestPutGetObjectRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, c := newTestS3(t)
	if err := c.ensureBucket(ctx, "b"); err != nil {
		t.Fatalf("ensureBucket: %v", err)
	}

	data := []byte{0x00, 0x01, 0xfe, 0xff, 'h', 'i'}
	key := "tenant-a/upload/deadbeef"
	if err := c.putObject(ctx, "b", key, "application/octet-stream", data); err != nil {
		t.Fatalf("putObject: %v", err)
	}
	got, err := c.getObject(ctx, "b", key)
	if err != nil {
		t.Fatalf("getObject: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("getObject = %x, want %x", got, data)
	}
}

func TestPutObjectKeyNeedsEncoding(t *testing.T) {
	// Keys with spaces, '+', and unicode must be signed exactly as sent.
	ctx := context.Background()
	_, c := newTestS3(t)
	if err := c.ensureBucket(ctx, "b"); err != nil {
		t.Fatalf("ensureBucket: %v", err)
	}
	key := "tenant-a/dir name/file+x é.txt"
	data := []byte("payload")
	if err := c.putObject(ctx, "b", key, "text/plain", data); err != nil {
		t.Fatalf("putObject: %v", err)
	}
	got, err := c.getObject(ctx, "b", key)
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
	if err := c.ensureBucket(ctx, "b"); err != nil {
		t.Fatalf("ensureBucket: %v", err)
	}
	if _, err := c.getObject(ctx, "b", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("getObject(missing): err = %v, want ErrNotFound", err)
	}
}

func TestErrorStatusesSurface(t *testing.T) {
	ctx := context.Background()
	srv, c := newTestS3(t)
	if err := c.ensureBucket(ctx, "b"); err != nil {
		t.Fatalf("ensureBucket: %v", err)
	}

	srv.failPut = http.StatusInternalServerError
	if err := c.putObject(ctx, "b", "k", "", []byte("x")); err == nil {
		t.Error("putObject succeeded despite 500")
	} else if !strings.Contains(err.Error(), "InternalError") {
		t.Errorf("putObject error %q does not carry the response excerpt", err)
	}

	srv.failGet = http.StatusServiceUnavailable
	if _, err := c.getObject(ctx, "b", "k"); err == nil {
		t.Error("getObject succeeded despite 503")
	} else if errors.Is(err, ErrNotFound) {
		t.Errorf("getObject 503 mapped to ErrNotFound: %v", err)
	}
}

func TestEnsureBucketUnexpectedHeadStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(ts.Close)
	c := newS3Client(strings.TrimPrefix(ts.URL, "http://"), testAccessKey, testSecretKey, false)
	if err := c.ensureBucket(context.Background(), "b"); err == nil {
		t.Fatal("ensureBucket succeeded despite HEAD 403")
	}
}

func TestEnsureBucketCreateErrorStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	c := newS3Client(strings.TrimPrefix(ts.URL, "http://"), testAccessKey, testSecretKey, false)
	if err := c.ensureBucket(context.Background(), "b"); err == nil {
		t.Fatal("ensureBucket succeeded despite PUT 500")
	}
}

func TestUnreachableEndpoint(t *testing.T) {
	c := newS3Client("127.0.0.1:1", testAccessKey, testSecretKey, false)
	ctx := context.Background()
	if err := c.ensureBucket(ctx, "b"); err == nil {
		t.Error("ensureBucket succeeded against closed port")
	}
	if err := c.putObject(ctx, "b", "k", "", nil); err == nil {
		t.Error("putObject succeeded against closed port")
	}
	if _, err := c.getObject(ctx, "b", "k"); err == nil {
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
	if stored := srv.objects["asker-blobs/tenant-a/upload/e2e"]; bytes.Contains(stored, plaintext) {
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

func TestUriEncode(t *testing.T) {
	for in, want := range map[string]string{
		"abc-123_x.y~z":  "abc-123_x.y~z",
		"a b":            "a%20b",
		"a+b":            "a%2Bb",
		"a/b":            "a%2Fb", // uriEncode escapes "/"; uriEncodePath keeps it
		"é":              "%C3%A9",
		"semi;colon=eq?": "semi%3Bcolon%3Deq%3F",
	} {
		if got := uriEncode(in); got != want {
			t.Errorf("uriEncode(%q) = %q, want %q", in, got, want)
		}
	}
	if got, want := uriEncodePath("a b/c+d"), "a%20b/c%2Bd"; got != want {
		t.Errorf("uriEncodePath = %q, want %q", got, want)
	}
}
