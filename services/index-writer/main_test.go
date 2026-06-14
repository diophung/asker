package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/platform/kafkautil"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

const testWait = 30 * time.Second

// TestEndToEndKafkaToVespa drives the real service loop: run() consumes
// docs.enriched from an in-memory kfake cluster and feeds an httptest Vespa
// stub — upsert POST, dimension-mismatch quarantine to docs.deadletter, and
// tombstone DELETE, then a clean drain on context cancellation.
func TestEndToEndKafkaToVespa(t *testing.T) {
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1))
	if err != nil {
		t.Fatalf("kfake.NewCluster: %v", err)
	}
	t.Cleanup(cluster.Close)

	stub := newVespaStub(t, nil)

	t.Setenv("KAFKA_BROKERS", strings.Join(cluster.ListenAddrs(), ","))
	t.Setenv("KAFKA_CLIENT_ID", "index-writer-test")
	t.Setenv("VESPA_URL", stub.srv.URL)
	t.Setenv("EMBEDDING_DIM", "4")
	t.Setenv("INDEX_WRITER_HEALTH_ADDR", "127.0.0.1:0")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	p, err := kafkautil.NewProducer(cfg.Kafka)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	t.Cleanup(p.Close)
	tctx := tenantCtx(t, "tenant-a")

	// 1) A rich enriched document becomes the exact upsert POST.
	if err := p.ProduceDocument(tctx, kafkautil.TopicDocsEnriched, richDoc()); err != nil {
		t.Fatalf("ProduceDocument(rich): %v", err)
	}
	reqs := stub.waitRequests(1, testWait)
	if len(reqs) != 1 {
		t.Fatalf("vespa saw %d requests, want 1", len(reqs))
	}
	if reqs[0].method != http.MethodPost {
		t.Errorf("method = %s, want POST", reqs[0].method)
	}
	if want := "/document/v1/asker/doc/group/tenant-a/doc-rich-1"; reqs[0].path != want {
		t.Errorf("path = %q, want %q", reqs[0].path, want)
	}
	fields := feedFields(t, reqs[0].body)
	if fields["doc_id"] != "doc-rich-1" || fields["type"] != "EMAIL" || fields["version_etag"] != "etag-1" {
		t.Errorf("fed fields = doc_id:%v type:%v etag:%v, want doc-rich-1/EMAIL/etag-1",
			fields["doc_id"], fields["type"], fields["version_etag"])
	}
	if _, ok := fields["embedding"].(map[string]any); !ok {
		t.Errorf("embedding missing or malformed in fed JSON: %v", fields["embedding"])
	}

	// 2) A wrong-dimension document never reaches Vespa and is quarantined
	// to docs.deadletter after the consumer's attempts.
	bad := richDoc()
	bad.DocId = "doc-bad-dim"
	bad.Chunks[0].Embedding = []float32{0.1, 0.2} // EMBEDDING_DIM is 4
	if err := p.ProduceDocument(tctx, kafkautil.TopicDocsEnriched, bad); err != nil {
		t.Fatalf("ProduceDocument(bad dim): %v", err)
	}
	dl := fetchDeadletter(t, cfg.Kafka, 1, testWait)
	if len(dl) != 1 {
		t.Fatalf("deadletter has %d records, want 1", len(dl))
	}
	hm := map[string]string{}
	for _, h := range dl[0].Headers {
		hm[h.Key] = string(h.Value)
	}
	if hm[kafkautil.HeaderDocID] != "doc-bad-dim" {
		t.Errorf("deadletter doc_id = %q, want doc-bad-dim", hm[kafkautil.HeaderDocID])
	}
	if !strings.Contains(hm[kafkautil.HeaderError], "EMBEDDING_DIM") {
		t.Errorf("deadletter error = %q, want EMBEDDING_DIM mismatch", hm[kafkautil.HeaderError])
	}
	if hm[kafkautil.HeaderOriginTopic] != kafkautil.TopicDocsEnriched {
		t.Errorf("deadletter origin_topic = %q, want %q", hm[kafkautil.HeaderOriginTopic], kafkautil.TopicDocsEnriched)
	}

	// 3) A tombstone becomes a DELETE on the same document path.
	tomb := &askerv1.Document{
		TenantId:    "tenant-a",
		DocId:       "doc-rich-1",
		ConnectorId: "gmail",
		VersionEtag: "etag-2",
		Tombstone:   &askerv1.Tombstone{Deleted: true, DeletedAt: timestamppb.Now()},
	}
	if err := p.ProduceDocument(tctx, kafkautil.TopicDocsEnriched, tomb); err != nil {
		t.Fatalf("ProduceDocument(tombstone): %v", err)
	}
	reqs = stub.waitRequests(2, testWait)
	if len(reqs) != 2 {
		t.Fatalf("vespa saw %d requests, want 2 (the bad-dim doc must never reach vespa)", len(reqs))
	}
	if reqs[1].method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", reqs[1].method)
	}
	if want := "/document/v1/asker/doc/group/tenant-a/doc-rich-1"; reqs[1].path != want {
		t.Errorf("path = %q, want %q", reqs[1].path, want)
	}

	// 4) Cancellation drains cleanly.
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run returned %v, want nil on graceful shutdown", err)
		}
	case <-time.After(testWait):
		t.Fatal("timeout waiting for run to drain")
	}
}

