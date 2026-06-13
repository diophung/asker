package gdrive

import (
	"context"
	"errors"
	"fmt"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// FullSync implements sdk.Connector: it captures the changes start page token
// FIRST (so every edit racing the backfill is replayed by the first
// incremental pass and converges via idempotent (doc_id, version_etag)
// upserts), then pages files.list, emitting a Document per file and
// checkpointing "start:<token>|files:<pageToken>" after every completed page.
// It returns "page:<startToken>", the steady-state incremental cursor.
func (c *Connector) FullSync(ctx context.Context, cfg sdk.Config, emit sdk.Emit) (sdk.Cursor, error) {
	conf, err := parseConfig(cfg.ConfigJSON)
	if err != nil {
		return "", err
	}
	cl, err := c.newClient(conf, cfg.Token)
	if err != nil {
		return "", fmt.Errorf("gdrive: build API client: %w", err)
	}
	startToken, err := cl.startToken(ctx)
	if err != nil {
		return "", fmt.Errorf("gdrive: seed changes cursor: %w", err)
	}
	c.log.Info("gdrive full sync starting",
		"instance_id", cfg.InstanceID, "start_token", startToken)
	return c.backfill(ctx, cfg, cl, emit, startToken, "")
}

// backfill pages files.list starting at filesPageToken (empty = first page),
// emits a Document per file, and checkpoints after every completed page. It is
// shared by FullSync (filesPageToken "") and by IncrementalSync when the hub
// replays a mid-backfill checkpoint cursor.
func (c *Connector) backfill(ctx context.Context, cfg sdk.Config, cl *client, emit sdk.Emit, startToken, filesPageToken string) (sdk.Cursor, error) {
	tenant := string(cfg.Tenant.TenantID())
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		page, err := cl.listFiles(ctx, filesPageToken, listPageSize)
		if err != nil {
			return "", fmt.Errorf("gdrive: files.list: %w", err)
		}
		for _, f := range page.Files {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			doc, err := c.documentForFile(ctx, cl, tenant, f)
			if err != nil {
				if isNotFound(err) {
					// Vanished between list and detail fetch; the deletion is a
					// change newer than startToken, so the first incremental
					// pass reconciles it. Skipping is safe.
					c.log.Info("file vanished during backfill, skipping", "file_id", f.ID)
					continue
				}
				return "", err
			}
			if doc == nil {
				continue // skipped (e.g. folder)
			}
			if err := emit(ctx, doc); err != nil {
				return "", err
			}
		}

		if page.NextPageToken == "" {
			break
		}
		filesPageToken = page.NextPageToken
		if err := cfg.Checkpoint(ctx, backfillCursor(startToken, filesPageToken)); err != nil {
			return "", err
		}
	}

	cur := incrementalCursor(startToken)
	if err := cfg.Checkpoint(ctx, cur); err != nil {
		return "", err
	}
	c.log.Info("gdrive backfill complete", "instance_id", cfg.InstanceID, "cursor", string(cur))
	return cur, nil
}

// IncrementalSync implements sdk.Connector. A steady-state cursor
// ("page:<token>") replays the changes feed from <token>; a mid-backfill
// checkpoint ("start:<token>|files:<pageToken>") resumes the interrupted
// backfill (the hub replays checkpoints verbatim into IncrementalSync per the
// sdk.Cursor contract). An unparseable cursor and a Drive 400/410 invalid page
// token both surface as sdk.ErrCursorExpired so the hub restarts a full sync.
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
		return "", fmt.Errorf("gdrive: build API client: %w", err)
	}
	if st.backfill {
		c.log.Info("resuming interrupted gdrive backfill",
			"instance_id", cfg.InstanceID, "files_page_token", st.filesPageToken)
		return c.backfill(ctx, cfg, cl, emit, st.changesToken, st.filesPageToken)
	}
	return c.replayChanges(ctx, cfg, cl, emit, st.changesToken)
}

