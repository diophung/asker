package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/asker/asker/platform/kafkautil"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func TestLoadConfigDefaults(t *testing.T) {
	for _, key := range []string{"INGEST_HEALTH_ADDR", "REDIS_ADDR", "OTEL_EXPORTER_OTLP_ENDPOINT", "KAFKA_BROKERS", "KAFKA_CLIENT_ID"} {
		// t.Setenv registers restoration; unset to exercise envDefault.
		t.Setenv(key, "placeholder")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("os.Unsetenv(%s): %v", key, err)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.HealthAddr != ":9501" {
		t.Errorf("HealthAddr = %q, want :9501", cfg.HealthAddr)
	}
	if cfg.RedisAddr != "redis:6379" {
		t.Errorf("RedisAddr = %q, want redis:6379", cfg.RedisAddr)
	}
	if cfg.OTLPEndpoint != "" {
		t.Errorf("OTLPEndpoint = %q, want empty", cfg.OTLPEndpoint)
	}
	if len(cfg.Kafka.Brokers) != 1 || cfg.Kafka.Brokers[0] != "redpanda:9092" {
		t.Errorf("Kafka.Brokers = %v, want [redpanda:9092]", cfg.Kafka.Brokers)
	}
	if cfg.Kafka.ClientID != serviceName {
		t.Errorf("Kafka.ClientID = %q, want %q default", cfg.Kafka.ClientID, serviceName)
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("INGEST_HEALTH_ADDR", ":7777")
	t.Setenv("REDIS_ADDR", "redis-other:6380")
	t.Setenv("KAFKA_BROKERS", "k1:9092,k2:9092")
	t.Setenv("KAFKA_CLIENT_ID", "custom-id")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.HealthAddr != ":7777" || cfg.RedisAddr != "redis-other:6380" {
		t.Errorf("cfg = %+v, want env overrides applied", cfg)
	}
	if len(cfg.Kafka.Brokers) != 2 {
		t.Errorf("Kafka.Brokers = %v, want 2 brokers", cfg.Kafka.Brokers)
	}
	if cfg.Kafka.ClientID != "custom-id" {
		t.Errorf("Kafka.ClientID = %q, want custom-id", cfg.Kafka.ClientID)
	}
}

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()
	var ready atomic.Bool
	srv := httptest.NewServer(newHealthHandler(&ready))
	t.Cleanup(srv.Close)

	get := func(path string) (int, map[string]string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode %s response: %v", path, err)
		}
		return resp.StatusCode, body
	}

	if code, body := get("/healthz"); code != http.StatusOK || body["status"] != "ok" {
		t.Errorf("GET /healthz = %d %v, want 200 ok", code, body)
	}
	if code, _ := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz before ready = %d, want 503", code)
	}
	ready.Store(true)
	if code, body := get("/readyz"); code != http.StatusOK || body["status"] != "ready" {
		t.Errorf("GET /readyz when ready = %d %v, want 200 ready", code, body)
	}
	ready.Store(false)
	if code, _ := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz after drain start = %d, want 503", code)
	}
	if code, _ := get("/nope"); code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", code)
	}

	resp, err := http.Post(srv.URL+"/healthz", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405", resp.StatusCode)
	}
}

func TestRunHealthcheck(t *testing.T) {
	t.Parallel()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(healthy.Close)
	if got := runHealthcheck(healthy.Listener.Addr().String()); got != 0 {
		t.Errorf("healthy probe exit = %d, want 0", got)
	}

	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(sick.Close)
	if got := runHealthcheck(sick.Listener.Addr().String()); got != 1 {
		t.Errorf("sick probe exit = %d, want 1", got)
	}

	// Nothing listening at all.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()
	if got := runHealthcheck(deadAddr); got != 1 {
		t.Errorf("dead probe exit = %d, want 1", got)
	}
}

// TestRunEndToEnd boots the full service shell (run with a real kfake
// cluster, an unreachable Redis so dedupe fails open, and a random health
// port), pushes a document through docs.raw, observes it chunked on
// docs.chunked, then shuts down gracefully via context cancellation.
func TestRunEndToEnd(t *testing.T) {
	t.Parallel()
	kcfg := newKafkaTestConfig(t)

	// A closed port: Redis is "down" for the whole test (fail-open path).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadRedis := ln.Addr().String()
	_ = ln.Close()

	cfg := ingestConfig{
		HealthAddr: "127.0.0.1:0",
		RedisAddr:  deadRedis,
		Kafka:      kcfg,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, cfg, discardLogger()) }()

	// run() creates the topics; wait for them so the produce does not race
	// service startup.
	producer, err := kafkautil.NewProducer(kcfg)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	t.Cleanup(producer.Close)
	doc := rawDoc("doc-e2e", "") // empty etag: ingest must derive one
	tctx := tenantCtx(t, "tenant-a")
	deadline := time.Now().Add(pipelineWait)
	for {
		if err = producer.ProduceDocument(tctx, kafkautil.TopicDocsRaw, doc); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ProduceDocument never succeeded: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	recs := fetchTopic(t, kcfg, kafkautil.TopicDocsChunked, 1, pipelineWait)
	if len(recs) != 1 {
		t.Fatalf("docs.chunked has %d records, want 1", len(recs))
	}
	var out askerv1.Document
	if err := proto.Unmarshal(recs[0].Value, &out); err != nil {
		t.Fatalf("unmarshal chunked record: %v", err)
	}
	if out.GetDocId() != "doc-e2e" {
		t.Errorf("doc_id = %q, want doc-e2e", out.GetDocId())
	}
	if len(out.GetChunks()) == 0 {
		t.Error("document came through unchunked")
	}
	if out.GetVersionEtag() == "" || !isHexSHA256(out.GetVersionEtag()) {
		t.Errorf("version_etag = %q, want derived sha256 hex", out.GetVersionEtag())
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run returned %v, want nil on graceful shutdown", err)
		}
	case <-time.After(pipelineWait):
		t.Fatal("timeout waiting for run to stop")
	}
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'f')
	}) < 0
}
