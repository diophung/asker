package outlookmail

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/asker/asker/connectors/sdk"
)

// messageListPage is one page of GET /me/messages: the messages in "value" and
// the absolute next-page URL in "@odata.nextLink" (absent on the last page).
type messageListPage struct {
	Value    []graphMessage `json:"value"`
	NextLink string         `json:"@odata.nextLink"`
}

// deltaPage is one page of GET /me/messages/delta: changed/removed messages in
// "value", with "@odata.nextLink" to the next delta page and, on the final
// page, "@odata.deltaLink" — whose $deltatoken is the cursor a later
// incremental round resumes from.
type deltaPage struct {
	Value     []graphMessage `json:"value"`
	NextLink  string         `json:"@odata.nextLink"`
	DeltaLink string         `json:"@odata.deltaLink"`
}

// FullSync implements sdk.Connector: it pages GET /me/messages, emitting a
// Document per message and checkpointing each page so an interrupted backfill
// resumes instead of restarting. It then establishes the incremental cursor by
// asking Graph for a delta token at the current state
// (/me/messages/delta?$deltatoken=latest), and returns that token as the cursor
// IncrementalSync continues from.
func (c *Connector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	client := newClient(cfg.Token)
	tenant := string(cfg.Tenant.TenantID())

	c.log.Info("outlook-mail full sync starting", "instance_id", cfg.InstanceID)

	next := conf.BaseURL + "/me/messages?$select=" + url.QueryEscape(messageSelect) +
		"&$top=" + fmt.Sprint(pageSize)
	for next != "" {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var page messageListPage
		if err := c.getJSON(ctx, client, next, &page); err != nil {
			return "", fmt.Errorf("outlook-mail: list messages: %w", err)
		}
		for i := range page.Value {
			doc := messageDocument(tenant, &page.Value[i])
			if err := emit(ctx, doc); err != nil {
				return "", err
			}
		}
		next = rebaseLink(conf.BaseURL, page.NextLink)
		// Checkpoint the rebased next-page URL (empty on the last page). The
		// hub replays a non-empty checkpoint into IncrementalSync, which treats
		// a list-page URL as a backfill resume.
		if err := cfg.Checkpoint(ctx, sdk.Cursor(next)); err != nil {
			return "", err
		}
	}

	cur, err := c.initialDeltaCursor(ctx, client, conf)
	if err != nil {
		return "", err
	}
	if err := cfg.Checkpoint(ctx, cur); err != nil {
		return "", err
	}
	c.log.Info("outlook-mail backfill complete", "instance_id", cfg.InstanceID)
	return cur, nil
}

// initialDeltaCursor asks Graph for a delta token at the mailbox's current
// state. /me/messages/delta?$deltatoken=latest returns a single page carrying
// only the @odata.deltaLink (no items), whose $deltatoken becomes the
// steady-state cursor; every change after the backfill is then reported by the
// first incremental round and converges via idempotent upserts.
func (c *Connector) initialDeltaCursor(ctx context.Context, client *http.Client, conf instanceConfig) (sdk.Cursor, error) {
	u := conf.BaseURL + "/me/messages/delta?$select=" + url.QueryEscape(messageSelect) +
		"&$deltatoken=latest"
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var page deltaPage
		if err := c.getJSON(ctx, client, u, &page); err != nil {
			return "", fmt.Errorf("outlook-mail: initialize delta cursor: %w", err)
		}
		if page.DeltaLink != "" {
			return deltaCursor(page.DeltaLink)
		}
		if page.NextLink == "" {
			return "", fmt.Errorf("outlook-mail: delta initialization returned neither nextLink nor deltaLink")
		}
		u = rebaseLink(conf.BaseURL, page.NextLink)
	}
}

// IncrementalSync implements sdk.Connector. The cursor is the $deltatoken from
// the previous round (or a mid-backfill list-page URL the hub is replaying);
// the connector walks the delta query from it, emitting an upsert per changed
// message and a tombstone per "@removed" item, and returns the new delta token.
// An empty/unparseable cursor and a 410 Gone on a stale delta token both
// surface as sdk.ErrCursorExpired so the hub restarts a full sync.
func (c *Connector) IncrementalSync(ctx context.Context, cfg sdk.Config, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	client := newClient(cfg.Token)

	// A mid-backfill checkpoint (a /me/messages list-page URL) is replayed here
	// verbatim by the hub; resume the backfill from it.
	if isBackfillCursor(cur) {
		return c.resumeBackfill(ctx, cfg, conf, client, cur, emit)
	}

	start, err := resumeDeltaURL(conf, cur)
	if err != nil {
		return "", err
	}
	tenant := string(cfg.Tenant.TenantID())
	now := time.Now()

	u := start
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var page deltaPage
		if err := c.getJSON(ctx, client, u, &page); err != nil {
			if isGone(err) {
				// The source pruned this position; only a full re-sync can
				// recover (the hub clears the cursor and reruns FullSync).
				return "", fmt.Errorf("outlook-mail: delta token expired (410 Gone): %w", sdk.ErrCursorExpired)
			}
			return "", fmt.Errorf("outlook-mail: delta query: %w", err)
		}

		for i := range page.Value {
			m := &page.Value[i]
			if m.ID == "" {
				continue
			}
			if m.isRemoved() {
				if err := emit(ctx, tombstoneDocument(tenant, m, now)); err != nil {
					return "", err
				}
				continue
			}
			if err := emit(ctx, messageDocument(tenant, m)); err != nil {
				return "", err
			}
		}

		switch {
		case page.DeltaLink != "":
			return deltaCursor(page.DeltaLink)
		case page.NextLink != "":
			u = rebaseLink(conf.BaseURL, page.NextLink)
		default:
			// Graph always terminates a delta round with a deltaLink.
			return "", fmt.Errorf("outlook-mail: delta round ended without a deltaLink")
		}
	}
}

