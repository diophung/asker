package slack

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/asker/asker/connectors/sdk"
)

// FullSync implements sdk.Connector: it lists every conversation the token can
// see (conversations.list, paginated), then pages through each channel's
// history (conversations.history, paginated), emitting a Document per message.
// It checkpoints the accumulating per-channel cursor after every completed
// channel so an interrupted backfill resumes, and returns the per-channel
// high-water-mark cursor IncrementalSync continues from.
func (c *Connector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	cl := newClient(conf, cfg.Token)
	tenant := string(cfg.Tenant.TenantID())

	channels, err := c.listChannels(ctx, cl)
	if err != nil {
		return "", err
	}
	c.log.Info("slack full sync starting",
		"instance_id", cfg.InstanceID, "channels", len(channels))

	state := cursorState{}
	for _, ch := range channels {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := c.backfillChannel(ctx, cl, tenant, ch, state, emit); err != nil {
			return "", err
		}
		if err := cfg.Checkpoint(ctx, state.encode()); err != nil {
			return "", err
		}
	}

	cur := state.encode()
	c.log.Info("slack backfill complete", "instance_id", cfg.InstanceID, "channels", len(channels))
	return cur, nil
}

// listChannels pages conversations.list (public + private channels and group
// DMs the token can see) into a slice, following response_metadata.next_cursor.
func (c *Connector) listChannels(ctx context.Context, cl *client) ([]channelInfo, error) {
	var out []channelInfo
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		params := url.Values{}
		params.Set("limit", strconv.Itoa(historyLimit))
		params.Set("types", "public_channel,private_channel,mpim,im")
		params.Set("exclude_archived", "true")
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var resp struct {
			apiResponse
			Channels []channelInfo `json:"channels"`
		}
		if err := cl.get(ctx, "conversations.list", params, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Channels...)
		cursor = resp.ResponseMetadata.NextCursor
		if cursor == "" {
			break
		}
	}
	return out, nil
}

// channelInfoByID fetches a single channel's metadata (conversations.info) so a
// webhook-delivered message — which names its channel only by id — can be mapped
// with the same channel_name and ACL the backfill (conversations.list) captured.
func (c *Connector) channelInfoByID(ctx context.Context, cl *client, channelID string) (channelInfo, error) {
	params := url.Values{}
	params.Set("channel", channelID)
	var resp struct {
		apiResponse
		Channel channelInfo `json:"channel"`
	}
	if err := cl.get(ctx, "conversations.info", params, &resp); err != nil {
		return channelInfo{}, err
	}
	return resp.Channel, nil
}

// backfillChannel pages a channel's full history oldest-to-newest, emitting a
// Document per message and advancing state to the channel's newest ts.
func (c *Connector) backfillChannel(ctx context.Context, cl *client, tenant string, ch channelInfo, state cursorState, emit sdk.Emit) error {
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		params := url.Values{}
		params.Set("channel", ch.ID)
		params.Set("limit", strconv.Itoa(historyLimit))
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var resp struct {
			apiResponse
			Messages []message `json:"messages"`
			HasMore  bool      `json:"has_more"`
		}
		if err := cl.get(ctx, "conversations.history", params, &resp); err != nil {
			return err
		}
		for i := range resp.Messages {
			m := resp.Messages[i]
			if !isUserMessage(m) {
				continue
			}
			if err := emit(ctx, messageDocument(tenant, ch, m)); err != nil {
				return err
			}
			state.advance(ch.ID, m.TS)
		}
		cursor = resp.ResponseMetadata.NextCursor
		if cursor == "" {
			break
		}
	}
	return nil
}

// IncrementalSync implements sdk.Connector. It reads each channel's history
// since the per-channel high-water mark in cur (conversations.history with
// oldest=<ts>), emitting created/edited messages as upserts and deletions as
// tombstones, and advances the cursor. New channels created since the last
// sync are picked up from conversations.list (no watermark => full channel
// history). A cursor the connector did not produce, or a Slack
// "invalid_cursor", surfaces as sdk.ErrCursorExpired so the hub restarts a
// full sync.
func (c *Connector) IncrementalSync(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	state, err := parseCursor(cur)
	if err != nil {
		return "", fmt.Errorf("%v: %w", err, sdk.ErrCursorExpired)
	}
	cl := newClient(conf, cfg.Token)
	tenant := string(cfg.Tenant.TenantID())

	channels, err := c.listChannels(ctx, cl)
	if err != nil {
		return "", err
	}

	for _, ch := range channels {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		oldest := state[ch.ID]
		if err := c.syncChannelSince(ctx, cl, tenant, ch, oldest, state, emit); err != nil {
			return "", err
		}
	}
	return state.encode(), nil
}

// syncChannelSince pages a channel's history newer than oldest (exclusive),
// emitting an upsert per message and advancing state. A "channel_not_found"
// error (the channel was deleted/left since listing) is skipped; an
// "invalid_cursor" is mapped to sdk.ErrCursorExpired.
func (c *Connector) syncChannelSince(ctx context.Context, cl *client, tenant string, ch channelInfo, oldest string, state cursorState, emit sdk.Emit) error {
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		params := url.Values{}
		params.Set("channel", ch.ID)
		params.Set("limit", strconv.Itoa(historyLimit))
		if oldest != "" {
			// oldest is inclusive on the Slack API; inclusive=false excludes
			// the boundary message we already have.
			params.Set("oldest", oldest)
			params.Set("inclusive", "false")
		}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var resp struct {
			apiResponse
			Messages []message `json:"messages"`
			HasMore  bool      `json:"has_more"`
		}
		if err := cl.get(ctx, "conversations.history", params, &resp); err != nil {
			switch {
			case isAPIError(err, "channel_not_found"):
				c.log.Info("channel vanished during incremental sync, skipping", "channel", ch.ID)
				return nil
			case isAPIError(err, "invalid_cursor"):
				return fmt.Errorf("slack: conversations.history channel=%s: %w", ch.ID, sdk.ErrCursorExpired)
			default:
				return err
			}
		}
		for i := range resp.Messages {
			m := resp.Messages[i]
			if !isUserMessage(m) {
				continue
			}
			if err := emit(ctx, messageDocument(tenant, ch, m)); err != nil {
				return err
			}
			state.advance(ch.ID, m.TS)
		}
		cursor = resp.ResponseMetadata.NextCursor
		if cursor == "" {
			break
		}
	}
	return nil
}

// isUserMessage reports whether m is a real channel message the connector
// should index. It filters out join/leave/topic-change and other system
// subtypes (which carry no user-authored content) while keeping plain messages
// and edited messages. Tombstones come from the webhook delete path, not from
// history (deleted messages disappear from history).
func isUserMessage(m message) bool {
	if m.Type != "" && m.Type != "message" {
		return false
	}
	if m.TS == "" {
		return false
	}
	switch m.Subtype {
	case "", "bot_message", "thread_broadcast", "me_message", "file_share":
		return true
	default:
		// channel_join, channel_leave, channel_topic, message_changed, etc.
		// "message_changed" never appears in history (history shows the
		// post-edit message directly), only in the Events API stream.
		return false
	}
}
