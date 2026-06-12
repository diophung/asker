package gmail

import (
	"context"
	"fmt"
	"sort"

	gmailapi "google.golang.org/api/gmail/v1"

	"github.com/asker/asker/connectors/sdk"
)

// FullSync implements sdk.Connector: it captures the mailbox's current
// history id FIRST (so every change racing the backfill is replayed by the
// first incremental pass), then pages through users.messages.list, fetching
// and emitting every message, checkpointing "history:<id>|page:<token>"
// after every completed page. It finishes by registering the push watch
// (when the hub merged a webhook_url into the config) and returns
// "history:<id>".
func (c *Connector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	svc, err := newService(ctx, conf, cfg.Token)
	if err != nil {
		return "", fmt.Errorf("gmail: build API client: %w", err)
	}
	historyID, err := c.currentHistoryID(ctx, svc)
	if err != nil {
		return "", err
	}
	c.log.Info("gmail full sync starting",
		"instance_id", cfg.InstanceID, "history_id", historyID)
	return c.backfill(ctx, cfg, conf, svc, emit, historyID, "")
}

// backfill pages through messages.list starting at pageToken (empty = first
// page), emits a Document per message, and checkpoints after every completed
// page. It is shared by FullSync (pageToken "") and by IncrementalSync when
// the hub replays a mid-backfill checkpoint cursor.
func (c *Connector) backfill(ctx context.Context, cfg sdk.Config, conf instanceConfig, svc *gmailapi.Service, emit sdk.Emit, historyID uint64, pageToken string) (sdk.Cursor, error) {
	tenant := string(cfg.Tenant.TenantID())
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		call := svc.Users.Messages.List(gmailUser).MaxResults(listPageSize).Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		page, err := call.Do()
		if err != nil {
			return "", fmt.Errorf("gmail: messages.list: %w", err)
		}

		for _, ref := range page.Messages {
			msg, err := svc.Users.Messages.Get(gmailUser, ref.Id).Format("full").Context(ctx).Do()
			if isNotFound(err) {
				// Deleted between list and get. The deletion is a history
				// event newer than the backfill-start history id, so the
				// first incremental pass reconciles it; skipping is safe.
				c.log.Info("message vanished during backfill, skipping", "message_id", ref.Id)
				continue
			}
			if err != nil {
				return "", fmt.Errorf("gmail: messages.get %s: %w", ref.Id, err)
			}
			doc, err := messageDocument(tenant, msg)
			if err != nil {
				return "", err
			}
			if err := emit(ctx, doc); err != nil {
				return "", err
			}
		}

		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
		if err := cfg.Checkpoint(ctx, backfillCursor(historyID, pageToken)); err != nil {
			return "", err
		}
	}

	cur := incrementalCursor(historyID)
	if err := cfg.Checkpoint(ctx, cur); err != nil {
		return "", err
	}
	c.registerWatch(ctx, svc, conf)
	c.log.Info("gmail backfill complete", "instance_id", cfg.InstanceID, "cursor", string(cur))
	return cur, nil
}

// registerWatch registers users.watch with the fake-gmail push shim header
// (ADR-008) when the hub supplied a webhook_url. Failure is deliberately
// non-fatal: the hub's polling fallback still meets the freshness SLA,
// whereas failing the sync here would force a needless re-backfill.
func (c *Connector) registerWatch(ctx context.Context, svc *gmailapi.Service, conf instanceConfig) {
	if conf.WebhookURL == "" {
		return
	}
	call := svc.Users.Watch(gmailUser, &gmailapi.WatchRequest{TopicName: watchTopicName}).Context(ctx)
	call.Header().Set(pushURLHeader, conf.WebhookURL)
	resp, err := call.Do()
	if err != nil {
		c.log.Warn("gmail watch registration failed; relying on polling fallback",
			"webhook_url", conf.WebhookURL, "error", err)
		return
	}
	c.log.Info("gmail watch registered",
		"webhook_url", conf.WebhookURL, "history_id", resp.HistoryId, "expiration_ms", resp.Expiration)
}

// IncrementalSync implements sdk.Connector. A steady-state cursor
// ("history:<id>") replays users.history.list from <id>; a mid-backfill
// checkpoint cursor ("history:<id>|page:<token>") resumes the interrupted
// backfill at <token> (the hub replays checkpoints verbatim into
// IncrementalSync, per the sdk.Cursor contract). An unparseable cursor and a
// 404 from history.list both surface as sdk.ErrCursorExpired so the hub
// restarts a full sync.
func (c *Connector) IncrementalSync(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	st, err := parseCursor(cur)
	if err != nil {
		return "", fmt.Errorf("%v: %w", err, sdk.ErrCursorExpired)
	}
	svc, err := newService(ctx, conf, cfg.Token)
	if err != nil {
		return "", fmt.Errorf("gmail: build API client: %w", err)
	}
	if st.backfill {
		c.log.Info("resuming interrupted gmail backfill",
			"instance_id", cfg.InstanceID, "history_id", st.historyID, "page_token", st.pageToken)
		return c.backfill(ctx, cfg, conf, svc, emit, st.historyID, st.pageToken)
	}
	return c.replayHistory(ctx, cfg, svc, emit, st.historyID)
}

