package s3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	"github.com/asker/asker/platform/tenancy"
)

// fakeObject is one object the fake S3 server lists and serves.
type fakeObject struct {
	key          string
	body         string
	contentType  string
	lastModified time.Time
	storageClass string
}

// fakeS3 is a tiny in-memory S3-compatible HTTP server: it answers
// ListObjectsV2 (a single page, with optional truncation by maxPerPage) and
// GetObject for the seeded objects, so the connector can be driven end to end
// without a live API. It is deliberately minimal — just enough surface for the
// connector's calls.
type fakeS3 struct {
	bucket     string
	objects    []fakeObject
	maxPerPage int // 0 = single page

	listCalls int
	getCalls  int
	failList  bool
}

func newFakeS3(t *testing.T, bucket string, objs []fakeObject) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{bucket: bucket, objects: objs}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("list-type") == "2" {
		f.serveList(w, r)
		return
	}
	f.serveGet(w, r)
}

func (f *fakeS3) serveList(w http.ResponseWriter, r *http.Request) {
	f.listCalls++
	if f.failList {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>denied</Message></Error>`)
		return
	}
	q := r.URL.Query()
	prefix := q.Get("prefix")
	after := q.Get("start-after")
	if ct := q.Get("continuation-token"); ct != "" {
		after = ct // our tokens are the last key emitted
	}

	var page []fakeObject
	for _, o := range f.objects {
		if prefix != "" && !strings.HasPrefix(o.key, prefix) {
			continue
		}
		if after != "" && o.key <= after {
			continue
		}
		page = append(page, o)
	}

	truncated := false
	var next string
	if f.maxPerPage > 0 && len(page) > f.maxPerPage {
		page = page[:f.maxPerPage]
		truncated = true
		next = page[len(page)-1].key
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	fmt.Fprintf(&b, "<Name>%s</Name><Prefix>%s</Prefix><MaxKeys>1000</MaxKeys>", f.bucket, prefix)
	fmt.Fprintf(&b, "<IsTruncated>%t</IsTruncated>", truncated)
	if truncated {
		fmt.Fprintf(&b, "<NextContinuationToken>%s</NextContinuationToken>", next)
	}
	for _, o := range page {
		sc := o.storageClass
		if sc == "" {
			sc = "STANDARD"
		}
		fmt.Fprintf(&b, "<Contents><Key>%s</Key><LastModified>%s</LastModified><ETag>&quot;etag-%s&quot;</ETag><Size>%d</Size><StorageClass>%s</StorageClass></Contents>",
			o.key, o.lastModified.UTC().Format("2006-01-02T15:04:05.000Z"), o.key, len(o.body), sc)
	}
	b.WriteString("</ListBucketResult>")
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, b.String())
}

func (f *fakeS3) serveGet(w http.ResponseWriter, r *http.Request) {
	f.getCalls++
	key := strings.TrimPrefix(r.URL.Path, "/"+f.bucket+"/")
	for _, o := range f.objects {
		if o.key == key {
			ct := o.contentType
			if ct == "" {
				ct = "application/octet-stream"
			}
			w.Header().Set("Content-Type", ct)
			w.Header().Set("ETag", `"etag-`+o.key+`"`)
			w.Header().Set("Last-Modified", o.lastModified.UTC().Format(http.TimeFormat))
			_, _ = io.WriteString(w, o.body)
			return
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code></Error>`)
}

