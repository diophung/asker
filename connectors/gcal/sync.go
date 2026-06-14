package gcal

import (
	"context"
	"errors"
	"fmt"

	"github.com/asker/asker/connectors/sdk"
)

// FullSync implements sdk.Connector: it pages through events.list
// (singleEvents=true), emitting a Document per event and checkpointing
// "page:<nextPageToken>" after every completed page (resumable). The final page
// carries nextSyncToken; FullSync returns "sync:<syncToken>", the cursor
// IncrementalSync continues from.
func (c *Connector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	c.log.Info("gcal full sync starting",
		"instance_id", cfg.InstanceID, "calendar_id", conf.CalendarID)
	return c.backfill(ctx, cfg, newClient(conf, cfg.Token), conf.CalendarID, emit, "")
}

// backfill pages events.list starting at pageToken (empty = first page), emits
// a Document per event, and checkpoints after every completed page. It is
// shared by FullSync (pageToken "") and by IncrementalSync when the hub replays
// a mid-backfill checkpoint cursor. It returns "sync:<nextSyncToken>".
func (c *Connector) backfill(ctx context.Context, cfg sdk.Config, cl *client, calendarID string, emit sdk.Emit, pageToken string) (sdk.Cursor, error) {
	tenant := string(cfg.Tenant.TenantID())
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		page, err := cl.listEvents(ctx, listParams{pageToken: pageToken, maxResults: listPageSize})
		if err != nil {
			return "", err
		}
		for _, ev := range page.Items {
			if ev == nil || ev.ID == "" {
				continue
			}
			if err := emit(ctx, c.eventDocument(tenant, calendarID, ev)); err != nil {
				return "", err
			}
		}

		if page.NextPageToken != "" {
			pageToken = page.NextPageToken
			if err := cfg.Checkpoint(ctx, pageCursor(pageToken)); err != nil {
				return "", err
			}
			continue
		}

		// Final page: the sync token anchors incremental sync.
		if page.NextSyncToken == "" {
			return "", errors.New("gcal: final events.list page carried no nextSyncToken")
		}
		cur := syncCursor(page.NextSyncToken)
		if err := cfg.Checkpoint(ctx, cur); err != nil {
			return "", err
		}
		c.log.Info("gcal backfill complete", "instance_id", cfg.InstanceID)
		return cur, nil
	}
}

// IncrementalSync implements sdk.Connector. A steady-state cursor
// ("sync:<token>") replays events.list?syncToken=<token>, emitting changed
// events as upserts and canceled events as tombstones, then paging through any
// follow-on pages to the new nextSyncToken. A mid-backfill checkpoint cursor
// ("page:<token>") resumes the interrupted backfill at <token> (the hub replays
// checkpoints verbatim into IncrementalSync, per the sdk.Cursor contract). An
// unparseable cursor and a 410 Gone from the source both surface as
// sdk.ErrCursorExpired so the hub restarts a full sync.
func (c *Connector) IncrementalSync(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	st, err := parseCursor(cur)
	if err != nil {
		return "", fmt.Errorf("%v: %w", err, sdk.ErrCursorExpired)
	}
	cl := newClient(conf, cfg.Token)
	if st.backfill {
		c.log.Info("resuming interrupted gcal backfill",
			"instance_id", cfg.InstanceID, "page_token", st.pageToken)
		return c.backfill(ctx, cfg, cl, conf.CalendarID, emit, st.pageToken)
	}
	return c.replaySync(ctx, cfg, cl, conf.CalendarID, emit, st.syncToken)
}

// replaySync walks the sync-token result pages: every item is emitted (a
// canceled event as a tombstone, any other as an upsert). Calendar returns the
// new nextSyncToken on the final page; intermediate pages carry nextPageToken
// and the SAME syncToken is not resent (pageToken takes over). The advanced
// "sync:<token>" cursor is returned.
func (c *Connector) replaySync(ctx context.Context, cfg sdk.Config, cl *client, calendarID string, emit sdk.Emit, syncToken string) (sdk.Cursor, error) {
	tenant := string(cfg.Tenant.TenantID())
	pageToken := ""
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var params listParams
		if pageToken != "" {
			params = listParams{pageToken: pageToken, maxResults: listPageSize}
		} else {
			params = listParams{syncToken: syncToken, maxResults: listPageSize}
		}
		page, err := cl.listEvents(ctx, params)
		if err != nil {
			if isCursorExpired(err) {
				return "", fmt.Errorf("gcal: incremental sync from syncToken failed: %w", err)
			}
			return "", err
		}
		for _, ev := range page.Items {
			if ev == nil || ev.ID == "" {
				continue
			}
			if err := emit(ctx, c.eventDocument(tenant, calendarID, ev)); err != nil {
				return "", err
			}
		}
		if page.NextPageToken != "" {
			pageToken = page.NextPageToken
			continue
		}
		if page.NextSyncToken == "" {
			return "", errors.New("gcal: incremental events.list page carried no nextSyncToken")
		}
		return syncCursor(page.NextSyncToken), nil
	}
}
