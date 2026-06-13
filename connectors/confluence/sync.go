package confluence

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/asker/asker/connectors/sdk"
)

// FullSync implements sdk.Connector: it pages through
// GET /content?type=page (offset pagination, expanding body/version/space/
// history), emits a Document per page, and checkpoints the page offset after
// every completed page so an interrupted backfill resumes instead of
// restarting. It returns the cursor IncrementalSync continues from: the newest
// lastModified seen across the backfill (or "now" when the space is empty).
func (c *Connector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	client := newClient(conf, cfg.Token)
	tenant := string(cfg.Tenant.TenantID())

	c.log.Info("confluence full sync starting",
		"instance_id", cfg.InstanceID, "space_key", conf.SpaceKey)

	start := 0
	var newest time.Time
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		page, err := c.listPage(ctx, client, conf, start)
		if err != nil {
			return "", err
		}
		for i := range page.Results {
			item := &page.Results[i]
			doc, derr := contentDocument(tenant, item, page.Links.Base)
			if derr != nil {
				return "", derr
			}
			if err := emit(ctx, doc); err != nil {
				return "", err
			}
			if mt, ok := parseTime(item.Version.When); ok && mt.After(newest) {
				newest = mt
			}
		}

		if !hasNextPage(page) {
			break
		}
		start += len(page.Results)
		if err := cfg.Checkpoint(ctx, offsetCursor(start)); err != nil {
			return "", err
		}
	}

	cur := c.finalCursor(newest)
	if err := cfg.Checkpoint(ctx, cur); err != nil {
		return "", err
	}
	c.log.Info("confluence backfill complete", "instance_id", cfg.InstanceID, "cursor", string(cur))
	return cur, nil
}

