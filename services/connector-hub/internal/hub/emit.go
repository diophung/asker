package hub

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// docProducer is the slice of *kafkautil.Producer the hub needs; an
// interface so tests can record produces without a broker.
type docProducer interface {
	ProduceDocument(ctx context.Context, topic string, doc *askerv1.Document) error
}

// emitter is THE hub chokepoint between connectors and Kafka. Every document
// a connector emits — from FullSync, IncrementalSync, HandleWebhook, or the
// /upload endpoint — passes through emitFor, which enforces the SDK Emit
// contract on the hub side:
//
//   - doc.tenant_id MUST equal the instance's tenant; anything else is
//     rejected before it can reach a topic (the kafkautil producer would
//     also refuse, but the hub fails first and loudly).
//   - ts.ingested is stamped here, at emit time (connectors leave it unset).
//   - connector_id is filled from the instance when the connector left it
//     empty.
type emitter struct {
	producer docProducer
	topic    string
	now      func() time.Time
}

func newEmitter(producer docProducer, topic string, now func() time.Time) *emitter {
	if now == nil {
		now = time.Now
	}
	return &emitter{producer: producer, topic: topic, now: now}
}

// emitFor returns the sdk.Emit-shaped callback for one connector instance.
// emitted, when non-nil, is incremented once per successfully produced
// document (the scheduler accumulates it into SyncState.docs_emitted).
func (e *emitter) emitFor(tc tenancy.Context, connectorID string, emitted *atomic.Int64) func(ctx context.Context, doc *askerv1.Document) error {
	return func(ctx context.Context, doc *askerv1.Document) error {
		if doc == nil {
			return errors.New("hub: emit: nil document")
		}
		if doc.GetTenantId() != string(tc.TenantID()) {
			// A connector trying to write into another tenant is the exact
			// failure mode the chokepoint exists for. Reject; do not "fix".
			return fmt.Errorf("hub: emit: document tenant %q does not match instance tenant %q (doc_id %q, connector %q)",
				doc.GetTenantId(), tc.TenantID(), doc.GetDocId(), connectorID)
		}
		if doc.GetConnectorId() == "" {
			doc.ConnectorId = connectorID
		}
		if doc.Ts == nil {
			doc.Ts = &askerv1.Timestamps{}
		}
		doc.Ts.Ingested = timestamppb.New(e.now().UTC())

		// Install the instance tenant for the producer's own tenancy check;
		// whatever the connector put on ctx is not a verified identity.
		if err := e.producer.ProduceDocument(tenancy.WithContext(ctx, tc), e.topic, doc); err != nil {
			return fmt.Errorf("hub: emit doc %q: %w", doc.GetDocId(), err)
		}
		if emitted != nil {
			emitted.Add(1)
		}
		return nil
	}
}
