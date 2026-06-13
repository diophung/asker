package main

import (
	"path/filepath"
	"strings"
	"testing"
)

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