// replayChanges pages the changes feed from pageToken, emitting an upsert for
// every changed/created file and a tombstone for every removed/trashed file,
// then returns the advanced cursor ("page:<newStartPageToken>"). Drive folds
// repeated edits to one file into the latest change, so a simple per-page walk
// is correct; a trailing newStartPageToken marks the end of the feed.
func (c *Connector) replayChanges(ctx context.Context, cfg sdk.Config, cl *client, emit sdk.Emit, pageToken string) (sdk.Cursor, error) {
	tenant := string(cfg.Tenant.TenantID())
	newStart := ""
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		page, err := cl.listChanges(ctx, pageToken)
		if err != nil {
			if errors.Is(err, errExpiredToken) {
				return "", fmt.Errorf("gdrive: changes.list pageToken invalid: %w", sdk.ErrCursorExpired)
			}
			return "", fmt.Errorf("gdrive: changes.list: %w", err)
		}

		for _, ch := range page.Changes {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if ch.ChangeType != "" && ch.ChangeType != "file" {
				continue // shared-drive membership changes etc. — not a file
			}
			if ch.FileID == "" {
				continue
			}
			doc, err := c.documentForChange(ctx, cl, tenant, ch)
			if err != nil {
				return "", err
			}
			if doc == nil {
				continue
			}
			if err := emit(ctx, doc); err != nil {
				return "", err
			}
		}

		switch {
		case page.NextPageToken != "":
			pageToken = page.NextPageToken
		case page.NewStartPageToken != "":
			newStart = page.NewStartPageToken
		default:
			// Defensive: a well-formed final page always carries
			// newStartPageToken; if absent, keep the cursor we replayed from so
			// the next poll retries rather than losing position.
			newStart = pageToken
		}
		if newStart != "" {
			break
		}
	}
	return incrementalCursor(newStart), nil
}

// documentForChange maps one changes-feed entry to a Document. A removed or
// trashed file becomes a tombstone; otherwise it is an upsert built from the
// change's embedded file (refreshed via files.get only if the change omits it).
func (c *Connector) documentForChange(ctx context.Context, cl *client, tenant string, ch *change) (*askerv1.Document, error) {
	if ch.Removed || (ch.File != nil && ch.File.Trashed) {
		return c.tombstoneDocument(tenant, ch.FileID, ch.Time), nil
	}
	f := ch.File
	if f == nil {
		got, err := cl.getFile(ctx, ch.FileID)
		if err != nil {
			if isNotFound(err) {
				// Gone since the change was recorded; tombstone it now rather
				// than wait for another poll cycle.
				return c.tombstoneDocument(tenant, ch.FileID, ch.Time), nil
			}
			return nil, fmt.Errorf("gdrive: files.get %s: %w", ch.FileID, err)
		}
		f = got
	}
	if f.Trashed {
		return c.tombstoneDocument(tenant, ch.FileID, ch.Time), nil
	}
	return c.documentForFile(ctx, cl, tenant, f)
}

// documentForFile builds the live Document for a file: it fetches the ACL
// (permissions.list) and the body (export / alt=media, within the size cap),
// then maps. Folders are skipped (return nil, nil). A 404 on the body fetch
// degrades to an empty body rather than failing the whole sync.
func (c *Connector) documentForFile(ctx context.Context, cl *client, tenant string, f *driveFile) (*askerv1.Document, error) {
	if f == nil || f.ID == "" {
		return nil, errors.New("gdrive: file has no id")
	}
	if isFolder(f.MimeType) {
		return nil, nil
	}

	perms, err := cl.listPermissions(ctx, f.ID)
	if err != nil {
		if isNotFound(err) {
			// File vanished before its ACL could be read; let the caller treat
			// the not-found as a vanished file (skip in backfill / tombstone in
			// incremental).
			return nil, err
		}
		return nil, fmt.Errorf("gdrive: permissions.list %s: %w", f.ID, err)
	}
	acl := aclFromPermissions(f, perms)

	body, err := c.extractBody(ctx, cl, f)
	if err != nil {
		if isNotFound(err) {
			return nil, err
		}
		// A non-fatal body error (e.g. an unexportable format) still yields a
		// searchable metadata-only document.
		c.log.Warn("gdrive body extraction failed; indexing metadata only",
			"file_id", f.ID, "mime_type", f.MimeType, "error", err)
		body = ""
	}
	return fileDocument(tenant, f, body, acl), nil
}

// extractBody returns the file's searchable text: Google-native docs are
// exported as text/plain; text/* files are downloaded via alt=media; every
// other binary (PDF, image, office doc) is left empty for M3 extraction.
func (c *Connector) extractBody(ctx context.Context, cl *client, f *driveFile) (string, error) {
	switch {
	case isGoogleDoc(f.MimeType):
		return cl.exportText(ctx, f.ID)
	case isPlainText(f.MimeType):
		return cl.downloadText(ctx, f.ID)
	default:
		return "", nil
	}
}
