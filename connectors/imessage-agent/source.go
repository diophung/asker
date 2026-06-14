package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"

	"github.com/asker/asker/connectors/imessage-agent/internal/imsg"
)

// messageReader reads chat.db rows newer than the given high-water ROWID. It is
// an interface so the CLI's mapping/orchestration logic can be unit-tested with
// a fake, and the real sqlite3-shelling implementation is exercised manually
// against a live chat.db (documented in the README).
type messageReader interface {
	// ReadSince returns all message rows with ROWID strictly greater than
	// sinceRowID, ordered ascending by ROWID.
	ReadSince(ctx context.Context, sinceRowID int64) ([]imsg.Row, error)
}

// sqlite3Reader reads chat.db by shelling out to the system "sqlite3" binary
// (present on macOS): there is no SQLite driver in the frozen go.mod, so the
// agent runs `sqlite3 -json <db> "<query>"` and parses the JSON rows. The DB is
// opened read-only via a file: URI so the agent never writes to the user's
// Messages database.
type sqlite3Reader struct {
	// sqlite3Path is the sqlite3 executable (resolved from PATH by default).
	sqlite3Path string
	// dbPath is the chat.db location.
	dbPath string
}

// newSQLite3Reader constructs a reader after verifying both the sqlite3 binary
// and the chat.db file are present, returning a clear, actionable error if
// either is missing (the common "Full Disk Access not granted" / "not on
// macOS" cases). The caller turns that error into a non-zero exit.
func newSQLite3Reader(dbPath string) (*sqlite3Reader, error) {
	sqlite3Path, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil, fmt.Errorf("the 'sqlite3' command was not found on PATH; it ships with macOS — are you on a Mac? (%w)", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("chat.db not found at %s; on a Mac it lives under ~/Library/Messages (pass --db to override)", dbPath)
		}
		if errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("permission denied reading %s; grant your terminal Full Disk Access in System Settings → Privacy & Security", dbPath)
		}
		return nil, fmt.Errorf("cannot access chat.db at %s: %w", dbPath, err)
	}
	return &sqlite3Reader{sqlite3Path: sqlite3Path, dbPath: dbPath}, nil
}

// sqlite3DBURI builds the connection string the agent hands to sqlite3 for a
// read-only open of chat.db.
//
// We open with mode=ro (a normal read transaction) and NOT immutable=1.
// immutable=1 promises SQLite the file never changes, so it skips locking and
// the WAL entirely: against the live, concurrently-written Messages database
// that risks torn reads and silently drops recently-sent (WAL-resident,
// not-yet-checkpointed) messages — the freshest ones we most want. mode=ro
// keeps SQLite's integrity checks and WAL handling on, so a normal read sees
// WAL-committed recent messages, while still guaranteeing we never write.
func sqlite3DBURI(dbPath string) string {
	return "file:" + dbPath + "?mode=ro"
}

// sqlite3Args builds the argv (after the binary) for a read-only chat query.
// The -readonly flag is the belt-and-suspenders companion to mode=ro: even if
// the URI were ever altered, sqlite3 still refuses to open the DB for writing.
func sqlite3Args(dbURI, query string) []string {
	return []string{"-json", "-readonly", dbURI, query}
}

// ReadSince runs the chat query through sqlite3 -json and parses the rows. The
// database is attached read-only. A non-zero exit from sqlite3 surfaces its
// stderr (which, for a locked/unauthorized database, names the Full Disk
// Access problem) — but never the row contents.
func (r *sqlite3Reader) ReadSince(ctx context.Context, sinceRowID int64) ([]imsg.Row, error) {
	query := fmt.Sprintf(chatQuery, sinceRowID)
	dbURI := sqlite3DBURI(r.dbPath)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.sqlite3Path, sqlite3Args(dbURI, query)...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// stderr is safe to surface: sqlite3 errors describe the DB/permission
		// state, never message bodies.
		msg := bytes.TrimSpace(stderr.Bytes())
		if len(msg) > 0 {
			return nil, fmt.Errorf("sqlite3 failed: %s: %w", msg, err)
		}
		return nil, fmt.Errorf("sqlite3 failed: %w", err)
	}
	return parseRows(stdout.Bytes())
}