// resumeBackfill finishes an interrupted FullSync from a checkpointed list-page
// URL, then establishes the delta cursor exactly like FullSync's tail. The hub
// replays a mid-backfill checkpoint into IncrementalSync (sdk.Cursor contract),
// so the backfill converges without duplicate or missing pages.
func (c *Connector) resumeBackfill(ctx context.Context, cfg sdk.Config, conf instanceConfig, client *http.Client, cur sdk.Cursor, emit sdk.Emit) (sdk.Cursor, error) {
	tenant := string(cfg.Tenant.TenantID())
	next := rebaseLink(conf.BaseURL, string(cur))
	for next != "" {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var page messageListPage
		if err := c.getJSON(ctx, client, next, &page); err != nil {
			return "", fmt.Errorf("outlook-mail: resume list messages: %w", err)
		}
		for i := range page.Value {
			if err := emit(ctx, messageDocument(tenant, &page.Value[i])); err != nil {
				return "", err
			}
		}
		next = rebaseLink(conf.BaseURL, page.NextLink)
		if err := cfg.Checkpoint(ctx, sdk.Cursor(next)); err != nil {
			return "", err
		}
	}
	cur2, err := c.initialDeltaCursor(ctx, client, conf)
	if err != nil {
		return "", err
	}
	if err := cfg.Checkpoint(ctx, cur2); err != nil {
		return "", err
	}
	return cur2, nil
}

// deltaTokenPrefix marks a steady-state incremental cursor. The value after it
// is the Graph $deltatoken, stored portably (independent of base_url) so a
// later poll rebuilds the delta URL against whatever base_url is configured.
const deltaTokenPrefix = "delta:"

// deltaCursor extracts the $deltatoken from a Graph @odata.deltaLink and renders
// the portable steady-state cursor "delta:<token>".
func deltaCursor(deltaLink string) (sdk.Cursor, error) {
	u, err := url.Parse(deltaLink)
	if err != nil {
		return "", fmt.Errorf("outlook-mail: delta link is not a URL: %w", err)
	}
	token := u.Query().Get("$deltatoken")
	if token == "" {
		// Some Graph variants name it "deltatoken"; accept both.
		token = u.Query().Get("deltatoken")
	}
	if token == "" {
		return "", fmt.Errorf("outlook-mail: delta link %q has no $deltatoken", redactURL(deltaLink))
	}
	return sdk.Cursor(deltaTokenPrefix + token), nil
}

// isBackfillCursor reports whether cur is a replayed mid-backfill checkpoint (a
// /me/messages list-page URL) rather than a steady-state delta cursor.
func isBackfillCursor(cur sdk.Cursor) bool {
	s := strings.TrimSpace(string(cur))
	return strings.Contains(s, "/me/messages") && !strings.Contains(s, "/messages/delta")
}

// resumeDeltaURL turns a steady-state cursor into the absolute delta URL to walk
// from, rebuilt against conf.BaseURL so it is portable across environments. An
// empty or non-"delta:<token>" cursor — anything this connector did not produce
// — maps to sdk.ErrCursorExpired so the hub recovers with a full sync rather
// than looping on a poison cursor.
func resumeDeltaURL(conf instanceConfig, cur sdk.Cursor) (string, error) {
	s := strings.TrimSpace(string(cur))
	token, ok := strings.CutPrefix(s, deltaTokenPrefix)
	if !ok || token == "" {
		return "", fmt.Errorf("outlook-mail: cursor %q is not a delta cursor: %w", redactURL(s), sdk.ErrCursorExpired)
	}
	return conf.BaseURL + "/me/messages/delta?$select=" + url.QueryEscape(messageSelect) +
		"&$deltatoken=" + url.QueryEscape(token), nil
}

// rebaseLink rewrites an absolute Graph follow-link (@odata.nextLink /
// @odata.deltaLink) onto base so every request goes to the configured endpoint
// (the replay server in CI, the fake in dev, real Graph in prod), regardless of
// the host Graph embedded. An empty link returns "". A link that is already
// relative or unparseable is appended to base as a path+query.
func rebaseLink(base, link string) string {
	if link == "" {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil {
		return base + ensureLeadingSlash(link)
	}
	// Strip a leading API-version segment ("/v1.0", "/beta") so it is not
	// duplicated when base already carries one.
	path := stripAPIVersion(u.Path)
	rebased := strings.TrimRight(base, "/") + path
	if u.RawQuery != "" {
		rebased += "?" + u.RawQuery
	}
	return rebased
}

// stripAPIVersion drops a leading "/v1.0" or "/beta" segment from a Graph path.
func stripAPIVersion(p string) string {
	for _, v := range []string{"/v1.0", "/beta"} {
		if p == v {
			return "/"
		}
		if rest, ok := strings.CutPrefix(p, v+"/"); ok {
			return "/" + rest
		}
	}
	return ensureLeadingSlash(p)
}

// ensureLeadingSlash guarantees p begins with "/".
func ensureLeadingSlash(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		return "/" + p
	}
	return p
}
