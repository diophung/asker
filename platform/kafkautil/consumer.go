package kafkautil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"github.com/asker/asker/platform/tenancy"
)

// meterName scopes kafkautil's OTel instruments. Uses the global MeterProvider,
// so it is a no-op until (and unless) telemetry.Init installs a real provider.
const meterName = "github.com/asker/asker/platform/kafkautil"

// Handler processes one document. The ctx it receives carries the
// tenancy.Context reconstructed from the record's tenant_id header
// (tenancy.FromContext will succeed). Returning an error triggers the retry /
// dead-letter policy described on Consumer.Run.
type Handler func(ctx context.Context, doc *askerv1.Document) error

// Retry policy for handler failures: maxHandlerAttempts total attempts with
// capped exponential backoff between them. The 2s entry is the cap should
// the attempt count ever grow.
const maxHandlerAttempts = 3

var handlerBackoff = []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second}

// Consumer is a consumer-group member that delivers canonical Documents to a
// Handler with at-least-once semantics. Create one per service instance and
// drive it with Run; Close it when done.
type Consumer struct {
	cl  *kgo.Client
	log *slog.Logger
	// deadletter counts records newly quarantined to docs.deadletter, labeled by
	// origin_topic only (NEVER tenant/doc — cardinality-safe across ~10M tenants).
	// This is the authoritative "data quarantined" signal for the zero-data-loss
	// SLO, distinct from a per-attempt handler-failure rate (M5).
	deadletter metric.Int64Counter
}

// NewConsumer joins the given consumer group on the given topics. Offsets are
// committed explicitly by Run (never automatically); brand-new groups start
// from the beginning of each topic so documents produced before the first
// consumer came up are not lost.
func NewConsumer(cfg Config, group string, topics ...string) (*Consumer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if group == "" {
		return nil, errors.New("kafkautil: new consumer: empty group")
	}
	if len(topics) == 0 {
		return nil, errors.New("kafkautil: new consumer: at least one topic required")
	}

	opts := append(cfg.clientOpts(),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		// Commit-after-success is the whole contract; nothing may commit
		// behind the handler's back.
		kgo.DisableAutoCommit(),
	)
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("kafkautil: new consumer: %w", err)
	}
	deadletter, err := otel.Meter(meterName).Int64Counter("asker_pipeline_deadletter",
		metric.WithDescription("Documents newly quarantined to the dead-letter topic."),
		metric.WithUnit("{record}"),
	)
	if err != nil {
		otel.Handle(err)
	}
	return &Consumer{
		cl:         cl,
		log:        slog.Default().With("component", "kafkautil.consumer", "group", group),
		deadletter: deadletter,
	}, nil
}

// Run polls records and delivers them to h until ctx is canceled (clean
// shutdown, returns nil) or an unrecoverable error occurs (returns it;
// nothing was committed for the failing record, so it will be redelivered).
//
// Delivery contract (ADR-004), per record:
//
//   - The tenant_id header is re-validated and installed on the handler ctx
//     as a tenancy.Context; the proto is decoded. Records failing either
//     check are malformed and go straight to docs.deadletter.
//   - Handler errors are retried in-process with capped exponential backoff
//     (3 attempts). On exhaustion the record is produced to docs.deadletter
//     with its original key/value/headers plus error and origin_topic
//     headers (poison-document quarantine).
//   - The record's offset is committed only after handler success or
//     successful quarantine — at-least-once, never skipped silently. If the
//     dead-letter produce fails, Run returns the error without committing.
func (c *Consumer) Run(ctx context.Context, h Handler) error {
	if h == nil {
		return errors.New("kafkautil: consumer run: nil handler")
	}
	for {
		fetches := c.cl.PollFetches(ctx)
		if fetches.IsClientClosed() {
			return nil
		}
		if ctx.Err() != nil {
			return nil // clean shutdown
		}
		fetches.EachError(func(topic string, partition int32, err error) {
			// Transient fetch errors (rebalances, broker restarts) resolve on
			// the next poll; log for visibility.
			c.log.Warn("fetch error", "topic", topic, "partition", partition, "error", err)
		})

		// Records are processed and committed strictly in order within the
		// fetch, so a commit never advances past an unhandled record.
		iter := fetches.RecordIter()
		for !iter.Done() {
			if err := c.processRecord(ctx, iter.Next(), h); err != nil {
				if ctx.Err() != nil {
					return nil // clean shutdown mid-record; it will be redelivered
				}
				return err
			}
		}
	}
}

