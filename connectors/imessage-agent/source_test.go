package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLite3DBURIIsReadOnlyAndNotImmutable(t *testing.T) {
	uri := sqlite3DBURI("/Users/me/Library/Messages/chat.db")
	if !strings.HasPrefix(uri, "file:") {
		t.Errorf("uri = %q, want a file: URI", uri)
	}
	if !strings.Contains(uri, "/Users/me/Library/Messages/chat.db") {
		t.Errorf("uri = %q, want it to embed the db path", uri)
	}
	if !strings.Contains(uri, "mode=ro") {
		t.Errorf("uri = %q, want mode=ro for a read-only open", uri)
	}
	// immutable=1 would make SQLite skip the WAL and locking, risking torn
	// reads and dropping recent (WAL-resident) messages from the live DB.
	if strings.Contains(uri, "immutable") {
		t.Errorf("uri = %q, must not set immutable (it breaks reading the live WAL)", uri)
	}
}

func TestSQLite3ArgsForceReadOnly(t *testing.T) {
	uri := sqlite3DBURI("/db/chat.db")
	args := sqlite3Args(uri, "SELECT 1;")

	var hasJSON, hasReadonly, hasURI, hasQuery bool
	for _, a := range args {
		switch a {
		case "-json":
			hasJSON = true
		case "-readonly":
			hasReadonly = true
		case uri:
			hasURI = true
		case "SELECT 1;":
			hasQuery = true
		}
	}
	if !hasJSON {
		t.Errorf("args = %q, want -json", args)
	}
	// The -readonly flag is the guarantee we never write the user's Messages DB.
	if !hasReadonly {
		t.Errorf("args = %q, want -readonly", args)
	}
	if !hasURI {
		t.Errorf("args = %q, want the db URI %q", args, uri)
	}
	if !hasQuery {
		t.Errorf("args = %q, want the SQL query", args)
	}
}

func TestNewSQLite3ReaderMissingDB(t *testing.T) {
	// sqlite3 is present on macOS/CI runners; if it is not, the error names
	// that instead — either way newSQLite3Reader must fail clearly.
	_, err := newSQLite3Reader(filepath.Join(t.TempDir(), "absent-chat.db"))
	if err == nil {
		t.Fatal("newSQLite3Reader should fail for a missing database")
	}
	msg := err.Error()
	if !strings.Contains(msg, "chat.db") && !strings.Contains(msg, "sqlite3") {
		t.Errorf("error = %q, want it to mention chat.db or sqlite3", msg)
	}
}
