package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/platform/kafkautil"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// stubUpload returns an UploadFunc that returns a clone of doc, or fails
// when doc is nil.
func stubUpload(doc *askerv1.Document) UploadFunc {
	return func(context.Context, tenancy.Context, io.Reader, string, string, string, int64) (*askerv1.Document, error) {
		if doc == nil {
			return nil, errors.New("stub upload failure")
		}
		return proto.Clone(doc).(*askerv1.Document), nil
	}
}

// newAPIRig builds an httpAPI whose scheduler is NOT running; tests inject
// workers directly to control the webhook routing table.
func newAPIRig(t *testing.T, conn *fakeConnector, up UploadFunc) (*rig, *httpAPI) {
	t.Helper()
	r := newRig(t, conn, nil)
	api := &httpAPI{
		cp:       r.sch.cp,
		registry: r.sch.registry,
		sched:    r.sch,
		emit:     r.sch.emit,
		upload:   up,
		logger:   testLogger(),
	}
	return r, api
}

// injectWorker registers a worker in the scheduler's table without running
// its loop.
func injectWorker(t *testing.T, r *rig, id, tenant, connectorID string) *worker {
	t.Helper()
	w := &worker{instanceID: id, cancel: func() {}, wake: make(chan struct{}, 1)}
	w.snap = instanceSnap{
		tenant: mustTenant(t, tenant),
		inst: &controlplanev1.ConnectorInstance{
			Id:          id,
			ConnectorId: connectorID,
			ConfigJson:  []byte(`{}`),
			Status:      controlplanev1.ConnectorStatus_ACTIVE,
		},
	}
	r.sch.mu.Lock()
	r.sch.workers[id] = w
	r.sch.mu.Unlock()
	return w
}

func doRequest(api *httpAPI, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, req)
	return rec
}