// listPage fetches one offset page of pages from /content.
func (c *Connector) listPage(ctx context.Context, client *apiClient, conf instanceConfig, start int) (*contentPage, error) {
	q := url.Values{}
	q.Set("type", "page")
	q.Set("status", "current")
	q.Set("expand", expandParam)
	q.Set("limit", strconv.Itoa(listPageSize))
	q.Set("start", strconv.Itoa(start))
	if conf.SpaceKey != "" {
		q.Set("spaceKey", conf.SpaceKey)
	}
	var page contentPage
	if err := client.getJSON(ctx, "/content", q, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// finalCursor renders the cursor FullSync returns: the newest lastModified seen,
// or the current time when the backfill emitted nothing (so the first
// incremental pass scans only future changes).
func (c *Connector) finalCursor(newest time.Time) sdk.Cursor {
	if newest.IsZero() {
		return renderCursor(c.now())
	}
	return renderCursor(newest)
}

// IncrementalSync implements sdk.Connector. It runs two CQL searches from the
// cursor's lastModified watermark:
//
//   - current pages modified at/after the cursor (created+changed upserts),
//   - trashed pages modified at/after the cursor (deletions -> tombstones).
//
// It advances the cursor to the newest lastModified observed across both. The
// cursor is a CQL timestamp comparison and is therefore always replayable at
// the source, so this connector NEVER returns sdk.ErrCursorExpired (documented
// in the package comment). An unparseable cursor degrades to a full window
// (zero time) rather than an error, for the same reason.
func (c *Connector) IncrementalSync(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	since, ok := parseCursor(cur)
	if !ok {
		c.log.Warn("confluence incremental cursor unparseable; scanning full window",
			"instance_id", cfg.InstanceID, "cursor", string(cur))
		since = time.Time{}
	}
	client := newClient(conf, cfg.Token)
	tenant := string(cfg.Tenant.TenantID())

	newest := since

	// 1. Current pages created or changed since the cursor.
	changedNewest, err := c.syncChanged(ctx, client, conf, tenant, since, emit)
	if err != nil {
		return "", err
	}
	if changedNewest.After(newest) {
		newest = changedNewest
	}

	// 2. Trashed pages in the same window -> tombstones (the M2 deletion model).
	trashedNewest, err := c.syncTrashed(ctx, client, conf, tenant, since, emit)
	if err != nil {
		return "", err
	}
	if trashedNewest.After(newest) {
		newest = trashedNewest
	}

	if newest.Equal(since) {
		// No changes: return the cursor unchanged so the hub records "no move".
		return renderCursor(since), nil
	}
	return renderCursor(newest), nil
}

// syncChanged emits an upsert Document for every current page with
// lastModified >= since, paging via the search API's offset/_links.next, and
// returns the newest lastModified it observed.
func (c *Connector) syncChanged(ctx context.Context, client *apiClient, conf instanceConfig, tenant string, since time.Time, emit sdk.Emit) (time.Time, error) {
	newest := since
	start := 0
	for {
		if err := ctx.Err(); err != nil {
			return newest, err
		}
		page, err := c.searchPage(ctx, client, conf, "current", since, start)
		if err != nil {
			return newest, err
		}
		for i := range page.Results {
			item := &page.Results[i]
			doc, derr := contentDocument(tenant, item, page.Links.Base)
			if derr != nil {
				return newest, derr
			}
			if err := emit(ctx, doc); err != nil {
				return newest, err
			}
			if mt, ok := parseTime(item.Version.When); ok && mt.After(newest) {
				newest = mt
			}
		}
		if !hasNextPage(page) {
			return newest, nil
		}
		start += len(page.Results)
	}
}

// syncTrashed emits a tombstone for every trashed page with lastModified >=
// since (the M2 deletion model: a page is detected as deleted by appearing in
// the trashed window) and returns the newest lastModified it observed.
func (c *Connector) syncTrashed(ctx context.Context, client *apiClient, conf instanceConfig, tenant string, since time.Time, emit sdk.Emit) (time.Time, error) {
	newest := since
	start := 0
	for {
		if err := ctx.Err(); err != nil {
			return newest, err
		}
		page, err := c.searchPage(ctx, client, conf, "trashed", since, start)
		if err != nil {
			return newest, err
		}
		for i := range page.Results {
			item := &page.Results[i]
			tomb := c.tombstoneDocument(tenant, item.ID, versionEtag(item))
			if err := emit(ctx, tomb); err != nil {
				return newest, err
			}
			if mt, ok := parseTime(item.Version.When); ok && mt.After(newest) {
				newest = mt
			}
		}
		if !hasNextPage(page) {
			return newest, nil
		}
		start += len(page.Results)
	}
}

// searchPage runs one offset page of the CQL search for pages of the given
// status (current|trashed) modified at/after since. The cursor's >= comparison
// plus asc ordering makes a re-run from the same minute safe.
func (c *Connector) searchPage(ctx context.Context, client *apiClient, conf instanceConfig, status string, since time.Time, start int) (*contentPage, error) {
	cql := buildCQL(conf.SpaceKey, status, since)
	q := url.Values{}
	q.Set("cql", cql)
	q.Set("expand", expandParam)
	q.Set("limit", strconv.Itoa(listPageSize))
	q.Set("start", strconv.Itoa(start))

	var page contentPage
	if err := client.getJSON(ctx, "/content/search", q, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// buildCQL assembles the CQL filter: type=page, the given status, a
// lastModified watermark, an optional space scope, ordered by lastModified asc
// so pagination is stable and the cursor advances monotonically.
func buildCQL(spaceKey, status string, since time.Time) string {
	clauses := []string{"type=page", "status=" + status}
	if spaceKey != "" {
		clauses = append(clauses, fmt.Sprintf("space=%q", spaceKey))
	}
	if !since.IsZero() {
		clauses = append(clauses, fmt.Sprintf("lastModified>=%q", since.UTC().Format(cqlTimeLayout)))
	}
	cql := clauses[0]
	for _, cl := range clauses[1:] {
		cql += " and " + cl
	}
	return cql + " order by lastModified asc"
}

// hasNextPage reports whether the offset page has a following page. Confluence
// signals "more" via a non-empty _links.next; as a fallback (some search
// responses omit it) a full page (size == limit) is treated as "maybe more".
func hasNextPage(page *contentPage) bool {
	if page.Links.Next != "" {
		return true
	}
	limit := page.Limit
	if limit == 0 {
		limit = listPageSize
	}
	return len(page.Results) >= limit && len(page.Results) > 0
}

// offsetCursor renders a mid-backfill checkpoint cursor. FullSync checkpoints
// the next page offset; the hub persists it verbatim. A resumed FullSync starts
// fresh (offset cursors are not replayed into IncrementalSync — the hub reruns
// FullSync), so this value exists for observability and crash-resume bookkeeping
// rather than to be parsed back.
func offsetCursor(start int) sdk.Cursor {
	return sdk.Cursor("offset:" + strconv.Itoa(start))
}