// testConfig builds an sdk.Config pointing endpoint at srvURL with the given
// prefix and a checkpoint sink.
func testConfig(t *testing.T, srvURL, prefix string, ckpt sdk.Checkpoint) sdk.Config {
	t.Helper()
	tcx, err := tenancy.FromClaims(map[string]any{"tenant_id": "tenant-x", "sub": "u"})
	if err != nil {
		t.Fatalf("tenancy.FromClaims: %v", err)
	}
	cfgJSON, _ := json.Marshal(map[string]any{"endpoint": srvURL, "bucket": "my-bucket", "prefix": prefix})
	if ckpt == nil {
		ckpt = sdk.NopCheckpoint
	}
	return sdk.Config{
		Tenant:     tcx,
		InstanceID: "inst-1",
		ConfigJSON: cfgJSON,
		Token:      []byte("AKID:SECRET"),
		Checkpoint: ckpt,
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	_, srv := newFakeS3(t, "my-bucket", []fakeObject{{key: "a.txt", body: "hi", lastModified: time.Now()}})
	c := New()

	t.Run("ok", func(t *testing.T) {
		if err := c.Validate(context.Background(), testConfig(t, srv.URL, "", nil)); err != nil {
			t.Errorf("Validate: %v", err)
		}
	})

	t.Run("no token is config-only", func(t *testing.T) {
		cfg := testConfig(t, srv.URL, "", nil)
		cfg.Token = nil
		if err := c.Validate(context.Background(), cfg); err != nil {
			t.Errorf("Validate without token: %v", err)
		}
	})

	t.Run("bad config", func(t *testing.T) {
		cfg := testConfig(t, srv.URL, "", nil)
		cfg.ConfigJSON = []byte(`{"bucket":"b"}`) // missing endpoint
		if err := c.Validate(context.Background(), cfg); err == nil {
			t.Error("expected error for missing endpoint")
		}
	})

	t.Run("bad token does not leak", func(t *testing.T) {
		cfg := testConfig(t, srv.URL, "", nil)
		cfg.Token = []byte("no-separator")
		err := c.Validate(context.Background(), cfg)
		if err == nil {
			t.Fatal("expected error for malformed token")
		}
		if strings.Contains(err.Error(), "no-separator") {
			t.Errorf("error leaked the token: %v", err)
		}
	})
}

func TestValidateBucketUnreachable(t *testing.T) {
	t.Parallel()
	f, srv := newFakeS3(t, "my-bucket", nil)
	f.failList = true
	if err := New().Validate(context.Background(), testConfig(t, srv.URL, "", nil)); err == nil {
		t.Error("expected error when the bucket is unreachable")
	}
}

func TestValidateEmptyBucketIsReachable(t *testing.T) {
	t.Parallel()
	// An empty bucket lists zero objects but is a valid, reachable bucket.
	_, srv := newFakeS3(t, "my-bucket", nil)
	if err := New().Validate(context.Background(), testConfig(t, srv.URL, "", nil)); err != nil {
		t.Errorf("Validate on empty bucket: %v", err)
	}
}

func TestFullSyncCheckpointsPeriodically(t *testing.T) {
	t.Parallel()
	// More objects than checkpointEvery, across several list pages, so the
	// periodic mid-backfill checkpoint fires and each checkpoint is a resumable
	// cursor (non-empty LastKey).
	lm := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	const n = checkpointEvery*2 + 5
	objs := make([]fakeObject, n)
	for i := range objs {
		// Binary objects so no body GET is needed; zero-padded keys list sorted.
		objs[i] = fakeObject{
			key:          fmt.Sprintf("docs/obj-%04d.bin", i),
			body:         "x",
			contentType:  "application/octet-stream",
			lastModified: lm.Add(time.Duration(i) * time.Minute),
		}
	}
	f, srv := newFakeS3(t, "my-bucket", objs)
	f.maxPerPage = 100 // force several list pages

	var checkpoints []sdk.Cursor
	ckpt := func(_ context.Context, cur sdk.Cursor) error {
		checkpoints = append(checkpoints, cur)
		return nil
	}
	c := New()
	var rec connectortest.EmitRecorder
	cur, err := c.FullSync(context.Background(), testConfig(t, srv.URL, "docs/", ckpt), rec.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if got := len(rec.Docs()); got != n {
		t.Fatalf("emitted %d docs, want %d", got, n)
	}
	// At least two periodic checkpoints (n > 2*checkpointEvery) plus the final.
	if len(checkpoints) < 3 {
		t.Errorf("expected >=3 checkpoints, got %d", len(checkpoints))
	}
	// A mid-backfill checkpoint is resumable (carries a LastKey).
	mid, err := parseCursor(checkpoints[0])
	if err != nil {
		t.Fatalf("parseCursor(checkpoint): %v", err)
	}
	if mid.LastKey == "" {
		t.Error("mid-backfill checkpoint has empty LastKey; not resumable")
	}

	// The returned (final) cursor carries the full keyset and max LastModified.
	st, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor: %v", err)
	}
	if len(st.Keys) != n {
		t.Errorf("cursor keys = %d, want %d", len(st.Keys), n)
	}
	if st.LastKey != "" {
		t.Error("final cursor LastKey is non-empty; the pass completed")
	}
	if f.getCalls != 0 {
		t.Errorf("getCalls = %d, want 0 (all binary objects)", f.getCalls)
	}
}

func TestIncrementalDetectsChangeAndDeletion(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Seed the prior pass's cursor: two keys, max modified = base.
	prior := completedCursor(base, []string{"docs/keep.txt", "docs/gone.txt"})

	// Now keep.txt is newer (change) and gone.txt is absent (deletion).
	objs := []fakeObject{
		{key: "docs/keep.txt", body: "updated", contentType: "text/plain", lastModified: base.Add(48 * time.Hour)},
	}
	_, srv := newFakeS3(t, "my-bucket", objs)

	c := New()
	var rec connectortest.EmitRecorder
	cur, err := c.IncrementalSync(context.Background(), testConfig(t, srv.URL, "docs/", nil), prior, rec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	var live, tomb int
	for _, d := range rec.Docs() {
		if d.GetTombstone().GetDeleted() {
			tomb++
			if d.GetSourceNativeId() != "my-bucket/docs/gone.txt" {
				t.Errorf("tombstone for %q, want my-bucket/docs/gone.txt", d.GetSourceNativeId())
			}
		} else {
			live++
			if d.GetSourceNativeId() != "my-bucket/docs/keep.txt" {
				t.Errorf("upsert for %q, want my-bucket/docs/keep.txt", d.GetSourceNativeId())
			}
		}
	}
	if live != 1 || tomb != 1 {
		t.Fatalf("emitted live=%d tomb=%d, want 1 and 1", live, tomb)
	}
	st, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor: %v", err)
	}
	if !st.maxModifiedTime().Equal(base.Add(48 * time.Hour)) {
		t.Errorf("advanced cursor max = %v, want %v", st.maxModifiedTime(), base.Add(48*time.Hour))
	}
}

func TestIncrementalResumesMidBackfillCheckpoint(t *testing.T) {
	t.Parallel()
	lm := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	objs := []fakeObject{
		{key: "docs/a.txt", body: "a", contentType: "text/plain", lastModified: lm},
		{key: "docs/b.txt", body: "b", contentType: "text/plain", lastModified: lm},
		{key: "docs/c.txt", body: "c", contentType: "text/plain", lastModified: lm},
	}
	_, srv := newFakeS3(t, "my-bucket", objs)

	// A mid-backfill checkpoint cursor (LastKey set) resumes the listing past
	// docs/a.txt, so only b and c are emitted.
	resume := checkpointCursor("docs/a.txt", lm, []string{"docs/a.txt"})
	c := New()
	var rec connectortest.EmitRecorder
	if _, err := c.IncrementalSync(context.Background(), testConfig(t, srv.URL, "docs/", nil), resume, rec.Emit); err != nil {
		t.Fatalf("IncrementalSync resume: %v", err)
	}
	if got := len(rec.Docs()); got != 2 {
		t.Errorf("resumed backfill emitted %d docs, want 2 (b, c)", got)
	}
}

func TestIncrementalStaleCursor(t *testing.T) {
	t.Parallel()
	_, srv := newFakeS3(t, "my-bucket", nil)
	var rec connectortest.EmitRecorder
	_, err := New().IncrementalSync(context.Background(), testConfig(t, srv.URL, "", nil), sdk.Cursor("garbage"), rec.Emit)
	if !errors.Is(err, sdk.ErrCursorExpired) {
		t.Errorf("err = %v, want ErrCursorExpired", err)
	}
}

func TestListErrorAborts(t *testing.T) {
	t.Parallel()
	f, srv := newFakeS3(t, "my-bucket", nil)
	f.failList = true
	var rec connectortest.EmitRecorder
	if _, err := New().FullSync(context.Background(), testConfig(t, srv.URL, "", nil), rec.Emit); err == nil {
		t.Error("expected FullSync to fail on a list error")
	}
}

func TestFullSyncCapturesTextBody(t *testing.T) {
	t.Parallel()
	_, srv := newFakeS3(t, "my-bucket", []fakeObject{
		{key: "docs/readme.txt", body: "hello body", contentType: "text/plain", lastModified: time.Now()},
	})
	var rec connectortest.EmitRecorder
	if _, err := New().FullSync(context.Background(), testConfig(t, srv.URL, "docs/", nil), rec.Emit); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	docs := rec.Docs()
	if len(docs) != 1 {
		t.Fatalf("emitted %d docs, want 1", len(docs))
	}
	if got, want := docs[0].GetBodyText(), "hello body"; got != want {
		t.Errorf("body_text = %q, want %q", got, want)
	}
	if got := docs[0].GetMetadata()["content_type"]; got != "text/plain" {
		t.Errorf("content_type = %q, want text/plain (from GET response)", got)
	}
}

// getFailFakeS3 lists a text object but fails its GET, so the connector should
// degrade to a metadata-only document rather than fail the whole sync.
func TestBodyFetchErrorDegradesToMetadataOnly(t *testing.T) {
	t.Parallel()
	lm := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>my-bucket</Name><Prefix>docs/</Prefix><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>docs/x.txt</Key><LastModified>%s</LastModified><ETag>&quot;e1&quot;</ETag><Size>10</Size><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>`,
				lm.UTC().Format("2006-01-02T15:04:05.000Z"))
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>InternalError</Code></Error>`)
	}))
	t.Cleanup(srv.Close)

	var rec connectortest.EmitRecorder
	if _, err := New().FullSync(context.Background(), testConfig(t, srv.URL, "docs/", nil), rec.Emit); err != nil {
		t.Fatalf("FullSync should not fail on a body-fetch error: %v", err)
	}
	docs := rec.Docs()
	if len(docs) != 1 {
		t.Fatalf("emitted %d docs, want 1", len(docs))
	}
	if docs[0].GetBodyText() != "" {
		t.Errorf("body_text = %q, want empty after a failed body fetch", docs[0].GetBodyText())
	}
}
