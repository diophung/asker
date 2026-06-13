package msteams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/asker/asker/connectors/sdk"
)

// deltaCursor is the JSON-encoded incremental cursor: a per-chat map from chat
// id to the Graph deltaLink that resumes that chat's /messages/delta feed.
type deltaCursor struct {
	Deltas map[string]string `json:"deltas"`
}

// encodeCursor marshals a deltaCursor to an sdk.Cursor.
func encodeCursor(dc deltaCursor) (sdk.Cursor, error) {
	if dc.Deltas == nil {
		dc.Deltas = map[string]string{}
	}
	raw, err := json.Marshal(dc)
	if err != nil {
		return "", fmt.Errorf("msteams: encode cursor: %w", err)
	}
	return sdk.Cursor(raw), nil
}

// decodeCursor parses an sdk.Cursor. An unparseable cursor is treated as
// expired so the hub restarts a full sync.
func decodeCursor(cur sdk.Cursor) (deltaCursor, error) {
	var dc deltaCursor
	if cur == "" {
		dc.Deltas = map[string]string{}
		return dc, nil
	}
	if err := json.Unmarshal([]byte(cur), &dc); err != nil {
		return dc, fmt.Errorf("msteams: cursor is not decodable (%v): %w", err, sdk.ErrCursorExpired)
	}
	if dc.Deltas == nil {
		dc.Deltas = map[string]string{}
	}
	return dc, nil
}

// listResponse is the Graph collection envelope: a value array plus the OData
// continuation links. nextLink paginates a list; deltaLink terminates a delta
// feed.
type listResponse[T any] struct {
	Value     []T    `json:"value"`
	NextLink  string `json:"@odata.nextLink"`
	DeltaLink string `json:"@odata.deltaLink"`
}

// FullSync implements sdk.Connector. It lists the signed-in user's chats
// (GET /me/chats?$expand=members), pages every chat's messages
// (GET /chats/{id}/messages), emits a Document per message, and checkpoints
// after each completed chat. After backfilling a chat it primes that chat's
// delta link by draining /messages/delta, so the returned cursor lets the first
// IncrementalSync continue exactly where the backfill stopped. Convergence for
// messages that race the backfill is via idempotent (doc_id, version_etag)
// upserts.
func (c *Connector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	gc := newGraphClient(conf, cfg.Token)
	tenant := string(cfg.Tenant.TenantID())

	chats, err := c.listChats(ctx, gc)
	if err != nil {
		return "", err
	}
	c.log.Info("msteams full sync starting", "instance_id", cfg.InstanceID, "chats", len(chats))

	deltas := make(map[string]string, len(chats))
	for _, chat := range chats {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := c.backfillChat(ctx, gc, tenant, chat, emit); err != nil {
			return "", err
		}
		link, err := c.primeDelta(ctx, gc, chat.ID)
		if err != nil {
			return "", err
		}
		deltas[chat.ID] = link
		if cur, cerr := encodeCursor(deltaCursor{Deltas: deltas}); cerr == nil {
			if err := cfg.Checkpoint(ctx, cur); err != nil {
				return "", err
			}
		}
	}

	cur, err := encodeCursor(deltaCursor{Deltas: deltas})
	if err != nil {
		return "", err
	}
	c.log.Info("msteams backfill complete", "instance_id", cfg.InstanceID, "chats", len(chats))
	return cur, nil
}

// listChats fetches every chat the user belongs to, expanding members, and
// following @odata.nextLink pages.
func (c *Connector) listChats(ctx context.Context, gc *graphClient) ([]graphChat, error) {
	var chats []graphChat
	next := "/me/chats?$expand=members"
	for next != "" {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var page listResponse[graphChat]
		if err := gc.getJSON(ctx, next, &page); err != nil {
			return nil, fmt.Errorf("msteams: list chats: %w", err)
		}
		chats = append(chats, page.Value...)
		next = page.NextLink
	}
	return chats, nil
}

