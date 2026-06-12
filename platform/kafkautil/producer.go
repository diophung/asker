package kafkautil

import (
	"context"
	"errors"
	"fmt"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// Producer publishes canonical Documents to pipeline topics. It is safe for
// concurrent use. Close it when done.
type Producer struct {
	cl *kgo.Client
}

// NewProducer connects a producer to the brokers in cfg.
func NewProducer(cfg Config) (*Producer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cl, err := kgo.NewClient(cfg.clientOpts()...)
	if err != nil {
		return nil, fmt.Errorf("kafkautil: new producer: %w", err)
	}
	return &Producer{cl: cl}, nil
}

// ProduceDocument publishes doc to topic, keyed by doc.TenantId, with
// tenant_id/doc_id/version_etag record headers (ADR-004).
//
// This is a tenancy chokepoint: ctx must carry a tenancy.Context (see
// tenancy.WithContext) whose tenant matches doc.TenantId, otherwise the
// produce is refused. The produce is synchronous — correctness over
// throughput for M1; batched async production is M5 work.
func (p *Producer) ProduceDocument(ctx context.Context, topic string, doc *askerv1.Document) error {
	if topic == "" {
		return errors.New("kafkautil: produce document: empty topic")
	}
	if doc == nil {
		return fmt.Errorf("kafkautil: produce document to %q: nil document", topic)
	}

	tc, err := tenancy.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("kafkautil: produce document to %q: %w", topic, err)
	}
	if string(tc.TenantID()) != doc.GetTenantId() {
		return fmt.Errorf("kafkautil: produce document to %q: tenant mismatch: context tenant %q, document tenant %q",
			topic, tc.TenantID(), doc.GetTenantId())
	}

	value, err := proto.Marshal(doc)
	if err != nil {
		return fmt.Errorf("kafkautil: produce document to %q: marshal: %w", topic, err)
	}

	rec := &kgo.Record{
		Topic: topic,
		// Keying by tenant gives per-tenant ordering: a tombstone can never
		// overtake the upsert it revokes within a tenant (ADR-004).
		Key:   []byte(doc.GetTenantId()),
		Value: value,
		Headers: []kgo.RecordHeader{
			{Key: HeaderTenantID, Value: []byte(doc.GetTenantId())},
			{Key: HeaderDocID, Value: []byte(doc.GetDocId())},
			{Key: HeaderVersionEtag, Value: []byte(doc.GetVersionEtag())},
		},
	}
	if err := p.cl.ProduceSync(ctx, rec).FirstErr(); err != nil {
		return fmt.Errorf("kafkautil: produce document to %q: %w", topic, err)
	}
	return nil
}

// Close releases the underlying client. Pending synchronous produces have
// already completed by the time their ProduceDocument call returned.
func (p *Producer) Close() {
	p.cl.Close()
}
