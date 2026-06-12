package kafkautil

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Pipeline topic names (ADR-004). Every stage consumes and produces the
// canonical asker.v1.Document; there are no stage-private message types.
const (
	// TopicDocsRaw carries documents exactly as connectors emitted them.
	TopicDocsRaw = "docs.raw"
	// TopicDocsChunked carries documents after chunking.
	TopicDocsChunked = "docs.chunked"
	// TopicDocsEnriched carries documents after enrichment (embeddings etc.).
	TopicDocsEnriched = "docs.enriched"
	// TopicDocsDeadletter quarantines documents that could not be processed.
	TopicDocsDeadletter = "docs.deadletter"
)

// Record header keys. tenant_id, doc_id and version_etag travel on every
// document record; error and origin_topic are added when a record is
// quarantined to the dead-letter topic.
const (
	HeaderTenantID    = "tenant_id"
	HeaderDocID       = "doc_id"
	HeaderVersionEtag = "version_etag"
	HeaderError       = "error"
	HeaderOriginTopic = "origin_topic"
)

// Config holds the broker connection settings shared by producers, consumers
// and EnsureTopics. Populate it with platform/config:
//
//	var cfg kafkautil.Config
//	err := config.Load("", &cfg) // reads KAFKA_BROKERS (comma-separated)
type Config struct {
	// Brokers is the comma-separated seed broker list. The dev default is
	// the in-network Redpanda address.
	Brokers []string `env:"KAFKA_BROKERS" envDefault:"redpanda:9092"`
	// ClientID identifies this client in broker logs and quotas. Optional.
	ClientID string `env:"KAFKA_CLIENT_ID"`
}

// validate reports whether the config is usable.
func (c Config) validate() error {
	if len(c.Brokers) == 0 {
		return errors.New("kafkautil: config: at least one broker required")
	}
	for _, b := range c.Brokers {
		if strings.TrimSpace(b) == "" {
			return errors.New("kafkautil: config: empty broker address")
		}
	}
	return nil
}

// clientOpts translates the config into base kgo client options.
func (c Config) clientOpts() []kgo.Opt {
	opts := []kgo.Opt{kgo.SeedBrokers(c.Brokers...)}
	if c.ClientID != "" {
		opts = append(opts, kgo.ClientID(c.ClientID))
	}
	return opts
}

// EnsureTopics idempotently creates the given topics with the given partition
// count, using the broker-default replication factor. Topics that already
// exist are left untouched (their partition counts are NOT reconciled).
// Services call this at startup before producing or consuming; remember to
// include TopicDocsDeadletter wherever a Consumer runs.
func EnsureTopics(ctx context.Context, cfg Config, partitions int32, topics ...string) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	if partitions <= 0 {
		return fmt.Errorf("kafkautil: ensure topics: partitions must be positive, got %d", partitions)
	}
	if len(topics) == 0 {
		return nil
	}

	cl, err := kgo.NewClient(cfg.clientOpts()...)
	if err != nil {
		return fmt.Errorf("kafkautil: ensure topics: create client: %w", err)
	}
	defer cl.Close()

	// Replication factor -1 = broker default (Kafka 2.4+ / Redpanda).
	resps, err := kadm.NewClient(cl).CreateTopics(ctx, partitions, -1, nil, topics...)
	if err != nil {
		return fmt.Errorf("kafkautil: ensure topics: %w", err)
	}
	for _, r := range resps.Sorted() {
		if r.Err != nil && !errors.Is(r.Err, kerr.TopicAlreadyExists) {
			return fmt.Errorf("kafkautil: ensure topic %q: %w", r.Topic, r.Err)
		}
	}
	return nil
}
