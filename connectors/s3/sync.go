package s3

import (
	"context"
	"fmt"
	"time"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// FullSync implements sdk.Connector: it walks every object under the configured
// prefix in key order, emitting a FILE Document per object, and checkpoints a
// resumable cursor every checkpointEvery objects so an interrupted backfill
// resumes (via ListObjects StartAfter) instead of restarting. It returns the
// steady-state cursor (max LastModified + full keyset) IncrementalSync resumes
// from.
func (c *Connector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	cl, err := c.newClient(conf, cfg.Token)
	if err != nil {
		return "", err
	}
	c.log.Info("s3 full sync starting", "instance_id", cfg.InstanceID, "bucket", conf.Bucket, "prefix", conf.Prefix)
	return c.backfill(ctx, cfg, cl, emit, conf.Bucket, "")
}

// checkpointEvery is how many emitted objects pass between FullSync checkpoints.
const checkpointEvery = 200

// backfill lists from startAfter (empty = beginning), emits a Document per
// object, accumulates the keyset + max LastModified, and checkpoints
// periodically. It is shared by FullSync (startAfter "") and by IncrementalSync
// when the hub replays a mid-backfill checkpoint cursor.
func (c *Connector) backfill(ctx context.Context, cfg sdk.Config, cl *client, emit sdk.Emit, bucket, startAfter string) (sdk.Cursor, error) {
	tenant := string(cfg.Tenant.TenantID())
	var (
		keys    []string
		maxMod  time.Time
		lastKey = startAfter
		since   int
	)

	err := cl.list(ctx, startAfter, func(o object) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		doc, err := c.documentForObject(ctx, cl, tenant, bucket, o)
		if err != nil {
			return err
		}
		if err := emit(ctx, doc); err != nil {
			return err
		}
		keys = append(keys, o.key)
		lastKey = o.key
		if o.lastModified.After(maxMod) {
			maxMod = o.lastModified
		}
		since++
		if since >= checkpointEvery {
			since = 0
			if err := cfg.Checkpoint(ctx, checkpointCursor(lastKey, maxMod, keys)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	cur := completedCursor(maxMod, keys)
	if err := cfg.Checkpoint(ctx, cur); err != nil {
		return "", err
	}
	c.log.Info("s3 backfill complete", "instance_id", cfg.InstanceID, "objects", len(keys))
	return cur, nil
}

// IncrementalSync implements sdk.Connector. S3 has no change feed, so the
// connector re-lists and reconciles against the cursor:
//
//   - a mid-backfill checkpoint cursor (non-empty LastKey) resumes the
//     interrupted backfill at that key (the hub replays checkpoints verbatim
//     into IncrementalSync per the sdk.Cursor contract);
//   - otherwise it re-lists the whole prefix, emits an upsert for every object
//     whose LastModified is strictly newer than the cursor's max, and emits a
//     tombstone for every key in the cursor's keyset that has since vanished.
//
// An unparseable cursor surfaces as sdk.ErrCursorExpired so the hub restarts a
// full sync rather than looping on a poison cursor.
func (c *Connector) IncrementalSync(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	st, err := parseCursor(cur)
	if err != nil {
		return "", fmt.Errorf("%v: %w", err, sdk.ErrCursorExpired)
	}
	cl, err := c.newClient(conf, cfg.Token)
	if err != nil {
		return "", err
	}
	if st.LastKey != "" {
		c.log.Info("resuming interrupted s3 backfill", "instance_id", cfg.InstanceID, "after_key", st.LastKey)
		return c.backfill(ctx, cfg, cl, emit, conf.Bucket, st.LastKey)
	}
	return c.reconcile(ctx, cfg, cl, emit, conf.Bucket, st)
}

// reconcile re-lists the prefix and diffs it against the prior pass's cursor.
// It emits an upsert for each object newer than the recorded max LastModified
// and a tombstone for each previously-seen key now absent, returning the
// advanced steady-state cursor.
func (c *Connector) reconcile(ctx context.Context, cfg sdk.Config, cl *client, emit sdk.Emit, bucket string, st cursorState) (sdk.Cursor, error) {
	tenant := string(cfg.Tenant.TenantID())
	prevKeys := st.keySet()
	prevMax := st.maxModifiedTime()

	var (
		keys   []string
		maxMod = prevMax
	)
	seen := make(map[string]struct{})

	err := cl.list(ctx, "", func(o object) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		keys = append(keys, o.key)
		seen[o.key] = struct{}{}
		if o.lastModified.After(maxMod) {
			maxMod = o.lastModified
		}
		// Upsert only objects strictly newer than the last pass's high-water
		// mark; unchanged objects are skipped so a steady poll emits nothing.
		if o.lastModified.After(prevMax) {
			doc, err := c.documentForObject(ctx, cl, tenant, bucket, o)
			if err != nil {
				return err
			}
			if err := emit(ctx, doc); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	// Deletions: any key present last pass but absent now is a tombstone.
	for key := range prevKeys {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if _, ok := seen[key]; ok {
			continue
		}
		if err := emit(ctx, c.tombstoneDocument(tenant, bucket, key)); err != nil {
			return "", err
		}
	}

	cur := completedCursor(maxMod, keys)
	c.log.Info("s3 incremental sync complete", "instance_id", cfg.InstanceID, "objects", len(keys))
	return cur, nil
}

// documentForObject fetches the body for small text objects (one GET, capped at
// bodyCap) and maps the object to a FILE Document. A body-fetch error degrades
// to a metadata-only document rather than failing the whole sync.
func (c *Connector) documentForObject(ctx context.Context, cl *client, tenant, bucket string, o object) (*askerv1.Document, error) {
	if !shouldFetchBody(o) {
		return objectDocument(tenant, bucket, o, "", ""), nil
	}
	bctx, cancel := context.WithTimeout(ctx, clientTimeout)
	defer cancel()
	body, contentType, err := cl.fetchBody(bctx, o.key)
	if err != nil {
		c.log.Warn("s3 body fetch failed; indexing metadata only", "key", o.key, "error", err)
		return objectDocument(tenant, bucket, o, "", ""), nil
	}
	return objectDocument(tenant, bucket, o, body, contentType), nil
}