func TestWebhookRouting(t *testing.T) {
	t.Run("accepted triggers immediate sync", func(t *testing.T) {
		conn := &fakeConnector{id: "gmail"}
		var gotBody string
		conn.webhookFn = func(ctx context.Context, cfg sdk.Config, r *http.Request, emit sdk.Emit) error {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			return emit(ctx, testDoc(tenantA, "gmail:hooked"))
		}
		r, api := newAPIRig(t, conn, stubUpload(nil))
		w := injectWorker(t, r, instGmail, tenantA, "gmail")

		rec := doRequest(api, httptest.NewRequest(http.MethodPost, "/webhooks/gmail/"+instGmail, strings.NewReader(`{"historyId":42}`)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d (body %s), want 202", rec.Code, rec.Body)
		}
		if gotBody != `{"historyId":42}` {
			t.Errorf("connector saw body %q", gotBody)
		}
		if len(w.wake) != 1 {
			t.Error("webhook did not queue an immediate sync")
		}
		docs := r.prod.docs()
		if len(docs) != 1 || docs[0].doc.GetDocId() != "gmail:hooked" {
			t.Fatalf("produced docs = %v, want the webhook-emitted doc", docs)
		}
		if docs[0].doc.GetTs().GetIngested() == nil {
			t.Error("webhook-emitted doc missing ts.ingested")
		}
	})

	t.Run("unknown instance is 404", func(t *testing.T) {
		conn := &fakeConnector{id: "gmail"}
		_, api := newAPIRig(t, conn, stubUpload(nil))
		rec := doRequest(api, httptest.NewRequest(http.MethodPost, "/webhooks/gmail/nope", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("connector mismatch is 404", func(t *testing.T) {
		conn := &fakeConnector{id: "gmail"}
		r, api := newAPIRig(t, conn, stubUpload(nil))
		injectWorker(t, r, instGmail, tenantA, "gmail")
		rec := doRequest(api, httptest.NewRequest(http.MethodPost, "/webhooks/slack/"+instGmail, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
		if _, _, hooks := conn.counts(); hooks != 0 {
			t.Error("HandleWebhook ran despite connector mismatch")
		}
	})

	t.Run("unsupported webhook is 404", func(t *testing.T) {
		conn := &fakeConnector{id: "gmail"} // webhookFn nil -> ErrWebhookUnsupported
		r, api := newAPIRig(t, conn, stubUpload(nil))
		w := injectWorker(t, r, instGmail, tenantA, "gmail")
		rec := doRequest(api, httptest.NewRequest(http.MethodPost, "/webhooks/gmail/"+instGmail, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
		if len(w.wake) != 0 {
			t.Error("unsupported webhook still triggered a sync")
		}
	})

	t.Run("handler error is 500 and no trigger", func(t *testing.T) {
		conn := &fakeConnector{id: "gmail"}
		conn.webhookFn = func(context.Context, sdk.Config, *http.Request, sdk.Emit) error {
			return errors.New("bad signature")
		}
		r, api := newAPIRig(t, conn, stubUpload(nil))
		w := injectWorker(t, r, instGmail, tenantA, "gmail")
		rec := doRequest(api, httptest.NewRequest(http.MethodPost, "/webhooks/gmail/"+instGmail, nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
		if len(w.wake) != 0 {
			t.Error("failed webhook still triggered a sync")
		}
	})
}

// multipartBody builds a multipart body with a file part and an optional
// title field, returning the body and its content type.
func multipartBody(t *testing.T, filename, content, title string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if title != "" {
		if err := mw.WriteField("title", title); err != nil {
			t.Fatalf("WriteField: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// multipartUpload builds a POST /upload request with a file part and an
// optional title field.
func multipartUpload(t *testing.T, filename, content, title string) *http.Request {
	t.Helper()
	body, contentType := multipartBody(t, filename, content, title)
	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Content-Type", contentType)
	return req
}

func TestUploadRequiresValidTenantHeader(t *testing.T) {
	called := false
	up := UploadFunc(func(context.Context, tenancy.Context, io.Reader, string, string, string, int64) (*askerv1.Document, error) {
		called = true
		return nil, errors.New("unreachable")
	})
	_, api := newAPIRig(t, &fakeConnector{id: "gmail"}, up)

	for _, tc := range []struct {
		name   string
		header string
	}{
		{"missing", ""},
		{"invalid syntax", "no spaces allowed"},
		{"path traversal", ".."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := multipartUpload(t, "a.txt", "hello", "")
			if tc.header != "" {
				req.Header.Set(tenantHeader, tc.header)
			}
			rec := doRequest(api, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (fail closed)", rec.Code)
			}
		})
	}
	if called {
		t.Error("upload helper ran without a valid tenant")
	}
}

func TestUploadHappyPath(t *testing.T) {
	var got struct {
		tenant      string
		filename    string
		title       string
		contentType string
		size        int64
		content     string
	}
	up := UploadFunc(func(_ context.Context, tc tenancy.Context, file io.Reader, filename, title, contentType string, size int64) (*askerv1.Document, error) {
		b, err := io.ReadAll(file)
		if err != nil {
			return nil, err
		}
		got.tenant = string(tc.TenantID())
		got.filename, got.title, got.contentType, got.size, got.content = filename, title, contentType, size, string(b)
		return &askerv1.Document{
			TenantId:       string(tc.TenantID()),
			DocId:          "upload:doc-1",
			SourceNativeId: "doc-1",
			Type:           askerv1.DocType_FILE,
			Title:          title,
			BodyText:       string(b),
			VersionEtag:    "v1",
		}, nil
	})
	r, api := newAPIRig(t, &fakeConnector{id: "gmail"}, up)

	req := multipartUpload(t, "notes.txt", "hello world", "My Notes")
	req.Header.Set(tenantHeader, tenantA)
	rec := doRequest(api, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d (body %s), want 202", rec.Code, rec.Body)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["doc_id"] != "upload:doc-1" {
		t.Errorf(`response = %v, want {"doc_id":"upload:doc-1"}`, resp)
	}

	if got.tenant != tenantA || got.filename != "notes.txt" || got.title != "My Notes" ||
		got.content != "hello world" || got.size != int64(len("hello world")) {
		t.Errorf("upload helper got %+v", got)
	}

	docs := r.prod.docs()
	if len(docs) != 1 {
		t.Fatalf("produced %d docs, want 1", len(docs))
	}
	d := docs[0]
	if d.topic != kafkautil.TopicDocsRaw || d.tenant != tenantA {
		t.Errorf("produced to %q for tenant %q", d.topic, d.tenant)
	}
	if d.doc.GetConnectorId() != "upload" {
		t.Errorf("connector_id = %q, want upload", d.doc.GetConnectorId())
	}
	if d.doc.GetTs().GetIngested() == nil {
		t.Error("uploaded doc missing ts.ingested")
	}
}

func TestUploadErrors(t *testing.T) {
	t.Run("missing file field", func(t *testing.T) {
		_, api := newAPIRig(t, &fakeConnector{id: "gmail"}, stubUpload(testDoc(tenantA, "upload:x")))
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("title", "no file")
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/upload", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("not multipart", func(t *testing.T) {
		_, api := newAPIRig(t, &fakeConnector{id: "gmail"}, stubUpload(testDoc(tenantA, "upload:x")))
		req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("plain"))
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("upload helper failure", func(t *testing.T) {
		r, api := newAPIRig(t, &fakeConnector{id: "gmail"}, stubUpload(nil))
		req := multipartUpload(t, "a.txt", "x", "")
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
		if len(r.prod.docs()) != 0 {
			t.Error("doc produced despite upload failure")
		}
	})

	t.Run("tenant mismatch from helper is rejected at the chokepoint", func(t *testing.T) {
		// The stub builds a document claiming ANOTHER tenant; the emit
		// chokepoint must refuse to produce it.
		r, api := newAPIRig(t, &fakeConnector{id: "gmail"}, stubUpload(testDoc(tenantB, "upload:evil")))
		req := multipartUpload(t, "a.txt", "x", "")
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
		if len(r.prod.docs()) != 0 {
			t.Error("cross-tenant doc reached the producer")
		}
	})
}

func TestSyncStatus(t *testing.T) {
	conn := &fakeConnector{id: "gmail"}
	r, api := newAPIRig(t, conn, stubUpload(nil))

	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	r.cpFake.addInstance(instGmail, tenantA, "gmail", []byte(`{"label":"INBOX"}`), controlplanev1.ConnectorStatus_ACTIVE)
	r.cpFake.seedState(&controlplanev1.SyncState{
		ConnectorInstanceId: instGmail,
		Cursor:              "c9",
		Phase:               controlplanev1.SyncPhase_INCREMENTAL,
		LastSyncStarted:     timestamppb.New(started),
		DocsEmitted:         7,
	})
	// Another tenant's instance must never appear in tenant A's listing.
	r.cpFake.addInstance("22222222-2222-2222-2222-222222222222", tenantB, "upload", nil, controlplanev1.ConnectorStatus_ACTIVE)

	t.Run("requires tenant header", func(t *testing.T) {
		rec := doRequest(api, httptest.NewRequest(http.MethodGet, "/v1/sync-status", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("lists the calling tenant only", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/sync-status", nil)
		req.Header.Set(tenantHeader, tenantA)
		rec := doRequest(api, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s), want 200", rec.Code, rec.Body)
		}
		var resp struct {
			Instances []instanceStatus `json:"instances"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("response not JSON: %v (%s)", err, rec.Body)
		}
		if len(resp.Instances) != 1 {
			t.Fatalf("instances = %d, want 1 (tenant-scoped)", len(resp.Instances))
		}
		got := resp.Instances[0]
		if got.Instance.ID != instGmail || got.Instance.ConnectorID != "gmail" || got.Instance.Status != "ACTIVE" {
			t.Errorf("instance = %+v", got.Instance)
		}
		if string(got.Instance.Config) != `{"label":"INBOX"}` {
			t.Errorf("config = %s", got.Instance.Config)
		}
		if got.Sync.Phase != "INCREMENTAL" || got.Sync.DocsEmitted != 7 {
			t.Errorf("sync = %+v", got.Sync)
		}
		if got.Sync.LastSyncStarted == nil || !got.Sync.LastSyncStarted.Equal(started) {
			t.Errorf("last_sync_started = %v, want %v", got.Sync.LastSyncStarted, started)
		}
		if got.Sync.LastSyncCompleted != nil {
			t.Errorf("last_sync_completed = %v, want omitted", got.Sync.LastSyncCompleted)
		}
	})
}

func TestUnknownPathIs404JSON(t *testing.T) {
	_, api := newAPIRig(t, &fakeConnector{id: "gmail"}, stubUpload(nil))
	rec := doRequest(api, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}
