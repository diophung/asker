package outlookcal

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/asker/asker/connectors/sdk"
)

// apiVersionSegments are the Graph API-version path prefixes a continuation
// link carries (e.g. "https://graph.microsoft.com/v1.0/me/events?..."). When
// re-hosting a link onto the configured base, this prefix is stripped from the
// link path because the configured base already carries its own version
// segment (real Graph: ".../v1.0"; the dev/CI cassette base: none).
var apiVersionSegments = []string{"/v1.0/", "/beta/"}

// resolveLink re-hosts a Graph continuation link (@odata.nextLink or
// @odata.deltaLink) onto the configured base URL, preserving the link's
// resource path (after its API-version prefix) and query. Graph returns
// absolute URLs pointing at graph.microsoft.com/v1.0/...; in dev/CI the
// base_url override points at a cassette ReplayServer on a different host with
// no version prefix, so the connector must follow the link against the
// configured base rather than the host the source baked into the link. A link
// that does not parse is returned unchanged (best effort).
func resolveLink(conf instanceConfig, link string) string {
	lu, err := url.Parse(link)
	if err != nil {
		return link
	}
	bu, err := url.Parse(conf.baseURL())
	if err != nil || bu.Host == "" {
		return link
	}
	resource := lu.Path
	for _, seg := range apiVersionSegments {
		if i := strings.Index(resource, seg); i >= 0 {
			resource = resource[i+len(seg)-1:] // keep the leading slash
			break
		}
	}
	resolved := *bu
	resolved.Path = strings.TrimRight(bu.Path, "/") + resource
	resolved.RawQuery = lu.RawQuery
	return resolved.String()
}

// listResponse is a Graph events / delta query page: a value array plus an
// @odata.nextLink (more pages of this query) or an @odata.deltaLink (end of a
// delta query; the token to resume from next time).
type listResponse struct {
	Value     []graphEvent `json:"value"`
	NextLink  string       `json:"@odata.nextLink"`
	DeltaLink string       `json:"@odata.deltaLink"`
}

// FullSync implements sdk.Connector. It first opens a fresh delta query against
// /me/calendarView/delta (capturing a delta token that covers the whole
// backfill window, draining it for the empty initial page until a deltaLink is
// returned), then pages the backfill via /me/events, emitting a Document per
// event and checkpointing "page:<nextLink>" after each completed page. It
// returns "delta:<deltaLink>" — the cursor IncrementalSync continues from.
func (c *Connector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	cl := newClient(conf, cfg.Token)

	deltaLink, err := c.primeDelta(ctx, cl, conf)
	if err != nil {
		return "", err
	}
	c.log.Info("outlook-cal full sync starting", "instance_id", cfg.InstanceID)

	nextURL := conf.baseURL() + "/me/events?" + backfillQuery()
	return c.backfill(ctx, cfg, cl, conf, emit, nextURL, deltaCursor(deltaLink))
}

// backfill pages events.list starting at startURL (a full URL: either the
// initial /me/events query or a resumed @odata.nextLink), emits a Document per
// event, and checkpoints "page:<nextLink>" after each completed page. It
// finishes by returning finalCursor (the delta cursor captured before the
// backfill). It is shared by FullSync and by IncrementalSync resuming a
// mid-backfill checkpoint.
func (c *Connector) backfill(ctx context.Context, cfg sdk.Config, cl *client, conf instanceConfig, emit sdk.Emit, startURL string, finalCursor sdk.Cursor) (sdk.Cursor, error) {
	tenant := string(cfg.Tenant.TenantID())
	nextURL := startURL
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var page listResponse
		if err := cl.getJSON(ctx, resolveLink(conf, nextURL), &page); err != nil {
			return "", fmt.Errorf("outlook-cal: list events: %w", err)
		}
		for i := range page.Value {
			ev := page.Value[i]
			if ev.ID == "" {
				continue
			}
			if err := emit(ctx, eventDocument(tenant, ev)); err != nil {
				return "", err
			}
		}
		if page.NextLink == "" {
			break
		}
		nextURL = page.NextLink
		if err := cfg.Checkpoint(ctx, pageCursor(nextURL)); err != nil {
			return "", err
		}
	}

	if err := cfg.Checkpoint(ctx, finalCursor); err != nil {
		return "", err
	}
	c.log.Info("outlook-cal backfill complete", "instance_id", cfg.InstanceID, "cursor", string(finalCursor))
	return finalCursor, nil
}

