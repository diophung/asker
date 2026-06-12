package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/platform/kafkautil"
	controlplanev1 "github.com/asker/asker/platform/proto/gen/go/asker/controlplane/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

const (
	instGmail   = "11111111-1111-1111-1111-111111111111"
	webhookBase = "http://connector-hub:9300"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testDoc(tenant, docID string) *askerv1.Document {
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          docID,
		SourceNativeId: "native-" + docID,
		Type:           askerv1.DocType_EMAIL,
		Title:          "title " + docID,
		BodyText:       "body " + docID,
		VersionEtag:    "v1",
	}
}

// rig wires a scheduler against the fake control plane, a fake producer and
// one fake connector, with intervals shrunk for tests.
type rig struct {
	cpFake *fakeControlPlane
	prod   *fakeProducer
	conn   *fakeConnector
	sch    *scheduler
}

func newRig(t *testing.T, conn *fakeConnector, mutate func(*schedulerOpts)) *rig {
	t.Helper()
	f := newFakeControlPlane()
	cp, schedClient, _ := startFakeControlPlane(t, f)
	prod := &fakeProducer{}
	registry := sdk.NewRegistry()
	if err := registry.Register(conn); err != nil {
		t.Fatalf("Register: %v", err)
	}
	opts := schedulerOpts{
		cp:           cp,
		sched:        schedClient,
		registry:     registry,
		emit:         newEmitter(prod, kafkautil.TopicDocsRaw, time.Now),
		logger:       testLogger(),
		webhookBase:  webhookBase,
		syncInterval: 25 * time.Millisecond,
		tick:         15 * time.Millisecond,
		backoffBase:  10 * time.Millisecond,
		backoffCap:   50 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	return &rig{cpFake: f, prod: prod, conn: conn, sch: newScheduler(opts)}
}

// start runs the scheduler loop until test cleanup, asserting it drains.
func (r *rig) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.sch.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("scheduler did not drain after cancel")
		}
	})
}

// phases extracts the phase sequence from the fake's SetSyncState log.
func phases(ws []stateWrite) []controlplanev1.SyncPhase {
	out := make([]controlplanev1.SyncPhase, len(ws))
	for i, w := range ws {
		out[i] = w.state.GetPhase()
	}
	return out
}

func TestFullSyncHappyPathThenSteadyIncremental(t *testing.T) {
	conn := &fakeConnector{
		id: "gmail",
		fullSyncFn: func(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
			if err := emit(ctx, testDoc(tenantA, "gmail:1")); err != nil {
				return "", err
			}
			if err := emit(ctx, testDoc(tenantA, "gmail:2")); err != nil {
				return "", err
			}
			return "c1", nil
		},
	}
	r := newRig(t, conn, nil)
	r.cpFake.addInstance(instGmail, tenantA, "gmail", []byte(`{"label":"INBOX"}`), controlplanev1.ConnectorStatus_ACTIVE)
	r.cpFake.setToken(instGmail, []byte("tok-1"))
	r.start(t)

	// Backfill completes: INCREMENTAL, cursor from FullSync, docs accumulated.
	waitFor(t, 5*time.Second, func() bool {
		w, ok := r.cpFake.lastWrite()
		return ok && w.state.GetPhase() == controlplanev1.SyncPhase_INCREMENTAL &&
			w.state.GetCursor() == "c1" && w.state.GetDocsEmitted() == 2
	}, "full sync completion write")

	// Steady state: IncrementalSync resumes from the FullSync cursor.
	waitFor(t, 5*time.Second, func() bool {
		_, inc, _ := conn.counts()
		return inc >= 1
	}, "incremental sync after backfill")
	if cursors := conn.cursorsSeen(); len(cursors) == 0 || cursors[0] != "c1" {
		t.Errorf("incremental cursors = %v, want first %q", cursors, "c1")
	}
	if full, _, _ := conn.counts(); full != 1 {
		t.Errorf("full syncs = %d, want exactly 1", full)
	}

	// The first write marked the backfill started.
	ws := r.cpFake.writes()
	if len(ws) == 0 || ws[0].state.GetPhase() != controlplanev1.SyncPhase_FULL_SYNC {
		t.Fatalf("write phases = %v, want FULL_SYNC first", phases(ws))
	}
	if ws[0].state.GetLastSyncStarted() == nil {
		t.Error("started write has no last_sync_started")
	}
	if ws[0].tenant != tenantA {
		t.Errorf("state write tenant = %q, want %q", ws[0].tenant, tenantA)
	}

	// The hub built the sdk.Config: tenant, instance, token, merged webhook_url.
	cfg, ok := conn.lastConfig()
	if !ok {
		t.Fatal("connector never received a config")
	}
	if string(cfg.Tenant.TenantID()) != tenantA || cfg.InstanceID != instGmail || string(cfg.Token) != "tok-1" {
		t.Errorf("config = tenant %q instance %q token %q", cfg.Tenant.TenantID(), cfg.InstanceID, cfg.Token)
	}
	var m map[string]any
	if err := json.Unmarshal(cfg.ConfigJSON, &m); err != nil {
		t.Fatalf("ConfigJSON: %v", err)
	}
	wantURL := webhookBase + "/webhooks/gmail/" + instGmail
	if m["webhook_url"] != wantURL {
		t.Errorf("webhook_url = %v, want %s", m["webhook_url"], wantURL)
	}
	if m["label"] != "INBOX" {
		t.Errorf("merged config lost original key: %v", m)
	}

	// Documents reached docs.raw through the chokepoint, stamped.
	docs := r.prod.docs()
	if len(docs) != 2 {
		t.Fatalf("produced %d docs, want 2", len(docs))
	}
	for _, d := range docs {
		if d.topic != kafkautil.TopicDocsRaw {
			t.Errorf("topic = %q, want %q", d.topic, kafkautil.TopicDocsRaw)
		}
		if d.tenant != tenantA {
			t.Errorf("produce ctx tenant = %q, want %q", d.tenant, tenantA)
		}
		if d.doc.GetTs().GetIngested() == nil {
			t.Errorf("doc %s missing ts.ingested", d.doc.GetDocId())
		}
		if d.doc.GetConnectorId() != "gmail" {
			t.Errorf("doc %s connector_id = %q, want gmail", d.doc.GetDocId(), d.doc.GetConnectorId())
		}
	}
}

