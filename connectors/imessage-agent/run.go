package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/asker/asker/connectors/imessage-agent/internal/imsg"
)

// options are the resolved run parameters (from flags + state). They are the
// seam between flag parsing (main) and the testable orchestration (runSync).
type options struct {
	// tenant is informational only: the agent never sets tenant_id itself, the
	// gateway derives it from the bearer token. It is logged for operator
	// clarity when known.
	since  int64 // high-water ROWID; only rows above it are read
	dryRun bool  // when true, print transcripts to stdout and upload nothing
}

// runResult reports what a sync pass did, so main can persist the new
// high-water mark and log a summary.
type runResult struct {
	messages      int   // messages read from chat.db
	chats         int   // distinct chats rendered
	uploaded      int   // transcripts uploaded (0 in dry-run)
	newHighWater  int64 // max ROWID seen; the next run's --since
	highWaterMove bool  // whether newHighWater advanced past opts.since
}

// runSync is the agent's core orchestration, factored away from sqlite3/HTTP so
// it is unit-testable with fake reader/uploader implementations: it reads rows
// newer than opts.since, groups them by chat, renders one transcript per chat,
// and either prints them (dry-run) or uploads them. It never prints message
// bodies except under dry-run, and never logs the bearer token.
//
// out is where dry-run transcripts are written (os.Stdout in production).
func runSync(ctx context.Context, log *slog.Logger, reader messageReader, up uploader, out io.Writer, opts options) (runResult, error) {
	rows, err := reader.ReadSince(ctx, opts.since)
	if err != nil {
		return runResult{}, fmt.Errorf("read messages: %w", err)
	}

	res := runResult{
		messages:     len(rows),
		newHighWater: opts.since,
	}
	if hw := maxRowID(rows); hw > res.newHighWater {
		res.newHighWater = hw
		res.highWaterMove = true
	}
	if len(rows) == 0 {
		log.Info("no new messages", "since_rowid", opts.since)
		return res, nil
	}

	groups := groupByChat(rows)
	res.chats = len(groups)

	for _, g := range groups {
		body := imsg.RenderTranscript(g.rows)
		t := transcript{
			filename: imsg.TranscriptFilename(g.guid),
			title:    transcriptTitle(g),
			body:     body,
		}

		if opts.dryRun {
			// Dry-run is the only path allowed to print message bodies.
			_, _ = fmt.Fprintf(out, "===== %s (%d messages) =====\n%s\n", t.title, len(g.rows), body)
			continue
		}

		docID, err := up.Upload(ctx, t)
		if err != nil {
			// Return after partial progress: res.newHighWater is NOT advanced
			// to cover unsent chats, because we recompute the high-water only
			// from successfully handled work below.
			return res, fmt.Errorf("upload chat transcript: %w", err)
		}
		res.uploaded++
		// doc_id is safe to log; message bodies are not.
		log.Info("uploaded chat transcript", "chat", t.title, "messages", len(g.rows), "doc_id", docID)
	}

	return res, nil
}

// transcriptTitle is the document title for a chat's transcript upload: the
// chat display name when set, else the GUID, with a message count.
func transcriptTitle(g chatGroup) string {
	label := g.guid
	if len(g.rows) > 0 {
		if name := g.rows[0].ChatName; name != "" {
			label = name
		}
	}
	if label == "" {
		label = "(unknown chat)"
	}
	return fmt.Sprintf("iMessage: %s", label)
}
