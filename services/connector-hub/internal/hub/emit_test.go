package hub

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/platform/kafkautil"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// newKafkaEnv boots an in-memory kfake cluster with the pipeline topics and
// returns the kafkautil config pointing at it.
func newKafkaEnv(t *testing.T) kafkautil.Config {
	t.Helper()
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1))
	if err != nil {
		t.Fatalf("kfake.NewCluster: %v", err)
	}
	t.Cleanup(cluster.Close)
	cfg := kafkautil.Config{Brokers: cluster.ListenAddrs(), ClientID: "hub-test"}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := kafkautil.EnsureTopics(ctx, cfg, 1,
		kafkautil.TopicDocsRaw, kafkautil.TopicDocsChunked,
		kafkautil.TopicDocsEnriched, kafkautil.TopicDocsDeadletter); err != nil {
		t.Fatalf("EnsureTopics: %v", err)
	}
	return cfg
}

func newRealProducer(t *testing.T, cfg kafkautil.Config) *kafkautil.Producer {
	t.Helper()
	p, err := kafkautil.NewProducer(cfg)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// fetchRawRecords reads up to want records from docs.raw, returning whatever
// arrived before the deadline.
func fetchRawRecords(t *testing.T, cfg kafkautil.Config, want int, wait time.Duration) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ConsumeTopics(kafkautil.TopicDocsRaw),
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

func TestEmitterStampsAndProducesToDocsRaw(t *testing.T) {
	cfg := newKafkaEnv(t)
	fixed := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	em := newEmitter(newRealProducer(t, cfg), kafkautil.TopicDocsRaw, func() time.Time { return fixed })

	var emitted atomic.Int64
	emit := em.emitFor(mustTenant(t, tenantA), "gmail", &emitted)

	doc := testDoc(tenantA, "gmail:msg-1")
	doc.ConnectorId = "" // hub must fill it from the instance
	// Connectors leave ts.ingested unset; ctx carries no tenant — the
	// chokepoint installs the verified instance tenant itself.
	if err := emit(context.Background(), doc); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if emitted.Load() != 1 {
		t.Errorf("emitted counter = %d, want 1", emitted.Load())
	}

	recs := fetchRawRecords(t, cfg, 1, 30*time.Second)
	if len(recs) != 1 {
		t.Fatalf("docs.raw records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if string(rec.Key) != tenantA {
		t.Errorf("record key = %q, want tenant %q (per-tenant ordering)", rec.Key, tenantA)
	}
	headers := make(map[string]string, len(rec.Headers))
	for _, h := range rec.Headers {
		headers[h.Key] = string(h.Value)
	}
	if headers[kafkautil.HeaderTenantID] != tenantA || headers[kafkautil.HeaderDocID] != "gmail:msg-1" ||
		headers[kafkautil.HeaderVersionEtag] != "v1" {
		t.Errorf("headers = %v", headers)
	}

	var got askerv1.Document
	if err := proto.Unmarshal(rec.Value, &got); err != nil {
		t.Fatalf("unmarshal produced doc: %v", err)
	}
	if !got.GetTs().GetIngested().AsTime().Equal(fixed) {
		t.Errorf("ts.ingested = %v, want %v (stamped at emit time)", got.GetTs().GetIngested().AsTime(), fixed)
	}
	if got.GetConnectorId() != "gmail" {
		t.Errorf("connector_id = %q, want gmail (filled by the hub)", got.GetConnectorId())
	}
}

func TestEmitterRejectsTenantMismatch(t *testing.T) {
	cfg := newKafkaEnv(t)
	em := newEmitter(newRealProducer(t, cfg), kafkautil.TopicDocsRaw, time.Now)

	var emitted atomic.Int64
	emit := em.emitFor(mustTenant(t, tenantA), "gmail", &emitted)

	err := emit(context.Background(), testDoc(tenantB, "gmail:evil"))
	if err == nil {
		t.Fatal("cross-tenant emit succeeded, want rejection")
	}
	if emitted.Load() != 0 {
		t.Errorf("emitted counter = %d, want 0", emitted.Load())
	}
	if recs := fetchRawRecords(t, cfg, 1, 2*time.Second); len(recs) != 0 {
		t.Errorf("cross-tenant doc reached docs.raw: %d records", len(recs))
	}
}

func TestEmitterPreservesConnectorIDAndTimestamps(t *testing.T) {
	prod := &fakeProducer{}
	fixed := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	em := newEmitter(prod, kafkautil.TopicDocsRaw, func() time.Time { return fixed })
	emit := em.emitFor(mustTenant(t, tenantA), "gmail", nil)

	created := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	doc := testDoc(tenantA, "custom:1")
	doc.ConnectorId = "custom"
	doc.Ts = &askerv1.Timestamps{Created: timestamppb.New(created)}
	if err := emit(context.Background(), doc); err != nil {
		t.Fatalf("emit: %v", err)
	}

	docs := prod.docs()
	if len(docs) != 1 {
		t.Fatalf("produced %d docs, want 1", len(docs))
	}
	got := docs[0].doc
	if got.GetConnectorId() != "custom" {
		t.Errorf("connector_id overwritten: %q", got.GetConnectorId())
	}
	if !got.GetTs().GetCreated().AsTime().Equal(created) {
		t.Errorf("ts.created clobbered: %v", got.GetTs().GetCreated().AsTime())
	}
	if !got.GetTs().GetIngested().AsTime().Equal(fixed) {
		t.Errorf("ts.ingested = %v, want %v", got.GetTs().GetIngested().AsTime(), fixed)
	}
}

func TestEmitterNilDocument(t *testing.T) {
	prod := &fakeProducer{}
	em := newEmitter(prod, kafkautil.TopicDocsRaw, nil)
	emit := em.emitFor(mustTenant(t, tenantA), "gmail", nil)
	if err := emit(context.Background(), nil); err == nil {
		t.Fatal("nil document accepted")
	}
	if len(prod.docs()) != 0 {
		t.Error("nil document produced")
	}
}