// backfillChat pages a single chat's messages and emits a Document for each
// live message, skipping deletions (a backfill sees current state only).
func (c *Connector) backfillChat(ctx context.Context, gc *graphClient, tenant string, chat graphChat, emit sdk.Emit) error {
	next := "/chats/" + chat.ID + "/messages"
	for next != "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		var page listResponse[graphMessage]
		if err := gc.getJSON(ctx, next, &page); err != nil {
			return fmt.Errorf("msteams: list messages for chat %s: %w", chat.ID, err)
		}
		for _, msg := range page.Value {
			if msg.ID == "" || msg.removed() {
				continue
			}
			if err := emit(ctx, messageDocument(tenant, chat, msg)); err != nil {
				return err
			}
		}
		next = page.NextLink
	}
	return nil
}

// primeDelta drains a chat's /messages/delta feed (discarding the messages,
// which the backfill already emitted) to obtain the terminal @odata.deltaLink
// that IncrementalSync resumes from. A 410 Gone here is unexpected during a
// fresh backfill and surfaces as an error.
func (c *Connector) primeDelta(ctx context.Context, gc *graphClient, chatID string) (string, error) {
	next := "/chats/" + chatID + "/messages/delta"
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var page listResponse[graphMessage]
		if err := gc.getJSON(ctx, next, &page); err != nil {
			return "", fmt.Errorf("msteams: prime delta for chat %s: %w", chatID, err)
		}
		switch {
		case page.NextLink != "":
			next = page.NextLink
		case page.DeltaLink != "":
			return page.DeltaLink, nil
		default:
			// No more pages and no deltaLink: nothing to resume from.
			return "", nil
		}
	}
}

// IncrementalSync implements sdk.Connector. For each chat in the cursor it
// replays /messages/delta from the stored deltaLink, emitting an upsert for
// each changed message and a tombstone for each deletion (deletedDateTime or
// "@removed"), then advances that chat's deltaLink. A 410 Gone on any chat's
// delta link means the source can no longer replay it, so the whole pass
// returns sdk.ErrCursorExpired and the hub restarts FullSync.
func (c *Connector) IncrementalSync(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	dc, err := decodeCursor(cur)
	if err != nil {
		return "", err
	}
	gc := newGraphClient(conf, cfg.Token)
	tenant := string(cfg.Tenant.TenantID())

	// Stable iteration order makes emission deterministic for tests.
	chatIDs := make([]string, 0, len(dc.Deltas))
	for id := range dc.Deltas {
		chatIDs = append(chatIDs, id)
	}
	sort.Strings(chatIDs)

	next := make(map[string]string, len(dc.Deltas))
	for _, chatID := range chatIDs {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		link, err := c.replayChatDelta(ctx, gc, tenant, chatID, dc.Deltas[chatID], emit)
		if err != nil {
			return "", err
		}
		next[chatID] = link
	}
	return encodeCursor(deltaCursor{Deltas: next})
}

// replayChatDelta walks one chat's delta feed from startLink, emitting upserts
// and tombstones, and returns the new deltaLink. A 410 Gone is translated to
// sdk.ErrCursorExpired.
func (c *Connector) replayChatDelta(ctx context.Context, gc *graphClient, tenant, chatID, startLink string, emit sdk.Emit) (string, error) {
	chat := graphChat{ID: chatID}
	next := startLink
	if next == "" {
		next = "/chats/" + chatID + "/messages/delta"
	}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var page listResponse[graphMessage]
		err := gc.getJSON(ctx, next, &page)
		if errors.Is(err, errGone) {
			return "", fmt.Errorf("msteams: delta for chat %s returned 410 Gone: %w", chatID, sdk.ErrCursorExpired)
		}
		if err != nil {
			return "", fmt.Errorf("msteams: delta for chat %s: %w", chatID, err)
		}
		for _, msg := range page.Value {
			if msg.ID == "" {
				continue
			}
			if msg.removed() {
				if err := emit(ctx, c.tombstoneDocument(tenant, chatID, msg)); err != nil {
					return "", err
				}
				continue
			}
			if err := emit(ctx, messageDocument(tenant, chat, msg)); err != nil {
				return "", err
			}
		}
		switch {
		case page.NextLink != "":
			next = page.NextLink
		case page.DeltaLink != "":
			return page.DeltaLink, nil
		default:
			return startLink, nil
		}
	}
}