func TestCheckpointPersistsAndResumesInterruptedBackfill(t *testing.T) {
	conn := &fakeConnector{id: "gmail"}
	conn.fullSyncFn = func(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
		if err := emit(ctx, testDoc(tenantA, "gmail:1")); err != nil {
			return "", err
		}
		if err := cfg.Checkpoint(ctx, "ckpt-1"); err != nil {
			return "", err
		}
		return "", errors.New("source hiccup mid-backfill")
	}
	r := newRig(t, conn, nil)
	r.cpFake.addInstance(instGmail, tenantA, "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)
	r.start(t)

	// The failure is recorded with the CHECKPOINTED cursor preserved.
	waitFor(t, 5*time.Second, func() bool {
		w, ok := r.cpFake.lastWrite()
		return ok && w.state.GetPhase() == controlplanev1.SyncPhase_FAILED
	}, "FAILED write after mid-backfill error")
	w, _ := r.cpFake.lastWrite()
	if w.state.GetCursor() != "ckpt-1" {
		t.Errorf("FAILED cursor = %q, want checkpointed ckpt-1", w.state.GetCursor())
	}
	if !strings.Contains(w.state.GetLastError(), "source hiccup") {
		t.Errorf("last_error = %q, want the sync error", w.state.GetLastError())
	}
	if w.state.GetDocsEmitted() != 1 {
		t.Errorf("docs_emitted = %d, want 1 (emitted before the failure)", w.state.GetDocsEmitted())
	}

	// The mid-backfill checkpoint itself was persisted (FULL_SYNC + cursor).
	var sawCheckpoint bool
	for _, w := range r.cpFake.writes() {
		if w.state.GetPhase() == controlplanev1.SyncPhase_FULL_SYNC && w.state.GetCursor() == "ckpt-1" {
			sawCheckpoint = true
		}
	}
	if !sawCheckpoint {
		t.Error("no FULL_SYNC write with the checkpointed cursor")
	}

	// Resume: the next pass continues from the checkpoint via IncrementalSync
	// instead of restarting the backfill.
	waitFor(t, 5*time.Second, func() bool {
		_, inc, _ := conn.counts()
		return inc >= 1
	}, "resumed sync after failure backoff")
	if cursors := conn.cursorsSeen(); len(cursors) == 0 || cursors[0] != "ckpt-1" {
		t.Errorf("resume cursors = %v, want first ckpt-1", cursors)
	}
	if full, _, _ := conn.counts(); full != 1 {
		t.Errorf("full syncs = %d, want 1 (no restart after checkpoint)", full)
	}
}

func TestCursorExpiredClearsCursorAndRestartsFullSync(t *testing.T) {
	conn := &fakeConnector{id: "gmail"}
	conn.incFn = func(_ context.Context, _ sdk.Config, cur sdk.Cursor, _ sdk.Emit) (sdk.Cursor, error) {
		if cur == "stale" {
			return "", fmt.Errorf("history gone: %w", sdk.ErrCursorExpired)
		}
		return cur, nil
	}
	conn.fullSyncFn = func(ctx context.Context, _ sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
		if err := emit(ctx, testDoc(tenantA, "gmail:re-1")); err != nil {
			return "", err
		}
		return "fresh", nil
	}
	r := newRig(t, conn, nil)
	r.cpFake.addInstance(instGmail, tenantA, "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)
	r.cpFake.seedState(&controlplanev1.SyncState{
		ConnectorInstanceId: instGmail,
		Cursor:              "stale",
		Phase:               controlplanev1.SyncPhase_INCREMENTAL,
		DocsEmitted:         5,
	})
	r.start(t)

	waitFor(t, 5*time.Second, func() bool {
		w, ok := r.cpFake.lastWrite()
		return ok && w.state.GetPhase() == controlplanev1.SyncPhase_INCREMENTAL && w.state.GetCursor() == "fresh"
	}, "full-sync restart completion")

	if cursors := conn.cursorsSeen(); len(cursors) == 0 || cursors[0] != "stale" {
		t.Fatalf("incremental cursors = %v, want first stale", cursors)
	}
	if full, _, _ := conn.counts(); full != 1 {
		t.Errorf("full syncs = %d, want exactly 1 (immediate restart in the same pass)", full)
	}

	// The restart was recorded: a FULL_SYNC write with the cursor CLEARED
	// before the fresh completion write.
	var sawCleared bool
	for _, w := range r.cpFake.writes() {
		if w.state.GetPhase() == controlplanev1.SyncPhase_FULL_SYNC && w.state.GetCursor() == "" {
			sawCleared = true
		}
	}
	if !sawCleared {
		t.Errorf("write phases = %v: no FULL_SYNC write with cleared cursor", phases(r.cpFake.writes()))
	}
	// Doc accounting carried across the restart: 5 prior + 1 from backfill.
	w, _ := r.cpFake.lastWrite()
	if w.state.GetDocsEmitted() != 6 {
		t.Errorf("docs_emitted = %d, want 6", w.state.GetDocsEmitted())
	}
}

func TestWebhookTriggersImmediateIncrementalSync(t *testing.T) {
	conn := &fakeConnector{id: "gmail"}
	conn.webhookFn = func(_ context.Context, _ sdk.Config, _ *http.Request, _ sdk.Emit) error {
		return nil
	}
	// Steady interval far beyond the test horizon: only a webhook can cause
	// the second pass.
	r := newRig(t, conn, func(o *schedulerOpts) { o.syncInterval = time.Hour })
	r.cpFake.addInstance(instGmail, tenantA, "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)
	r.start(t)

	waitFor(t, 5*time.Second, func() bool {
		w, ok := r.cpFake.lastWrite()
		return ok && w.state.GetPhase() == controlplanev1.SyncPhase_INCREMENTAL
	}, "initial backfill completion")
	if _, inc, _ := conn.counts(); inc != 0 {
		t.Fatalf("incremental syncs before webhook = %d, want 0", inc)
	}

	api := &httpAPI{cp: r.sch.cp, registry: r.sch.registry, sched: r.sch, emit: r.sch.emit, upload: stubUpload(nil), logger: testLogger()}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gmail/"+instGmail, strings.NewReader(`{"ping":1}`))
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook status = %d (body %s), want 202", rec.Code, rec.Body)
	}

	waitFor(t, 5*time.Second, func() bool {
		_, inc, _ := conn.counts()
		return inc >= 1
	}, "incremental sync triggered by webhook")
	if _, _, hooks := conn.counts(); hooks != 1 {
		t.Errorf("HandleWebhook calls = %d, want 1", hooks)
	}
}

func TestPausedAndRemovedInstancesStopTheirWorkers(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(f *fakeControlPlane)
	}{
		{"paused", func(f *fakeControlPlane) { f.setStatus(instGmail, controlplanev1.ConnectorStatus_PAUSED) }},
		{"removed", func(f *fakeControlPlane) { f.removeInstance(instGmail) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &fakeConnector{id: "gmail"}
			r := newRig(t, conn, nil)
			r.cpFake.addInstance(instGmail, tenantA, "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)
			r.start(t)

			waitFor(t, 5*time.Second, func() bool {
				_, ok := r.sch.lookup(instGmail)
				return ok
			}, "worker started")

			tc.stop(r.cpFake)
			waitFor(t, 5*time.Second, func() bool {
				_, ok := r.sch.lookup(instGmail)
				return !ok
			}, "worker removed after instance went inactive")

			full0, inc0, _ := conn.counts()
			time.Sleep(6 * r.sch.syncInterval) // several steady intervals
			full1, inc1, _ := conn.counts()
			if full1 != full0 || inc1 != inc0 {
				t.Errorf("syncs continued after stop: full %d->%d inc %d->%d", full0, full1, inc0, inc1)
			}
		})
	}
}

func TestRepeatedFailuresRecordFAILEDAndKeepRetrying(t *testing.T) {
	conn := &fakeConnector{id: "gmail"}
	conn.fullSyncFn = func(context.Context, sdk.Config, sdk.Emit) (sdk.Cursor, error) {
		return "", errors.New("source is down")
	}
	r := newRig(t, conn, nil)
	r.cpFake.addInstance(instGmail, tenantA, "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)
	r.start(t)

	// Retries continue through backoff (base 10ms, cap 50ms in tests).
	waitFor(t, 10*time.Second, func() bool {
		full, _, _ := conn.counts()
		return full >= 3
	}, "at least 3 backfill attempts")

	waitFor(t, 5*time.Second, func() bool {
		w, ok := r.cpFake.lastWrite()
		return ok && w.state.GetPhase() == controlplanev1.SyncPhase_FAILED
	}, "FAILED state recorded")
	w, _ := r.cpFake.lastWrite()
	if !strings.Contains(w.state.GetLastError(), "source is down") {
		t.Errorf("last_error = %q, want the sync error", w.state.GetLastError())
	}
	// Each attempt re-marks FULL_SYNC then records FAILED; no INCREMENTAL ever.
	for _, w := range r.cpFake.writes() {
		if p := w.state.GetPhase(); p != controlplanev1.SyncPhase_FULL_SYNC && p != controlplanev1.SyncPhase_FAILED {
			t.Errorf("unexpected phase %v in failure loop", p)
		}
	}
}

func TestReconcileSkipsInvalidTenant(t *testing.T) {
	conn := &fakeConnector{id: "gmail"}
	r := newRig(t, conn, nil)
	r.cpFake.addInstance(instGmail, "bad tenant!", "gmail", nil, controlplanev1.ConnectorStatus_ACTIVE)
	r.start(t)

	waitFor(t, 5*time.Second, func() bool {
		r.cpFake.mu.Lock()
		defer r.cpFake.mu.Unlock()
		return r.cpFake.listCalls >= 2
	}, "two reconcile rounds")

	if _, ok := r.sch.lookup(instGmail); ok {
		t.Error("worker scheduled for an instance with an invalid tenant")
	}
	if full, inc, _ := conn.counts(); full != 0 || inc != 0 {
		t.Errorf("connector ran (%d full, %d inc) for invalid tenant", full, inc)
	}
}

func TestBackoffDelay(t *testing.T) {
	base, limit := 5*time.Second, 5*time.Minute
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{6, 160 * time.Second},
		{7, 5 * time.Minute}, // 320s caps at 5m
		{100, 5 * time.Minute},
	}
	for _, c := range cases {
		if got := backoffDelay(base, limit, c.failures); got != c.want {
			t.Errorf("backoffDelay(%d) = %v, want %v", c.failures, got, c.want)
		}
	}
}

func TestMergeWebhookURL(t *testing.T) {
	out, err := mergeWebhookURL([]byte(`{"a":1}`), "http://hub:9300/", "gmail", "i-1")
	if err != nil {
		t.Fatalf("mergeWebhookURL: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if m["webhook_url"] != "http://hub:9300/webhooks/gmail/i-1" {
		t.Errorf("webhook_url = %v", m["webhook_url"])
	}
	if m["a"] != float64(1) {
		t.Errorf("original key lost: %v", m)
	}

	if _, err := mergeWebhookURL([]byte(`[1,2]`), "http://hub:9300", "gmail", "i-1"); err == nil {
		t.Error("non-object config accepted")
	}
	out, err = mergeWebhookURL(nil, "http://hub:9300", "gmail", "i-1")
	if err != nil || !json.Valid(out) {
		t.Errorf("empty config merge: %v / %s", err, out)
	}
}
