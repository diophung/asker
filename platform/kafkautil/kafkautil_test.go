package kafkautil

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	"github.com/asker/asker/platform/config"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

const testWait = 30 * time.Second

// newTestConfig starts an in-memory kfake cluster and returns a Config
// pointing at it.
func newTestConfig(t *testing.T) Config {
	t.Helper()
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1))
	if err != nil {
		t.Fatalf("kfake.NewCluster: %v", err)
	}
	t.Cleanup(cluster.Close)
	return Config{Brokers: cluster.ListenAddrs(), ClientID: "kafkautil-test"}
}

// tenantCtx returns a context carrying a tenancy.Context for tenant.
func tenantCtx(t *testing.T, tenant string) context.Context {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": tenant})
	if err != nil {
		t.Fatalf("tenancy.FromClaims(%q): %v", tenant, err)
	}
	return tenancy.WithContext(context.Background(), tc)
}

func testDoc(tenant, docID, etag string) *askerv1.Document {
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          docID,
		ConnectorId:    "gmail",
		SourceNativeId: "native-" + docID,
		Title:          "title of " + docID,
		BodyText:       "body of " + docID,
		VersionEtag:    etag,
	}
}

// newProducer builds a Producer with cleanup.
func newProducer(t *testing.T, cfg Config) *Producer {
	t.Helper()
	p, err := NewProducer(cfg)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// rawClient returns a plain kgo client for producing raw records and for
// inspecting topics without a consumer group.
func rawClient(t *testing.T, cfg Config, consumeTopics ...string) *kgo.Client {
	t.Helper()
	opts := []kgo.Opt{kgo.SeedBrokers(cfg.Brokers...)}
	if len(consumeTopics) > 0 {
		opts = append(opts,
			kgo.ConsumeTopics(consumeTopics...),
			kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		)
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		t.Fatalf("kgo.NewClient: %v", err)
	}
	t.Cleanup(cl.Close)
	return cl
}

// fetchRecords reads up to want records from topic, polling until want are
// seen or wait elapses. It returns however many arrived.
func fetchRecords(t *testing.T, cfg Config, topic string, want int, wait time.Duration) []*kgo.Record {
	t.Helper()
	cl := rawClient(t, cfg, topic)
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

func headerMap(rec *kgo.Record) map[string]string {
	m := make(map[string]string, len(rec.Headers))
	for _, h := range rec.Headers {
		m[h.Key] = string(h.Value)
	}
	return m
}

// startConsumer runs c.Run(ctx, h) in a goroutine and returns the error
// channel; Close is registered as cleanup.
func startConsumer(ctx context.Context, t *testing.T, c *Consumer, h Handler) <-chan error {
	t.Helper()
	t.Cleanup(c.Close)
	errc := make(chan error, 1)
	go func() { errc <- c.Run(ctx, h) }()
	return errc
}

func waitRunStops(t *testing.T, errc <-chan error) {
	t.Helper()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run returned error, want nil: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("timeout waiting for Run to return")
	}
}

func TestEnsureTopicsIdempotent(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	ctx := context.Background()

	topics := []string{TopicDocsRaw, TopicDocsChunked, TopicDocsEnriched, TopicDocsDeadletter}
	if err := EnsureTopics(ctx, cfg, 4, topics...); err != nil {
		t.Fatalf("first EnsureTopics: %v", err)
	}
	if err := EnsureTopics(ctx, cfg, 4, topics...); err != nil {
		t.Fatalf("second EnsureTopics (idempotency): %v", err)
	}
	// Overlapping set: one existing, one new.
	if err := EnsureTopics(ctx, cfg, 4, TopicDocsRaw, "docs.other"); err != nil {
		t.Fatalf("EnsureTopics with overlap: %v", err)
	}
}

func TestEnsureTopicsValidation(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	ctx := context.Background()

	if err := EnsureTopics(ctx, cfg, 0, TopicDocsRaw); err == nil {
		t.Error("EnsureTopics with partitions=0: want error, got nil")
	}
	if err := EnsureTopics(ctx, cfg, 4); err != nil {
		t.Errorf("EnsureTopics with no topics: want nil, got %v", err)
	}
	if err := EnsureTopics(ctx, Config{}, 4, TopicDocsRaw); err == nil {
		t.Error("EnsureTopics with no brokers: want error, got nil")
	}
	if err := EnsureTopics(ctx, Config{Brokers: []string{" "}}, 4, TopicDocsRaw); err == nil {
		t.Error("EnsureTopics with blank broker: want error, got nil")
	}
}

func TestProduceConsumeRoundTrip(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	ctx := context.Background()
	if err := EnsureTopics(ctx, cfg, 4, TopicDocsRaw, TopicDocsDeadletter); err != nil {
		t.Fatalf("EnsureTopics: %v", err)
	}

	p := newProducer(t, cfg)
	doc := testDoc("tenant-a", "doc-1", "etag-1")
	if err := p.ProduceDocument(tenantCtx(t, "tenant-a"), TopicDocsRaw, doc); err != nil {
		t.Fatalf("ProduceDocument: %v", err)
	}

	// Wire shape: key and headers on the raw record.
	recs := fetchRecords(t, cfg, TopicDocsRaw, 1, testWait)
	if len(recs) != 1 {
		t.Fatalf("raw fetch: got %d records, want 1", len(recs))
	}
	rec := recs[0]
	if got := string(rec.Key); got != "tenant-a" {
		t.Errorf("record key = %q, want %q", got, "tenant-a")
	}
	hm := headerMap(rec)
	for hdr, want := range map[string]string{
		HeaderTenantID:    "tenant-a",
		HeaderDocID:       "doc-1",
		HeaderVersionEtag: "etag-1",
	} {
		if hm[hdr] != want {
			t.Errorf("header %s = %q, want %q", hdr, hm[hdr], want)
		}
	}
	var onWire askerv1.Document
	if err := proto.Unmarshal(rec.Value, &onWire); err != nil {
		t.Fatalf("unmarshal wire value: %v", err)
	}
	if !proto.Equal(&onWire, doc) {
		t.Errorf("wire document = %v, want %v", &onWire, doc)
	}

	// Consumer delivers the document with a reconstructed tenancy.Context.
	c, err := NewConsumer(cfg, "group-roundtrip", TopicDocsRaw)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	type delivery struct {
		doc *askerv1.Document
		tc  tenancy.Context
	}
	got := make(chan delivery, 16)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := startConsumer(runCtx, t, c, func(hctx context.Context, d *askerv1.Document) error {
		tc, err := tenancy.FromContext(hctx)
		if err != nil {
			t.Errorf("handler ctx missing tenancy: %v", err)
		}
		got <- delivery{doc: d, tc: tc}
		return nil
	})

	select {
	case d := <-got:
		if !proto.Equal(d.doc, doc) {
			t.Errorf("consumed document = %v, want %v", d.doc, doc)
		}
		if d.tc.TenantID() != "tenant-a" {
			t.Errorf("handler tenant = %q, want %q", d.tc.TenantID(), "tenant-a")
		}
		if d.tc.Subject() != "tenant-a" {
			t.Errorf("handler subject = %q, want %q", d.tc.Subject(), "tenant-a")
		}
	case <-time.After(testWait):
		t.Fatal("timeout waiting for consumed document")
	}

	// ctx cancellation is a clean shutdown.
	cancel()
	waitRunStops(t, errc)
}

func TestProduceDocumentTenantEnforcement(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	ctx := context.Background()
	if err := EnsureTopics(ctx, cfg, 1, TopicDocsRaw); err != nil {
		t.Fatalf("EnsureTopics: %v", err)
	}
	p := newProducer(t, cfg)
	doc := testDoc("tenant-b", "doc-1", "etag-1")

	// Context tenant differs from document tenant.
	err := p.ProduceDocument(tenantCtx(t, "tenant-a"), TopicDocsRaw, doc)
	if err == nil || !strings.Contains(err.Error(), "tenant mismatch") {
		t.Errorf("mismatched tenant: got %v, want tenant mismatch error", err)
	}

	// No tenancy on the context at all.
	err = p.ProduceDocument(ctx, TopicDocsRaw, doc)
	if !errors.Is(err, tenancy.ErrNoTenant) {
		t.Errorf("no tenancy in ctx: got %v, want ErrNoTenant", err)
	}

	// Degenerate arguments.
	if err := p.ProduceDocument(tenantCtx(t, "tenant-b"), TopicDocsRaw, nil); err == nil {
		t.Error("nil document: want error, got nil")
	}
	if err := p.ProduceDocument(tenantCtx(t, "tenant-b"), "", doc); err == nil {
		t.Error("empty topic: want error, got nil")
	}

	// Nothing may have reached the topic.
	if recs := fetchRecords(t, cfg, TopicDocsRaw, 1, time.Second); len(recs) != 0 {
		t.Errorf("topic has %d records after rejected produces, want 0", len(recs))
	}
}

func TestHandlerFailureQuarantinesAndContinues(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	ctx := context.Background()
	if err := EnsureTopics(ctx, cfg, 1, TopicDocsRaw, TopicDocsDeadletter); err != nil {
		t.Fatalf("EnsureTopics: %v", err)
	}

	p := newProducer(t, cfg)
	docBad := testDoc("tenant-a", "doc-bad", "etag-1")
	docGood := testDoc("tenant-a", "doc-good", "etag-2")
	tctx := tenantCtx(t, "tenant-a")
	for _, d := range []*askerv1.Document{docBad, docGood} {
		if err := p.ProduceDocument(tctx, TopicDocsRaw, d); err != nil {
			t.Fatalf("ProduceDocument(%s): %v", d.GetDocId(), err)
		}
	}

	var badAttempts atomic.Int32
	good := make(chan *askerv1.Document, 16)
	c, err := NewConsumer(cfg, "group-dlq", TopicDocsRaw)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := startConsumer(runCtx, t, c, func(_ context.Context, d *askerv1.Document) error {
		if d.GetDocId() == "doc-bad" {
			badAttempts.Add(1)
			return errors.New("boom: simulated handler failure")
		}
		good <- d
		return nil
	})

	// Single partition + same key: doc-good is only delivered after doc-bad
	// exhausted its retries and was quarantined.
	select {
	case d := <-good:
		if !proto.Equal(d, docGood) {
			t.Errorf("good document = %v, want %v", d, docGood)
		}
	case <-time.After(testWait):
		t.Fatal("timeout waiting for the document after the poison one")
	}
	if got := badAttempts.Load(); got != 3 {
		t.Errorf("poison document handled %d times, want 3", got)
	}

	// The poison record landed on the dead letter with its original payload
	// and headers, plus error and origin_topic.
	recs := fetchRecords(t, cfg, TopicDocsDeadletter, 1, testWait)
	if len(recs) != 1 {
		t.Fatalf("deadletter has %d records, want 1", len(recs))
	}
	dl := recs[0]
	if got := string(dl.Key); got != "tenant-a" {
		t.Errorf("deadletter key = %q, want %q", got, "tenant-a")
	}
	var quarantined askerv1.Document
	if err := proto.Unmarshal(dl.Value, &quarantined); err != nil {
		t.Fatalf("unmarshal deadletter value: %v", err)
	}
	if !proto.Equal(&quarantined, docBad) {
		t.Errorf("deadletter document = %v, want original %v", &quarantined, docBad)
	}
	hm := headerMap(dl)
	if hm[HeaderTenantID] != "tenant-a" || hm[HeaderDocID] != "doc-bad" || hm[HeaderVersionEtag] != "etag-1" {
		t.Errorf("deadletter lost original headers: %v", hm)
	}
	if !strings.Contains(hm[HeaderError], "boom") || !strings.Contains(hm[HeaderError], "after 3 attempts") {
		t.Errorf("deadletter error header = %q, want handler failure after 3 attempts", hm[HeaderError])
	}
	if hm[HeaderOriginTopic] != TopicDocsRaw {
		t.Errorf("deadletter origin_topic = %q, want %q", hm[HeaderOriginTopic], TopicDocsRaw)
	}

	cancel()
	waitRunStops(t, errc)
}

func TestMalformedRecordsQuarantined(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	ctx := context.Background()
	if err := EnsureTopics(ctx, cfg, 1, TopicDocsRaw, TopicDocsDeadletter); err != nil {
		t.Fatalf("EnsureTopics: %v", err)
	}

	validValue, err := proto.Marshal(testDoc("tenant-a", "doc-x", "etag-1"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := rawClient(t, cfg)
	badRecords := []*kgo.Record{
		{ // undecodable proto, valid tenant header
			Topic: TopicDocsRaw,
			Key:   []byte("tenant-a"),
			Value: []byte{0x08}, // truncated varint: always a proto error
			Headers: []kgo.RecordHeader{
				{Key: HeaderTenantID, Value: []byte("tenant-a")},
				{Key: HeaderDocID, Value: []byte("doc-garbage")},
			},
		},
		{ // valid proto, no tenant header at all
			Topic: TopicDocsRaw,
			Key:   []byte("tenant-a"),
			Value: validValue,
		},
		{ // tenant header failing the syntax allowlist
			Topic: TopicDocsRaw,
			Key:   []byte("tenant-a"),
			Value: validValue,
			Headers: []kgo.RecordHeader{
				{Key: HeaderTenantID, Value: []byte("../evil")},
			},
		},
		{ // valid header that does not match the document tenant
			Topic: TopicDocsRaw,
			Key:   []byte("tenant-b"),
			Value: validValue,
			Headers: []kgo.RecordHeader{
				{Key: HeaderTenantID, Value: []byte("tenant-b")},
			},
		},
	}
	if err := raw.ProduceSync(ctx, badRecords...).FirstErr(); err != nil {
		t.Fatalf("produce malformed records: %v", err)
	}
	// A valid sentinel after the malformed ones; the topic has a single
	// partition, so seeing it proves the consumer worked through them all.
	p := newProducer(t, cfg)
	sentinel := testDoc("tenant-a", "doc-sentinel", "etag-s")
	if err := p.ProduceDocument(tenantCtx(t, "tenant-a"), TopicDocsRaw, sentinel); err != nil {
		t.Fatalf("ProduceDocument(sentinel): %v", err)
	}

	seen := make(chan string, 16)
	c, err := NewConsumer(cfg, "group-malformed", TopicDocsRaw)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := startConsumer(runCtx, t, c, func(_ context.Context, d *askerv1.Document) error {
		seen <- d.GetDocId()
		return nil
	})

	select {
	case id := <-seen:
		if id != "doc-sentinel" {
			t.Errorf("handler saw %q, want only %q (malformed records must bypass the handler)", id, "doc-sentinel")
		}
	case <-time.After(testWait):
		t.Fatal("timeout waiting for sentinel document")
	}

	recs := fetchRecords(t, cfg, TopicDocsDeadletter, 4, testWait)
	if len(recs) != 4 {
		t.Fatalf("deadletter has %d records, want 4", len(recs))
	}
	var errHeaders []string
	for _, r := range recs {
		hm := headerMap(r)
		if hm[HeaderOriginTopic] != TopicDocsRaw {
			t.Errorf("deadletter origin_topic = %q, want %q", hm[HeaderOriginTopic], TopicDocsRaw)
		}
		errHeaders = append(errHeaders, hm[HeaderError])
	}
	for _, want := range []string{
		"unmarshal document",             // undecodable proto
		"no tenant",                      // missing tenant header
		"invalid tenant claim",           // header failing the allowlist
		"does not match document tenant", // header/payload mismatch
	} {
		found := false
		for _, e := range errHeaders {
			if strings.Contains(e, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no deadletter error header contains %q; got %q", want, errHeaders)
		}
	}

	cancel()
	waitRunStops(t, errc)
}

// TestNoCommitPastUnhandledRecord verifies commit-after-success semantics: a
// consumer stopped mid-record (handled neither successfully nor via the dead
// letter) must not have committed past it, so a restarted group member sees
// it again — and never re-sees records that were fully handled.
func TestNoCommitPastUnhandledRecord(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	ctx := context.Background()
	if err := EnsureTopics(ctx, cfg, 1, TopicDocsRaw, TopicDocsDeadletter); err != nil {
		t.Fatalf("EnsureTopics: %v", err)
	}

	p := newProducer(t, cfg)
	tctx := tenantCtx(t, "tenant-a")
	for _, id := range []string{"d1", "d2", "d3"} {
		if err := p.ProduceDocument(tctx, TopicDocsRaw, testDoc("tenant-a", id, "etag-"+id)); err != nil {
			t.Fatalf("ProduceDocument(%s): %v", id, err)
		}
	}

	const group = "group-restart"

	// Phase 1: handle d1, then shut down while d2 is in flight.
	c1, err := NewConsumer(cfg, group, TopicDocsRaw)
	if err != nil {
		t.Fatalf("NewConsumer (phase 1): %v", err)
	}
	run1Ctx, cancel1 := context.WithCancel(ctx)
	defer cancel1()
	phase1 := make(chan string, 16)
	errc1 := startConsumer(run1Ctx, t, c1, func(_ context.Context, d *askerv1.Document) error {
		if d.GetDocId() == "d2" {
			cancel1() // simulate shutdown mid-processing
			return errors.New("interrupted")
		}
		phase1 <- d.GetDocId()
		return nil
	})
	select {
	case id := <-phase1:
		if id != "d1" {
			t.Fatalf("phase 1 handled %q first, want d1", id)
		}
	case <-time.After(testWait):
		t.Fatal("timeout waiting for d1 in phase 1")
	}
	waitRunStops(t, errc1)
	c1.Close()

	// d2 was interrupted, not poisoned: it must not be on the dead letter.
	if recs := fetchRecords(t, cfg, TopicDocsDeadletter, 1, time.Second); len(recs) != 0 {
		t.Errorf("deadletter has %d records after interrupted shutdown, want 0", len(recs))
	}

	// Phase 2: a new member of the same group resumes at d2 — the commit
	// never advanced past the unhandled record — and does not replay d1.
	c2, err := NewConsumer(cfg, group, TopicDocsRaw)
	if err != nil {
		t.Fatalf("NewConsumer (phase 2): %v", err)
	}
	run2Ctx, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	phase2 := make(chan string, 16)
	errc2 := startConsumer(run2Ctx, t, c2, func(_ context.Context, d *askerv1.Document) error {
		phase2 <- d.GetDocId()
		return nil
	})

	var got []string
	for len(got) < 2 {
		select {
		case id := <-phase2:
			got = append(got, id)
		case <-time.After(testWait):
			t.Fatalf("timeout in phase 2; handled so far: %v", got)
		}
	}
	if got[0] != "d2" || got[1] != "d3" {
		t.Errorf("phase 2 handled %v, want [d2 d3]", got)
	}
	select {
	case id := <-phase2:
		t.Errorf("phase 2 unexpectedly replayed %q", id)
	case <-time.After(500 * time.Millisecond):
	}

	cancel2()
	waitRunStops(t, errc2)
}

// TestDeadletterConsumerNeverRequarantines guards against quarantine loops: a
// consumer of docs.deadletter whose record fails processing commits it
// without producing it back onto the same topic.
func TestDeadletterConsumerNeverRequarantines(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	ctx := context.Background()
	if err := EnsureTopics(ctx, cfg, 1, TopicDocsDeadletter); err != nil {
		t.Fatalf("EnsureTopics: %v", err)
	}

	// A malformed record sitting on the dead letter (as quarantine produces
	// them), followed by a well-formed one.
	raw := rawClient(t, cfg)
	bad := &kgo.Record{
		Topic: TopicDocsDeadletter,
		Key:   []byte("tenant-a"),
		Value: []byte{0x08},
		Headers: []kgo.RecordHeader{
			{Key: HeaderTenantID, Value: []byte("tenant-a")},
			{Key: HeaderError, Value: []byte("previous failure")},
			{Key: HeaderOriginTopic, Value: []byte(TopicDocsRaw)},
		},
	}
	if err := raw.ProduceSync(ctx, bad).FirstErr(); err != nil {
		t.Fatalf("produce malformed deadletter record: %v", err)
	}
	p := newProducer(t, cfg)
	good := testDoc("tenant-a", "doc-replayable", "etag-1")
	if err := p.ProduceDocument(tenantCtx(t, "tenant-a"), TopicDocsDeadletter, good); err != nil {
		t.Fatalf("ProduceDocument: %v", err)
	}

	seen := make(chan string, 16)
	c, err := NewConsumer(cfg, "group-dlq-reader", TopicDocsDeadletter)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := startConsumer(runCtx, t, c, func(_ context.Context, d *askerv1.Document) error {
		seen <- d.GetDocId()
		return nil
	})

	select {
	case id := <-seen:
		if id != "doc-replayable" {
			t.Errorf("handler saw %q, want %q", id, "doc-replayable")
		}
	case <-time.After(testWait):
		t.Fatal("timeout waiting for well-formed deadletter record")
	}

	// Exactly the two records we produced; the malformed one was not
	// re-quarantined onto the same topic.
	if recs := fetchRecords(t, cfg, TopicDocsDeadletter, 3, time.Second); len(recs) != 2 {
		t.Errorf("deadletter has %d records, want 2 (no re-quarantine loop)", len(recs))
	}

	cancel()
	waitRunStops(t, errc)
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("KAFKA_BROKERS", "b1:9092,b2:9092")
	t.Setenv("KAFKA_CLIENT_ID", "svc-x")
	var cfg Config
	if err := config.Load("", &cfg); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(cfg.Brokers) != 2 || cfg.Brokers[0] != "b1:9092" || cfg.Brokers[1] != "b2:9092" {
		t.Errorf("Brokers = %v, want [b1:9092 b2:9092]", cfg.Brokers)
	}
	if cfg.ClientID != "svc-x" {
		t.Errorf("ClientID = %q, want %q", cfg.ClientID, "svc-x")
	}
}

func TestConfigEnvDefaults(t *testing.T) {
	// t.Setenv registers restoration of the original value; unset afterwards
	// to exercise the envDefault path.
	t.Setenv("KAFKA_BROKERS", "placeholder")
	if err := os.Unsetenv("KAFKA_BROKERS"); err != nil {
		t.Fatalf("os.Unsetenv: %v", err)
	}
	var cfg Config
	if err := config.Load("", &cfg); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(cfg.Brokers) != 1 || cfg.Brokers[0] != "redpanda:9092" {
		t.Errorf("Brokers = %v, want dev default [redpanda:9092]", cfg.Brokers)
	}
}

func TestConstructorValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewProducer(Config{}); err == nil {
		t.Error("NewProducer with no brokers: want error, got nil")
	}
	cfg := Config{Brokers: []string{"127.0.0.1:1"}} // never dialed in this test
	if _, err := NewConsumer(cfg, "", TopicDocsRaw); err == nil {
		t.Error("NewConsumer with empty group: want error, got nil")
	}
	if _, err := NewConsumer(cfg, "group"); err == nil {
		t.Error("NewConsumer with no topics: want error, got nil")
	}
	if _, err := NewConsumer(Config{}, "group", TopicDocsRaw); err == nil {
		t.Error("NewConsumer with no brokers: want error, got nil")
	}

	c, err := NewConsumer(cfg, "group", TopicDocsRaw)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	defer c.Close()
	if err := c.Run(context.Background(), nil); err == nil {
		t.Error("Run with nil handler: want error, got nil")
	}
}
