package ical

import (
	"context"
	"fmt"
	"strings"

	"github.com/asker/asker/connectors/sdk"
)

// FullSync implements sdk.Connector: it GETs the feed once, parses every VEVENT,
// emits one CALENDAR_EVENT Document each, and returns a cursor that fingerprints
// the feed (sha256 of the body + the max DTSTAMP + a per-UID etag map).
//
// A feed is a single document, not a paginated collection, so there is exactly
// one page. The build contract still asks FullSync to checkpoint per page; we
// checkpoint the final cursor once the whole feed has been emitted, which is the
// only resumable position a single-request source has.
func (c *Connector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	c.log.Info("ical full sync starting", "instance_id", cfg.InstanceID)

	body, err := c.fetch(ctx, conf.FeedURL)
	if err != nil {
		return "", redactURL(err, conf.FeedURL)
	}
	events := parseEvents(body)
	tenant := string(cfg.Tenant.TenantID())

	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if strings.TrimSpace(ev.value("UID")) == "" {
			// A VEVENT without a UID cannot be tracked across syncs (its doc_id
			// would not be stable), so it is skipped. This is rare in real feeds.
			c.log.Warn("ical: skipping VEVENT without UID", "instance_id", cfg.InstanceID)
			continue
		}
		if err := emit(ctx, c.eventDocument(tenant, conf.FeedURL, ev)); err != nil {
			return "", err
		}
	}

	cur := buildCursorState(body, events).encode()
	if err := cfg.Checkpoint(ctx, cur); err != nil {
		return "", err
	}
	c.log.Info("ical full sync complete", "instance_id", cfg.InstanceID, "events", len(events))
	return cur, nil
}

// IncrementalSync implements sdk.Connector: it re-fetches the feed and diffs it
// against the baseline encoded in cur.
//
//   - If the new body hashes identically to cur's hash, nothing changed: the same
//     cursor is returned with no emissions.
//   - Otherwise every event whose UID is new, or whose version_etag differs from
//     the baseline, is emitted as an upsert; every event with a canceled STATUS
//     is emitted as a tombstone; and every UID present at the baseline but absent
//     from the new feed is emitted as a tombstone (the event was removed).
//
// An unparseable cursor surfaces as sdk.ErrCursorExpired so the hub restarts a
// full sync rather than looping on a poison cursor.
func (c *Connector) IncrementalSync(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	prev, err := parseCursor(cur)
	if err != nil {
		return "", wrapExpired(err)
	}

	body, err := c.fetch(ctx, conf.FeedURL)
	if err != nil {
		return "", redactURL(err, conf.FeedURL)
	}

	if feedHash(body) == prev.Hash {
		// Byte-identical feed: nothing changed. Return the cursor unchanged.
		c.log.Info("ical incremental sync: feed unchanged", "instance_id", cfg.InstanceID)
		return cur, nil
	}

	events := parseEvents(body)
	tenant := string(cfg.Tenant.TenantID())

	// Track which baseline UIDs we still see, so the leftovers are deletions.
	present := make(map[string]bool, len(events))

	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		uid := strings.TrimSpace(ev.value("UID"))
		if uid == "" {
			continue
		}
		present[uid] = true

		canceled := strings.EqualFold(strings.TrimSpace(ev.value("STATUS")), statusCancelled)
		etag := versionEtag(ev)
		prevEtag, known := prev.Events[uid]

		switch {
		case canceled:
			// A canceled event is always a tombstone (eventDocument handles it).
			if err := emit(ctx, c.eventDocument(tenant, conf.FeedURL, ev)); err != nil {
				return "", err
			}
		case !known || prevEtag != etag:
			// New or changed event -> upsert.
			if err := emit(ctx, c.eventDocument(tenant, conf.FeedURL, ev)); err != nil {
				return "", err
			}
		default:
			// Unchanged event: skip (it is still present, no emission needed).
		}
	}

	// UIDs present at the baseline but gone now were deleted from the feed.
	for _, uid := range sortedUIDs(prev.Events) {
		if present[uid] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := emit(ctx, c.disappearedTombstone(tenant, conf.FeedURL, uid)); err != nil {
			return "", err
		}
	}

	cur = buildCursorState(body, events).encode()
	c.log.Info("ical incremental sync complete", "instance_id", cfg.InstanceID, "events", len(events))
	return cur, nil
}

// wrapExpired wraps a cursor-parse failure as sdk.ErrCursorExpired so the hub
// clears the cursor and reruns FullSync.
func wrapExpired(err error) error {
	return fmt.Errorf("%v: %w", err, sdk.ErrCursorExpired)
}
