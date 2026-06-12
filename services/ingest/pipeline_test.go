package main

import (
	"context"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	"github.com/asker/asker/platform/kafkautil"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

const pipelineWait = 30 * time.Second

// newKafkaTestConfig starts an in-memory kfake cluster.
func newKafkaTestConfig(t *testing.T) kafkautil.Config {
	t.Helper()
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1))
	if err != nil {
		t.Fatalf("kfake.NewCluster: %v", err)
	}
	t.Cleanup(cluster.Close)
	return kafkautil.Config{Brokers: cluster.ListenAddrs(), ClientID: "ingest-test"}
}

func tenantCtx(t *testing.T, tenant string) context.Context {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": tenant})
	if err != nil {
		t.Fatalf("tenancy.FromClaims(%q): %v", tenant, err)
	}
	return tenancy.WithContext(context.Background(), tc)
}

// fetchTopic reads up to want records from topic, polling until want arrive
// or wait elapses.
func fetchTopic(t *testing.T, cfg kafkautil.Config, topic string, want int, wait time.Duration) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ConsumeTopics(topic),
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

// TestPipelineRawToChunked wires the real kafkautil consumer and producer
// around the handler (with the in-memory dedupe fake) and drives documents
// through docs.raw -> ingest -> docs.chunked end to end:
//
//   - a normal document comes out normalized and chunked,
//   - its byte-identical replay is deduped away,
//   - a tombstone passes through untouched.
func TestPipelineRawToChunked(t *testing.T) {
	t.Parallel()
	cfg := newKafkaTestConfig(t)
	ctx := context.Background()
	if err := kafkautil.EnsureTopics(ctx, cfg, 4,
		kafkautil.TopicDocsRaw, kafkautil.TopicDocsChunked,
		kafkautil.TopicDocsEnriched, kafkautil.TopicDocsDeadletter); err != nil {
		t.Fatalf("EnsureTopics: %v", err)
	}

	producer, err := kafkautil.NewProducer(cfg)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	t.Cleanup(producer.Close)

	consumer, err := kafkautil.NewConsumer(cfg, consumerGroup, kafkautil.TopicDocsRaw)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	t.Cleanup(consumer.Close)

	h := newHandler(producer, newFakeSeen(), discardLogger())
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- consumer.Run(runCtx, h.Handle) }()

	// One normal document (produced twice: the replay must be deduped) and
	// one tombstone.
	doc := &askerv1.Document{
		TenantId:       "tenant-a",
		DocId:          "doc-mail",
		ConnectorId:    "gmail",
		SourceNativeId: "native-mail",
		Type:           askerv1.DocType_EMAIL,
		Title:          "  Meeting   notes ",
		BodyText:       "Fresh reply.\r\n\r\nOn Mon, Jun 2, 2025 at 9:14 AM Alice <a@example.com> wrote:\r\n> earlier message",
		VersionEtag:    "etag-1",
	}
	tomb := &askerv1.Document{
		TenantId:    "tenant-a",
		DocId:       "doc-gone",
		ConnectorId: "gmail",
		VersionEtag: "etag-del",
		Tombstone:   &askerv1.Tombstone{Deleted: true},
	}
	tctx := tenantCtx(t, "tenant-a")
	for _, d := range []*askerv1.Document{doc, proto.Clone(doc).(*askerv1.Document), tomb} {
		if err := producer.ProduceDocument(tctx, kafkautil.TopicDocsRaw, d); err != nil {
			t.Fatalf("ProduceDocument(%s): %v", d.GetDocId(), err)
		}
	}

	recs := fetchTopic(t, cfg, kafkautil.TopicDocsChunked, 2, pipelineWait)
	if len(recs) != 2 {
		t.Fatalf("docs.chunked has %d records, want 2 (chunked doc + tombstone; duplicate deduped)", len(recs))
	}

	byID := map[string]*askerv1.Document{}
	for _, rec := range recs {
		var out askerv1.Document
		if err := proto.Unmarshal(rec.Value, &out); err != nil {
			t.Fatalf("unmarshal chunked record: %v", err)
		}
		byID[out.GetDocId()] = &out

		if got := string(rec.Key); got != "tenant-a" {
			t.Errorf("record key = %q, want tenant-a", got)
		}
		headers := map[string]string{}
		for _, hd := range rec.Headers {
			headers[hd.Key] = string(hd.Value)
		}
		if headers[kafkautil.HeaderTenantID] != "tenant-a" {
			t.Errorf("tenant_id header = %q, want tenant-a", headers[kafkautil.HeaderTenantID])
		}
		if headers[kafkautil.HeaderDocID] != out.GetDocId() {
			t.Errorf("doc_id header = %q, want %q", headers[kafkautil.HeaderDocID], out.GetDocId())
		}
		if headers[kafkautil.HeaderVersionEtag] != out.GetVersionEtag() {
			t.Errorf("version_etag header = %q, want %q", headers[kafkautil.HeaderVersionEtag], out.GetVersionEtag())
		}
	}

	mail := byID["doc-mail"]
	if mail == nil {
		t.Fatal("chunked doc-mail not found on docs.chunked")
	}
	if mail.GetTitle() != "Meeting notes" {
		t.Errorf("title = %q, want normalized %q", mail.GetTitle(), "Meeting notes")
	}
	if len(mail.GetChunks()) != 2 {
		t.Fatalf("doc-mail has %d chunks, want 2 (fresh reply + quoted section)", len(mail.GetChunks()))
	}
	for i, c := range mail.GetChunks() {
		start, end := int(c.GetCharStart()), int(c.GetCharEnd())
		if mail.GetBodyText()[start:end] != c.GetText() {
			t.Errorf("chunk %d offsets do not slice the normalized body", i)
		}
		if len(c.GetEmbedding()) != 0 {
			t.Errorf("chunk %d has an embedding on docs.chunked; that is enrich's job", i)
		}
	}

	gone := byID["doc-gone"]
	if gone == nil {
		t.Fatal("tombstone doc-gone not found on docs.chunked")
	}
	if !proto.Equal(gone, tomb) {
		t.Errorf("tombstone modified in flight:\n got %v\nwant %v", gone, tomb)
	}

	// Nothing was quarantined.
	if dl := fetchTopic(t, cfg, kafkautil.TopicDocsDeadletter, 1, time.Second); len(dl) != 0 {
		t.Errorf("docs.deadletter has %d records, want 0", len(dl))
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("consumer.Run: %v", err)
		}
	case <-time.After(pipelineWait):
		t.Fatal("timeout waiting for consumer.Run to stop")
	}
}
