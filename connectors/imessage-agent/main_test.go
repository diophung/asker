package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// devNull opens os.DevNull as the stdout/stderr sink for realMain tests.
func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestRealMainRequiresGatewayURL(t *testing.T) {
	null := devNull(t)
	args := []string{
		"--db", filepath.Join(t.TempDir(), "chat.db"),
		"--state", filepath.Join(t.TempDir(), "state.json"),
		"--token", "abc",
	}
	err := realMain(args, null, null)
	if err == nil || !strings.Contains(err.Error(), "gateway-url") {
		t.Fatalf("err = %v, want a missing --gateway-url error", err)
	}
}

func TestRealMainRequiresToken(t *testing.T) {
	null := devNull(t)
	args := []string{
		"--db", filepath.Join(t.TempDir(), "chat.db"),
		"--state", filepath.Join(t.TempDir(), "state.json"),
		"--gateway-url", "http://127.0.0.1:8080",
	}
	err := realMain(args, null, null)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("err = %v, want a missing --token error", err)
	}
}

func TestRealMainCorruptStateFails(t *testing.T) {
	null := devNull(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--dry-run", "--state", statePath}
	err := realMain(args, null, null)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("err = %v, want a corrupt-state error", err)
	}
}

func TestRealMainUnknownFlag(t *testing.T) {
	null := devNull(t)
	if err := realMain([]string{"--nope"}, null, null); err == nil {
		t.Fatal("realMain should error on an unknown flag")
	}
}

// TestRealMainDryRunMissingDB exercises the dry-run path far enough to fail at
// the (absent) chat.db, confirming dry-run skips the gateway/token checks but
// still needs a readable database.
func TestRealMainDryRunMissingDB(t *testing.T) {
	null := devNull(t)
	args := []string{
		"--dry-run",
		"--db", filepath.Join(t.TempDir(), "absent.db"),
		"--state", filepath.Join(t.TempDir(), "state.json"),
	}
	err := realMain(args, null, null)
	if err == nil {
		t.Fatal("dry-run with a missing db should fail at the reader")
	}
	if !strings.Contains(err.Error(), "chat.db") && !strings.Contains(err.Error(), "sqlite3") {
		t.Errorf("err = %v, want it to mention chat.db or sqlite3", err)
	}
}
