package jira

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/asker/asker/connectors/sdk"
)

// FullSync implements sdk.Connector: it pages through every issue the token
// can read in updated-ascending order, emits a TICKET Document per issue,
// checkpoints the per-issue cursor after every completed page, and returns the
// cursor IncrementalSync continues from.
func (c *Connector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	cl := c.newClient(conf, cfg.Token)
	c.log.Info("jira full sync starting", "instance_id", cfg.InstanceID)
	// FullSync starts from the empty cursor (whole site) and never expires:
	// the empty bound is always valid, so a search error is a real failure.
	return c.scan(ctx, cfg, conf, cl, emit, cursorState{}, false)
}

// IncrementalSync implements sdk.Connector: it resumes the "updated >=" scan
// from cur, dedupes the inclusive JQL boundary issue, and emits created/
// changed issues plus tombstones for issues that reach a configured
// deleted-like status. A cursor the source rejects surfaces as
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
	cl := c.newClient(conf, cfg.Token)
	c.log.Info("jira incremental sync starting",
		"instance_id", cfg.InstanceID, "from_updated", st.updatedJQL)
	return c.scan(ctx, cfg, conf, cl, emit, st, true)
}

// scan pages POST /rest/api/3/search for the JQL implied by start, emits a
// Document (or tombstone) per issue, and checkpoints the per-issue cursor
// after each page. boundaryKey from start is skipped so the inclusive JQL
// "updated >=" boundary issue is not re-emitted. When expireOnReject is true a
// 400 from the source (a bad/expired JQL bound) is mapped to
// sdk.ErrCursorExpired. It returns the cursor of the newest issue seen, or
// start's cursor when nothing was emitted.
func (c *Connector) scan(ctx context.Context, cfg sdk.Config, conf instanceConfig, cl *client, emit sdk.Emit, start cursorState, expireOnReject bool) (sdk.Cursor, error) {
	tenant := string(cfg.Tenant.TenantID())
	base := conf.baseURL()
	deleted := deletedStatusSet(conf.DeletedStatuses)

	jql := withProjectFilter(start.jql(), conf.ProjectKeys)
	cursor := startCursor(start)
	startAt := 0

	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		page, err := cl.search(ctx, jql, startAt, pageSize)
		if err != nil {
			if expireOnReject && isCursorRejected(err) {
				return "", fmt.Errorf("jira: search rejected the cursor bound (%v): %w", err, sdk.ErrCursorExpired)
			}
			return "", fmt.Errorf("jira: search startAt=%d: %w", startAt, err)
		}

		for _, iss := range page.Issues {
			// doc_id derives from the immutable iss.ID, so an issue missing it
			// cannot produce a valid Document; the key is still needed for the
			// boundary dedupe and the display title.
			if iss == nil || iss.ID == "" || iss.Key == "" {
				continue
			}
			// Dedupe the inclusive JQL boundary issue from the previous pass.
			if iss.Key == start.boundaryKey {
				continue
			}
			if err := c.emitIssue(ctx, tenant, base, deleted, iss, emit); err != nil {
				return "", err
			}
			if next, ok := cursorForIssue(iss); ok {
				cursor = next
			}
		}

		startAt += len(page.Issues)
		if len(page.Issues) == 0 || startAt >= page.Total {
			break
		}
		if err := cfg.Checkpoint(ctx, cursor); err != nil {
			return "", err
		}
	}

	c.log.Info("jira sync complete", "instance_id", cfg.InstanceID, "cursor", string(cursor))
	return cursor, nil
}

// emitIssue emits one issue as either a live Document or, when its status is a
// configured deleted-like status, a tombstone.
func (c *Connector) emitIssue(ctx context.Context, tenant, base string, deleted map[string]bool, iss *issue, emit sdk.Emit) error {
	if isDeletedStatus(deleted, iss) {
		return emit(ctx, c.tombstoneDocument(tenant, iss))
	}
	return emit(ctx, issueDocument(tenant, base, iss))
}

// startCursor returns the cursor to fall back to when a scan emits nothing:
// the cursor reconstructed from start (so an empty incremental pass returns
// the same cursor it was handed). The empty start yields the empty cursor.
func startCursor(start cursorState) sdk.Cursor {
	if start.updatedJQL == "" {
		return ""
	}
	return makeCursor(start.updatedJQL, start.boundaryKey)
}

// withProjectFilter prepends a "project in (...)" clause to jql when the
// instance restricts the sync to specific projects. The clause is ANDed before
// any "order by". Empty keys leave the JQL unchanged.
func withProjectFilter(jql string, keys []string) string {
	clean := cleanKeys(keys)
	if len(clean) == 0 {
		return jql
	}
	quoted := make([]string, len(clean))
	for i, k := range clean {
		quoted[i] = "'" + k + "'"
	}
	filter := fmt.Sprintf("project in (%s)", strings.Join(quoted, ", "))

	lower := strings.ToLower(jql)
	if idx := strings.Index(lower, "order by"); idx >= 0 {
		head := strings.TrimSpace(jql[:idx])
		order := jql[idx:]
		if head == "" {
			return filter + " " + order
		}
		return head + " AND " + filter + " " + order
	}
	if strings.TrimSpace(jql) == "" {
		return filter
	}
	return jql + " AND " + filter
}

// cleanKeys trims and drops empty project keys.
func cleanKeys(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// deletedStatusSet builds a case-insensitive lookup of the configured
// deleted-like status names.
func deletedStatusSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			set[strings.ToLower(n)] = true
		}
	}
	return set
}

// isDeletedStatus reports whether iss is in a configured deleted-like status.
func isDeletedStatus(deleted map[string]bool, iss *issue) bool {
	if len(deleted) == 0 || iss.Fields.Status == nil {
		return false
	}
	return deleted[strings.ToLower(iss.Fields.Status.Name)]
}

// HandleWebhook implements sdk.Connector. Jira webhooks are deferred for M2
// (Spec.SupportsWebhook is false), so the connector has no push path and the
// hub schedules polling only.
func (c *Connector) HandleWebhook(_ context.Context, _ sdk.Config, _ *http.Request, _ sdk.Emit) error {
	return sdk.ErrWebhookUnsupported
}
