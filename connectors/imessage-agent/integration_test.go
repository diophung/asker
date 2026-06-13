package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asker/asker/connectors/imessage-agent/internal/imsg"
)

// seedChatDB creates a minimal chat.db with the subset of the real Messages
// schema the agent queries (message, handle, chat, chat_message_join) and a few
// rows, by shelling out to the same sqlite3 binary the agent uses. It skips the
// test if sqlite3 is unavailable (non-macOS dev box without it). This makes the
// sqlite3-shelling reader and the gateway upload path genuinely covered in CI
// rather than only manually.
func seedChatDB(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not on PATH; skipping live-db integration test")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "chat.db")

	// appleNanos for 2021-06-01T12:00:00Z.
	base := int64(time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC).
		Sub(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)))

	schema := fmt.Sprintf(`
CREATE TABLE handle (ROWID INTEGER PRIMARY KEY, id TEXT);
CREATE TABLE chat (ROWID INTEGER PRIMARY KEY, guid TEXT, display_name TEXT);
CREATE TABLE message (ROWID INTEGER PRIMARY KEY, text TEXT, handle_id INTEGER, is_from_me INTEGER, date INTEGER, service TEXT);
CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER);

INSERT INTO handle (ROWID, id) VALUES (1, '+15551234567');
INSERT INTO chat (ROWID, guid, display_name) VALUES (1, 'iMessage;-;+15551234567', '');
INSERT INTO chat (ROWID, guid, display_name) VALUES (2, 'iMessage;+;chat42', 'Lunch Crew');

INSERT INTO message (ROWID, text, handle_id, is_from_me, date, service) VALUES (1, 'hey lunch?', 1, 0, %d, 'iMessage');
INSERT INTO message (ROWID, text, handle_id, is_from_me, date, service) VALUES (2, 'yes!', 1, 1, %d, 'iMessage');
INSERT INTO message (ROWID, text, handle_id, is_from_me, date, service) VALUES (3, 'group hi', 1, 0, %d, 'iMessage');

INSERT INTO chat_message_join (chat_id, message_id) VALUES (1, 1);
INSERT INTO chat_message_join (chat_id, message_id) VALUES (1, 2);
INSERT INTO chat_message_join (chat_id, message_id) VALUES (2, 3);
`, base, base+int64(time.Minute), base+2*int64(time.Minute))

	cmd := exec.Command("sqlite3", db)
	cmd.Stdin = strings.NewReader(schema)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed chat.db: %v\n%s", err, out)
	}
	return db
}

func TestSQLite3ReaderReadsSeededDB(t *testing.T) {
	db := seedChatDB(t)
	reader, err := newSQLite3Reader(db)
	if err != nil {
		t.Fatalf("newSQLite3Reader: %v", err)
	}

	rows, err := reader.ReadSince(context.Background(), 0)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("ReadSince(0) = %d rows, want 3", len(rows))
	}
	// Ordered ascending by ROWID.
	if rows[0].RowID != 1 || rows[2].RowID != 3 {
		t.Errorf("rows not ordered ascending: %d..%d", rows[0].RowID, rows[2].RowID)
	}
	// is_from_me decoded.
	if rows[0].IsFromMe || !rows[1].IsFromMe {
		t.Errorf("is_from_me decode wrong: %v %v", rows[0].IsFromMe, rows[1].IsFromMe)
	}
	// Group chat display name flows through.
	if rows[2].ChatName != "Lunch Crew" {
		t.Errorf("chat_name = %q, want Lunch Crew", rows[2].ChatName)
	}

	// Incremental: since=1 drops the first row.
	since1, err := reader.ReadSince(context.Background(), 1)
	if err != nil {
		t.Fatalf("ReadSince(1): %v", err)
	}
	if len(since1) != 2 || since1[0].RowID != 2 {
		t.Errorf("ReadSince(1) = %d rows starting at %d, want 2 starting at 2", len(since1), firstRowID(since1))
	}
}

func TestRealMainEndToEndHappyPath(t *testing.T) {
	db := seedChatDB(t)

	var uploads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, _, err := r.FormFile("file"); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n := atomic.AddInt32(&uploads, 1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, `{"doc_id":"d-%d"}`, n)
	}))
	t.Cleanup(srv.Close)

	null := devNull(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	args := []string{
		"--db", db,
		"--state", statePath,
		"--gateway-url", srv.URL,
		"--token", "test-token",
	}
	if err := realMain(args, null, null); err != nil {
		t.Fatalf("realMain: %v", err)
	}
	// Two chats -> two transcript uploads.
	if got := atomic.LoadInt32(&uploads); got != 2 {
		t.Errorf("uploads = %d, want 2", got)
	}
	// State advanced to the max ROWID (3).
	st, err := loadState(statePath)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if st.LastRowID != 3 {
		t.Errorf("state LastRowID = %d, want 3", st.LastRowID)
	}

	// A second run with the advanced state uploads nothing new.
	if err := realMain(args, null, null); err != nil {
		t.Fatalf("second realMain: %v", err)
	}
	if got := atomic.LoadInt32(&uploads); got != 2 {
		t.Errorf("uploads after no-op run = %d, want still 2", got)
	}
}

func TestRealMainDryRunEndToEnd(t *testing.T) {
	db := seedChatDB(t)
	out, err := os.CreateTemp(t.TempDir(), "dryrun-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close() })
	null := devNull(t)
	statePath := filepath.Join(t.TempDir(), "state.json")

	args := []string{"--dry-run", "--db", db, "--state", statePath}
	if err := realMain(args, out, null); err != nil {
		t.Fatalf("realMain dry-run: %v", err)
	}
	// Dry-run must not persist state.
	if _, err := os.Stat(statePath); err == nil {
		t.Error("dry-run wrote a state file; it must not")
	}
	data, _ := os.ReadFile(out.Name())
	if !strings.Contains(string(data), "hey lunch?") {
		t.Errorf("dry-run output missing transcript body:\n%s", data)
	}
}

func firstRowID(rows []imsg.Row) int64 {
	if len(rows) == 0 {
		return -1
	}
	return rows[0].RowID
}
