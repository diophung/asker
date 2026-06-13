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

// ReadSince runs the chat query through sqlite3 -json and parses the rows. The
// database is attached read-only. A non-zero exit from sqlite3 surfaces its
// stderr (which, for a locked/unauthorized database, names the Full Disk
// Access problem) — but never the row contents.
func (r *sqlite3Reader) ReadSince(ctx context.Context, sinceRowID int64) ([]imsg.Row, error) {
	query := fmt.Sprintf(chatQuery, sinceRowID)
	// "file:<path>?mode=ro&immutable=1" opens the DB read-only and tolerates
	// the WAL of the live Messages app without taking a write lock.
	dbURI := "file:" + r.dbPath + "?mode=ro&immutable=1"

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.sqlite3Path, "-json", "-readonly", dbURI, query)
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