// changeKind is the final observed state of one message in a history replay.
type changeKind int

const (
	changeAdded changeKind = iota
	changeDeleted
)

// lastChange tracks the LAST history event seen for one message id. An edit
// at the source appears as messageDeleted followed by messageAdded for the
// SAME id, so set-differencing a history response would wrongly tombstone
// edited messages; last-event-wins replay re-fetches them instead.
type lastChange struct {
	msgID     string
	kind      changeKind
	historyID uint64 // history record that produced this final state
	seq       int    // global replay sequence, for stable emission order
}

// replayHistory lists history records since startID, folds them STRICTLY in
// history-id order into a last-event-per-message map, then emits an upsert
// (messages.get) or a tombstone per affected message and returns the
// advanced cursor.
func (c *Connector) replayHistory(ctx context.Context, cfg sdk.Config, svc *gmailapi.Service, emit sdk.Emit, startID uint64) (sdk.Cursor, error) {
	tenant := string(cfg.Tenant.TenantID())

	var records []*gmailapi.History
	currentID := startID
	pageToken := ""
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		call := svc.Users.History.List(gmailUser).
			StartHistoryId(startID).
			HistoryTypes("messageAdded", "messageDeleted").
			MaxResults(listPageSize).
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			if isNotFound(err) {
				// The source pruned this position; only a full re-sync can
				// recover (the hub clears the cursor and reruns FullSync).
				return "", fmt.Errorf("gmail: history.list startHistoryId=%d returned 404 (%v): %w",
					startID, err, sdk.ErrCursorExpired)
			}
			return "", fmt.Errorf("gmail: history.list: %w", err)
		}
		records = append(records, resp.History...)
		if resp.HistoryId > currentID {
			currentID = resp.HistoryId
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	changes := foldHistory(records)
	for _, ch := range changes {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		switch ch.kind {
		case changeDeleted:
			if err := emit(ctx, c.tombstoneDocument(tenant, ch.msgID, ch.historyID)); err != nil {
				return "", err
			}
		case changeAdded:
			msg, err := svc.Users.Messages.Get(gmailUser, ch.msgID).Format("full").Context(ctx).Do()
			if isNotFound(err) {
				// Deleted again since the history fetch; the source truth is
				// "gone", so tombstone it now rather than wait a poll cycle.
				if err := emit(ctx, c.tombstoneDocument(tenant, ch.msgID, currentID)); err != nil {
					return "", err
				}
				continue
			}
			if err != nil {
				return "", fmt.Errorf("gmail: messages.get %s: %w", ch.msgID, err)
			}
			doc, err := messageDocument(tenant, msg)
			if err != nil {
				return "", err
			}
			if err := emit(ctx, doc); err != nil {
				return "", err
			}
		}
	}

	return incrementalCursor(currentID), nil
}

// foldHistory sorts records by history id and reduces them to one final
// change per message id, returned in the order each message's final event
// occurred. Within a single record deletions are folded before additions so
// a record carrying both for one id resolves to "added"; the
// fetch-or-tombstone path then converges on the source's actual state.
func foldHistory(records []*gmailapi.History) []*lastChange {
	sort.SliceStable(records, func(i, j int) bool { return records[i].Id < records[j].Id })

	last := make(map[string]*lastChange)
	var changes []*lastChange
	seq := 0
	note := func(msgID string, kind changeKind, historyID uint64) {
		if msgID == "" {
			return
		}
		seq++
		ch, ok := last[msgID]
		if !ok {
			ch = &lastChange{msgID: msgID}
			last[msgID] = ch
			changes = append(changes, ch)
		}
		ch.kind = kind
		ch.historyID = historyID
		ch.seq = seq
	}
	for _, rec := range records {
		for _, d := range rec.MessagesDeleted {
			if d.Message != nil {
				note(d.Message.Id, changeDeleted, rec.Id)
			}
		}
		for _, a := range rec.MessagesAdded {
			if a.Message != nil {
				note(a.Message.Id, changeAdded, rec.Id)
			}
		}
	}

	sort.Slice(changes, func(i, j int) bool { return changes[i].seq < changes[j].seq })
	return changes
}