// processRecord applies the full per-record contract. It returns an error
// only when the record could be neither handled nor quarantined (the caller
// must not commit past it) or when ctx was canceled.
func (c *Consumer) processRecord(ctx context.Context, rec *kgo.Record, h Handler) error {
	tc, err := tenantFromRecord(rec)
	if err != nil {
		return c.quarantine(ctx, rec, fmt.Errorf("invalid tenant header: %w", err))
	}
	doc := new(askerv1.Document)
	if err := proto.Unmarshal(rec.Value, doc); err != nil {
		return c.quarantine(ctx, rec, fmt.Errorf("unmarshal document: %w", err))
	}
	// Defense in depth: the header (which routed the record) and the payload
	// must agree, or the document would be processed under the wrong tenant.
	if doc.GetTenantId() != string(tc.TenantID()) {
		return c.quarantine(ctx, rec, fmt.Errorf("tenant header %q does not match document tenant %q",
			tc.TenantID(), doc.GetTenantId()))
	}

	hctx := tenancy.WithContext(ctx, tc)
	var lastErr error
	for attempt := 1; attempt <= maxHandlerAttempts; attempt++ {
		if attempt > 1 {
			if err := sleepCtx(ctx, backoffFor(attempt-2)); err != nil {
				return err
			}
		}
		lastErr = h(hctx, doc)
		if lastErr == nil {
			c.commit(ctx, rec)
			return nil
		}
		c.log.Warn("handler failed",
			"topic", rec.Topic, "partition", rec.Partition, "offset", rec.Offset,
			"doc_id", doc.GetDocId(), "attempt", attempt, "max_attempts", maxHandlerAttempts,
			"error", lastErr)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return c.quarantine(ctx, rec, fmt.Errorf("handler failed after %d attempts: %w", maxHandlerAttempts, lastErr))
}

// quarantine produces rec to docs.deadletter preserving its key, value and
// headers, adding error and origin_topic headers, then commits the original
// record. If the dead-letter produce fails, the record is NOT committed and
// the error is returned so it is redelivered rather than silently skipped.
func (c *Consumer) quarantine(ctx context.Context, rec *kgo.Record, cause error) error {
	if rec.Topic == TopicDocsDeadletter {
		// A consumer reading the dead-letter topic itself (replay tooling)
		// must never re-quarantine into the same topic: the record is
		// already quarantined, so log loudly and move on.
		c.log.Error("dead-letter record failed processing; committing without re-quarantine",
			"partition", rec.Partition, "offset", rec.Offset, "error", cause)
		c.commit(ctx, rec)
		return nil
	}

	headers := make([]kgo.RecordHeader, 0, len(rec.Headers)+2)
	headers = append(headers, rec.Headers...)
	headers = append(headers,
		kgo.RecordHeader{Key: HeaderError, Value: []byte(cause.Error())},
		kgo.RecordHeader{Key: HeaderOriginTopic, Value: []byte(rec.Topic)},
	)
	dl := &kgo.Record{
		Topic:   TopicDocsDeadletter,
		Key:     rec.Key,
		Value:   rec.Value,
		Headers: headers,
	}
	if err := c.cl.ProduceSync(ctx, dl).FirstErr(); err != nil {
		return fmt.Errorf("kafkautil: quarantine %s[%d]@%d to %s: %w",
			rec.Topic, rec.Partition, rec.Offset, TopicDocsDeadletter, err)
	}
	if c.deadletter != nil {
		c.deadletter.Add(ctx, 1, metric.WithAttributes(attribute.String("origin_topic", rec.Topic)))
	}
	c.log.Error("record quarantined to dead-letter topic",
		"origin_topic", rec.Topic, "partition", rec.Partition, "offset", rec.Offset, "error", cause)
	c.commit(ctx, rec)
	return nil
}

// commit synchronously commits rec's offset. A failed commit (rebalance,
// broker hiccup) is logged, not fatal: at-least-once means the record may be
// redelivered, which downstream idempotent writes absorb (ADR-004).
func (c *Consumer) commit(ctx context.Context, rec *kgo.Record) {
	if err := c.cl.CommitRecords(ctx, rec); err != nil {
		c.log.Warn("offset commit failed; record may be redelivered",
			"topic", rec.Topic, "partition", rec.Partition, "offset", rec.Offset, "error", err)
	}
}

// Close leaves the consumer group and releases the underlying client. Any
// concurrent Run returns nil once the client is closed.
func (c *Consumer) Close() {
	c.cl.Close()
}

// tenantFromRecord reconstructs the tenancy.Context from the record's
// tenant_id header.
func tenantFromRecord(rec *kgo.Record) (tenancy.Context, error) {
	for _, h := range rec.Headers {
		if h.Key == HeaderTenantID {
			return tenancy.FromHeaderValue(string(h.Value))
		}
	}
	return tenancy.Context{}, tenancy.ErrNoTenant
}

// backoffFor returns the capped exponential backoff delay for the i-th retry
// wait (0-based).
func backoffFor(i int) time.Duration {
	if i >= len(handlerBackoff) {
		i = len(handlerBackoff) - 1
	}
	return handlerBackoff[i]
}

// sleepCtx sleeps for d, returning early with ctx.Err() if ctx is canceled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