// primeDelta opens a fresh delta query and drains its pages (which Graph
// returns empty for an initial delta) until it yields the @odata.deltaLink that
// will catch every change made after this point. The returned deltaLink becomes
// the steady-state cursor.
func (c *Connector) primeDelta(ctx context.Context, cl *client, conf instanceConfig) (string, error) {
	nextURL := conf.baseURL() + "/me/calendarView/delta?" + deltaQuery()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var page listResponse
		if err := cl.getJSON(ctx, resolveLink(conf, nextURL), &page); err != nil {
			return "", fmt.Errorf("outlook-cal: prime delta query: %w", err)
		}
		if page.DeltaLink != "" {
			return page.DeltaLink, nil
		}
		if page.NextLink == "" {
			return "", fmt.Errorf("outlook-cal: delta query returned neither nextLink nor deltaLink")
		}
		nextURL = page.NextLink
	}
}

// IncrementalSync implements sdk.Connector. A steady-state cursor
// ("delta:<deltaLink>") replays the delta query from that link, emitting an
// upsert per changed event and a tombstone per "@removed" event, and advancing
// to the new deltaLink. A mid-backfill checkpoint cursor ("page:<nextLink>")
// resumes the interrupted backfill. An unparseable cursor, or a 410/resync from
// the delta query, surfaces as sdk.ErrCursorExpired so the hub restarts a full
// sync.
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
		c.log.Info("resuming interrupted outlook-cal backfill", "instance_id", cfg.InstanceID)
		// Resume the backfill; the final delta cursor was lost on interruption,
		// so re-prime a fresh delta token after draining the remaining pages.
		deltaLink, err := c.primeDelta(ctx, cl, conf)
		if err != nil {
			return "", err
		}
		return c.backfill(ctx, cfg, cl, conf, emit, st.link, deltaCursor(deltaLink))
	}
	return c.replayDelta(ctx, cfg, cl, conf, emit, st.link)
}

// replayDelta walks the delta query from deltaLink to its terminal
// @odata.deltaLink, emitting an upsert per changed event and a tombstone per
// "@removed" event, and returns the advanced delta cursor. A 410/resync from
// Graph becomes sdk.ErrCursorExpired.
func (c *Connector) replayDelta(ctx context.Context, cfg sdk.Config, cl *client, conf instanceConfig, emit sdk.Emit, deltaLink string) (sdk.Cursor, error) {
	tenant := string(cfg.Tenant.TenantID())
	nextURL := deltaLink
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var page listResponse
		if err := cl.getJSON(ctx, resolveLink(conf, nextURL), &page); err != nil {
			return "", asCursorExpired(fmt.Errorf("outlook-cal: delta query: %w", err))
		}
		for i := range page.Value {
			ev := page.Value[i]
			if ev.ID == "" {
				continue
			}
			if ev.isRemoved() {
				if err := emit(ctx, c.tombstoneDocument(tenant, ev)); err != nil {
					return "", err
				}
				continue
			}
			if err := emit(ctx, eventDocument(tenant, ev)); err != nil {
				return "", err
			}
		}
		if page.DeltaLink != "" {
			return deltaCursor(page.DeltaLink), nil
		}
		if page.NextLink == "" {
			// A well-formed delta walk always terminates in a deltaLink; a
			// page with neither link is a malformed source response.
			return "", fmt.Errorf("outlook-cal: delta page returned neither nextLink nor deltaLink")
		}
		nextURL = page.NextLink
	}
}

// backfillQuery builds the query string for the initial /me/events page:
// $select to limit fields and $top to set the page size.
func backfillQuery() string {
	q := url.Values{}
	q.Set("$select", eventSelect)
	q.Set("$top", strconv.Itoa(pageSize))
	return q.Encode()
}

// deltaQuery builds the query string for the initial /me/calendarView/delta
// page. $select keeps the payload small; $top sets the page size. (A real
// deployment may add startDateTime/endDateTime window params; the connector
// keeps the default rolling window Graph applies to calendarView/delta.)
func deltaQuery() string {
	q := url.Values{}
	q.Set("$select", eventSelect)
	q.Set("$top", strconv.Itoa(pageSize))
	return q.Encode()
}