// fetchDeadletter reads up to want records from docs.deadletter, polling
// until they arrive or the wait elapses.
func fetchDeadletter(t *testing.T, cfg kafkautil.Config, want int, wait time.Duration) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ConsumeTopics(kafkautil.TopicDocsDeadletter),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	if err != nil {
		t.Fatalf("kgo.NewClient: %v", err)
	}
	t.Cleanup(cl.Close)
	deadline := time.Now().Add(wait)
	var recs []*kgo.Record
	for len(recs) < want && time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		fetches := cl.PollFetches(ctx)
		cancel()
		fetches.EachRecord(func(r *kgo.Record) { recs = append(recs, r) })
	}
	return recs
}

func TestConfigDefaults(t *testing.T) {
	// t.Setenv registers restoration; unset afterwards to exercise defaults.
	for _, key := range []string{"KAFKA_BROKERS", "VESPA_URL", "EMBEDDING_DIM", "INDEX_WRITER_HEALTH_ADDR", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
		t.Setenv(key, "placeholder")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("os.Unsetenv(%s): %v", key, err)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.VespaURL != "http://vespa:8080" {
		t.Errorf("VespaURL = %q, want http://vespa:8080", cfg.VespaURL)
	}
	if cfg.EmbeddingDim != 1024 {
		t.Errorf("EmbeddingDim = %d, want 1024", cfg.EmbeddingDim)
	}
	if cfg.HealthAddr != ":9701" {
		t.Errorf("HealthAddr = %q, want :9701", cfg.HealthAddr)
	}
	if len(cfg.Kafka.Brokers) != 1 || cfg.Kafka.Brokers[0] != "redpanda:9092" {
		t.Errorf("Kafka.Brokers = %v, want [redpanda:9092]", cfg.Kafka.Brokers)
	}
}

func TestConfigOverridesAndValidation(t *testing.T) {
	t.Setenv("KAFKA_BROKERS", "b1:9092")
	t.Setenv("VESPA_URL", "http://vespa.test:8080")
	t.Setenv("EMBEDDING_DIM", "384")
	t.Setenv("INDEX_WRITER_HEALTH_ADDR", ":9999")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.VespaURL != "http://vespa.test:8080" || cfg.EmbeddingDim != 384 || cfg.HealthAddr != ":9999" {
		t.Errorf("cfg = %+v, want overridden values", cfg)
	}

	t.Setenv("EMBEDDING_DIM", "0")
	if _, err := loadConfig(); err == nil {
		t.Error("loadConfig with EMBEDDING_DIM=0: want error, got nil")
	}
	t.Setenv("EMBEDDING_DIM", "not-a-number")
	if _, err := loadConfig(); err == nil {
		t.Error("loadConfig with non-numeric EMBEDDING_DIM: want error, got nil")
	}
	t.Setenv("EMBEDDING_DIM", "384")
	t.Setenv("VESPA_URL", "  ")
	if _, err := loadConfig(); err == nil {
		t.Error("loadConfig with blank VESPA_URL: want error, got nil")
	}
}

func TestHealthHandler(t *testing.T) {
	t.Parallel()

	t.Run("healthz always ok", func(t *testing.T) {
		t.Parallel()
		h := newHealthHandler("http://vespa.invalid:1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("healthz = %d, want 200", rec.Code)
		}
	})

	t.Run("readyz up when vespa is up", func(t *testing.T) {
		t.Parallel()
		vespa := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/state/v1/health" {
				t.Errorf("readyz probed %q, want /state/v1/health", r.URL.Path)
			}
			_, _ = io.WriteString(rw, `{"status":{"code":"up"}}`)
		}))
		t.Cleanup(vespa.Close)
		h := newHealthHandler(vespa.URL)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("readyz = %d, want 200", rec.Code)
		}
	})

	t.Run("readyz 503 when vespa errors", func(t *testing.T) {
		t.Parallel()
		vespa := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			rw.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(vespa.Close)
		h := newHealthHandler(vespa.URL)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("readyz = %d, want 503", rec.Code)
		}
	})

	t.Run("readyz 503 when vespa is unreachable", func(t *testing.T) {
		t.Parallel()
		h := newHealthHandler("http://127.0.0.1:1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("readyz = %d, want 503", rec.Code)
		}
	})
}

func TestRunHealthcheck(t *testing.T) {
	t.Parallel()

	healthy := httptest.NewServer(newHealthHandler("http://vespa.invalid:1"))
	t.Cleanup(healthy.Close)
	if got := runHealthcheck(healthy.Listener.Addr().String()); got != 0 {
		t.Errorf("runHealthcheck(healthy) = %d, want 0", got)
	}

	unhealthy := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(unhealthy.Close)
	if got := runHealthcheck(unhealthy.Listener.Addr().String()); got != 1 {
		t.Errorf("runHealthcheck(unhealthy) = %d, want 1", got)
	}

	if got := runHealthcheck("127.0.0.1:1"); got != 1 {
		t.Errorf("runHealthcheck(unreachable) = %d, want 1", got)
	}
}
