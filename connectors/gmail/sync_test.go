package gmail

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// TestFullSyncSeeded: N seeded messages -> N canonical documents and the
// steady-state cursor.
func TestFullSyncSeeded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seeded := f.seed(12, 21)
	cfg := f.connectorConfig("", nil)
	c := newTestConnector()

	var rec connectortest.EmitRecorder
	cur, err := c.FullSync(context.Background(), cfg, rec.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if want := incrementalCursor(seeded.HistoryID); cur != want {
		t.Errorf("cursor = %q, want %q", cur, want)
	}

	docs := rec.Docs()
	if len(docs) != 12 {
		t.Fatalf("emitted %d documents, want 12", len(docs))
	}
	seen := make(map[string]bool)
	for _, doc := range docs {
		connectortest.ValidateDocument(t, cfg, doc)
		if doc.GetType() != askerv1.DocType_EMAIL {
			t.Errorf("doc %s type = %v, want EMAIL", doc.GetDocId(), doc.GetType())
		}
		if doc.GetTombstone().GetDeleted() {
			t.Errorf("doc %s is a tombstone in a full sync", doc.GetDocId())
		}
		if doc.GetBodyText() == "" {
			t.Errorf("doc %s has no body", doc.GetDocId())
		}
		if seen[doc.GetDocId()] {
			t.Errorf("doc %s emitted twice", doc.GetDocId())
		}
		seen[doc.GetDocId()] = true
	}
	for _, id := range f.listAllIDs() {
		if !seen[sdk.DocID("gmail", id)] {
			t.Errorf("message %s missing from full sync", id)
		}
	}
}

// TestFullSyncGolden checks the full mapping of one known message.
func TestFullSyncGolden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	msg := f.addMessage("Ava Alvarez <ava@example.com>", testEmail,
		"Quarterly review", "Numbers look good.\n\nMargin is tighter than expected.")
	cfg := f.connectorConfig("", nil)
	c := newTestConnector()

	var rec connectortest.EmitRecorder
	if _, err := c.FullSync(context.Background(), cfg, rec.Emit); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	docs := rec.Docs()
	if len(docs) != 1 {
		t.Fatalf("emitted %d documents, want 1", len(docs))
	}
	doc := docs[0]
	connectortest.ValidateDocument(t, cfg, doc)

	if got, want := doc.GetDocId(), sdk.DocID("gmail", msg.ID); got != want {
		t.Errorf("doc_id = %q, want %q", got, want)
	}
	if doc.GetConnectorId() != "gmail" || doc.GetSourceNativeId() != msg.ID {
		t.Errorf("identity = (%q, %q), want (gmail, %q)", doc.GetConnectorId(), doc.GetSourceNativeId(), msg.ID)
	}
	if doc.GetType() != askerv1.DocType_EMAIL {
		t.Errorf("type = %v, want EMAIL", doc.GetType())
	}
	if doc.GetTitle() != "Quarterly review" {
		t.Errorf("title = %q, want %q", doc.GetTitle(), "Quarterly review")
	}
	if want := "Numbers look good.\n\nMargin is tighter than expected."; doc.GetBodyText() != want {
		t.Errorf("body = %q, want %q", doc.GetBodyText(), want)
	}
	if got, want := doc.GetVersionEtag(), strconv.FormatUint(msg.HistoryID, 10); got != want {
		t.Errorf("version_etag = %q, want %q", got, want)
	}
	if got, want := doc.GetTs().GetCreated().AsTime(), time.UnixMilli(msg.InternalDate).UTC(); !got.Equal(want) {
		t.Errorf("ts.created = %v, want %v", got, want)
	}
	if doc.GetTs().GetIngested() != nil {
		t.Error("ts.ingested set; the hub stamps it")
	}

	wantParticipants := []*askerv1.Participant{
		{Name: "Ava Alvarez", Email: "ava@example.com", Role: "from"},
		{Email: testEmail, Role: "to"},
	}
	gotParticipants := doc.GetParticipants()
	if len(gotParticipants) != len(wantParticipants) {
		t.Fatalf("participants = %v, want %v", gotParticipants, wantParticipants)
	}
	for i, want := range wantParticipants {
		got := gotParticipants[i]
		if got.GetName() != want.GetName() || got.GetEmail() != want.GetEmail() || got.GetRole() != want.GetRole() {
			t.Errorf("participant[%d] = %v, want %v", i, got, want)
		}
	}

	wantMeta := map[string]string{
		"from":       "Ava Alvarez <ava@example.com>",
		"to":         testEmail,
		"thread_id":  msg.ThreadID,
		"message_id": msg.ID,
	}
	gotMeta := doc.GetMetadata()
	if len(gotMeta) != len(wantMeta) {
		t.Errorf("metadata = %v, want %v", gotMeta, wantMeta)
	}
	for k, want := range wantMeta {
		if gotMeta[k] != want {
			t.Errorf("metadata[%q] = %q, want %q", k, gotMeta[k], want)
		}
	}
}

// TestFullSyncUsesGetProfile drives the canonical users.getProfile path (the
// raw fake lacks the endpoint; the harness shims it).
func TestFullSyncUsesGetProfile(t *testing.T) {
	t.Parallel()
	f := newFixtureWithProfile(t)
	seeded := f.seed(5, 3)
	cfg := f.connectorConfig("", nil)

	var rec connectortest.EmitRecorder
	cur, err := newTestConnector().FullSync(context.Background(), cfg, rec.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if want := incrementalCursor(seeded.HistoryID); cur != want {
		t.Errorf("cursor = %q, want %q", cur, want)
	}
	if len(rec.Docs()) != 5 {
		t.Fatalf("emitted %d documents, want 5", len(rec.Docs()))
	}
}

// TestFullSyncEmptyMailbox: no messages, cursor still established.
func TestFullSyncEmptyMailbox(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := f.connectorConfig("", nil)

	var rec connectortest.EmitRecorder
	cur, err := newTestConnector().FullSync(context.Background(), cfg, rec.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if cur != incrementalCursor(0) {
		t.Errorf("cursor = %q, want %q", cur, incrementalCursor(0))
	}
	if n := len(rec.Docs()); n != 0 {
		t.Fatalf("emitted %d documents from an empty mailbox", n)
	}
}

// TestFullSyncResume aborts a backfill at the first page checkpoint and
// resumes it from the checkpointed cursor: combined emissions cover every
// message exactly once (no duplicate, no missing).
func TestFullSyncResume(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seeded := f.seed(250, 9) // 3 pages at the contractual page size of 100
	c := newTestConnector()

	// Phase 1: fail the sync at the first mid-backfill checkpoint.
	abort := errors.New("hub restart")
	rec1 := &checkpointRecorder{fail: func(sdk.Cursor) error { return abort }}
	var emit1 connectortest.EmitRecorder
	_, err := c.FullSync(context.Background(), f.connectorConfig("", rec1.Checkpoint), emit1.Emit)
	if !errors.Is(err, abort) {
		t.Fatalf("FullSync error = %v, want %v", err, abort)
	}
	cursors1 := rec1.all()
	if len(cursors1) != 1 {
		t.Fatalf("checkpointed %d cursors, want 1", len(cursors1))
	}
	resumeCur := cursors1[0]
	if want := backfillCursor(seeded.HistoryID, "100"); resumeCur != want {
		t.Fatalf("checkpoint cursor = %q, want %q", resumeCur, want)
	}
	if len(emit1.Docs()) != 100 {
		t.Fatalf("phase 1 emitted %d documents, want one full page (100)", len(emit1.Docs()))
	}

	// Phase 2: the hub replays the checkpoint into IncrementalSync.
	rec2 := &checkpointRecorder{}
	var emit2 connectortest.EmitRecorder
	cfg2 := f.connectorConfig("", rec2.Checkpoint)
	cur, err := c.IncrementalSync(context.Background(), cfg2, resumeCur, emit2.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync (resume): %v", err)
	}
	if want := incrementalCursor(seeded.HistoryID); cur != want {
		t.Errorf("final cursor = %q, want %q", cur, want)
	}
	wantCheckpoints := []sdk.Cursor{
		backfillCursor(seeded.HistoryID, "200"),
		incrementalCursor(seeded.HistoryID),
	}
	if got := rec2.all(); fmt.Sprint(got) != fmt.Sprint(wantCheckpoints) {
		t.Errorf("resume checkpoints = %v, want %v", got, wantCheckpoints)
	}

	// No duplicate, no missing across both phases.
	emitted := make(map[string]int)
	for _, doc := range append(emit1.Docs(), emit2.Docs()...) {
		connectortest.ValidateDocument(t, cfg2, doc)
		emitted[doc.GetDocId()]++
	}
	all := f.listAllIDs()
	if len(all) != 250 {
		t.Fatalf("fixture has %d messages, want 250", len(all))
	}
	for _, id := range all {
		switch n := emitted[sdk.DocID("gmail", id)]; n {
		case 1:
		case 0:
			t.Errorf("message %s never emitted", id)
		default:
			t.Errorf("message %s emitted %d times", id, n)
		}
	}
	if len(emitted) != 250 {
		t.Errorf("emitted %d distinct documents, want 250", len(emitted))
	}
}

// TestIncrementalAddEditDelete replays an add + source-edit + delete
// sequence: the edit yields ONE upsert with a new version_etag (never a
// tombstone), the delete yields a tombstone, and the cursor advances.
func TestIncrementalAddEditDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	msgA := f.addMessage("ava@example.com", testEmail, "first", "body a")  // history 1
	msgB := f.addMessage("ben@example.com", testEmail, "second", "body b") // history 2
	cfg := f.connectorConfig("", nil)
	c := newTestConnector()

	var full connectortest.EmitRecorder
	cur, err := c.FullSync(context.Background(), cfg, full.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if len(full.Docs()) != 2 {
		t.Fatalf("full sync emitted %d documents, want 2", len(full.Docs()))
	}
	var etagA string
	for _, doc := range full.Docs() {
		if doc.GetSourceNativeId() == msgA.ID {
			etagA = doc.GetVersionEtag()
		}
	}

	edited := f.editMessage(msgA.ID, "", "body a v2")                       // history 3 (deleted) + 4 (added)
	msgC := f.addMessage("carol@example.com", testEmail, "third", "body c") // history 5
	f.deleteMessage(msgB.ID)                                                // history 6

	var rec connectortest.EmitRecorder
	cur, err = c.IncrementalSync(context.Background(), cfg, cur, rec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	docs := rec.Docs()
	if len(docs) != 3 {
		t.Fatalf("emitted %d documents, want 3 (edit upsert, add, tombstone)", len(docs))
	}
	for _, doc := range docs {
		connectortest.ValidateDocument(t, cfg, doc)
	}

	docA, docC, docB := docs[0], docs[1], docs[2]
	if docA.GetSourceNativeId() != msgA.ID {
		t.Fatalf("docs[0] is %s, want edited message %s", docA.GetSourceNativeId(), msgA.ID)
	}
	if docA.GetTombstone().GetDeleted() {
		t.Error("edited message replayed as a tombstone; last-event-per-id must win")
	}
	if docA.GetBodyText() != "body a v2" {
		t.Errorf("edited body = %q, want %q", docA.GetBodyText(), "body a v2")
	}
	if got, want := docA.GetVersionEtag(), strconv.FormatUint(edited.HistoryID, 10); got != want {
		t.Errorf("edited version_etag = %q, want %q", got, want)
	}
	if docA.GetVersionEtag() == etagA {
		t.Errorf("edited version_etag %q did not change from %q", docA.GetVersionEtag(), etagA)
	}

	if docC.GetSourceNativeId() != msgC.ID || docC.GetTombstone().GetDeleted() {
		t.Errorf("docs[1] = (%s, tombstone=%v), want upsert of %s",
			docC.GetSourceNativeId(), docC.GetTombstone().GetDeleted(), msgC.ID)
	}

	if docB.GetSourceNativeId() != msgB.ID || !docB.GetTombstone().GetDeleted() {
		t.Fatalf("docs[2] = (%s, tombstone=%v), want tombstone of %s",
			docB.GetSourceNativeId(), docB.GetTombstone().GetDeleted(), msgB.ID)
	}
	if docB.GetTombstone().GetDeletedAt() == nil {
		t.Error("tombstone deleted_at unset")
	}
	if docB.GetBodyText() != "" || len(docB.GetChunks()) != 0 {
		t.Error("tombstone carries a body")
	}

	if want := incrementalCursor(6); cur != want {
		t.Errorf("cursor = %q, want %q", cur, want)
	}
}

// TestIncrementalNoChanges: same cursor back, nothing emitted.
func TestIncrementalNoChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed(3, 5)
	cfg := f.connectorConfig("", nil)
	c := newTestConnector()

	cur, err := c.FullSync(context.Background(), cfg, sdkNopEmit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	var rec connectortest.EmitRecorder
	next, err := c.IncrementalSync(context.Background(), cfg, cur, rec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	if next != cur {
		t.Errorf("cursor advanced from %q to %q with no changes", cur, next)
	}
	if n := len(rec.Docs()); n != 0 {
		t.Errorf("emitted %d documents with no changes", n)
	}
}

// TestIncrementalStaleCursor: a history position the source cannot replay
// surfaces as sdk.ErrCursorExpired.
func TestIncrementalStaleCursor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed(3, 5)
	cfg := f.connectorConfig("", nil)
	c := newTestConnector()

	_, err := c.IncrementalSync(context.Background(), cfg, incrementalCursor(999999), sdkNopEmit)
	if !errors.Is(err, sdk.ErrCursorExpired) {
		t.Fatalf("stale cursor error = %v, want sdk.ErrCursorExpired", err)
	}
}

// TestIncrementalUnreadableCursor: cursors this connector never produced
// also recover via sdk.ErrCursorExpired (hub restarts the full sync).
func TestIncrementalUnreadableCursor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := f.connectorConfig("", nil)
	c := newTestConnector()

	for _, cur := range []sdk.Cursor{"", "bogus", "history:notanumber", "history:5|page:"} {
		_, err := c.IncrementalSync(context.Background(), cfg, cur, sdkNopEmit)
		if !errors.Is(err, sdk.ErrCursorExpired) {
			t.Errorf("cursor %q error = %v, want sdk.ErrCursorExpired", cur, err)
		}
	}
}

// sdkNopEmit discards documents (for tests that only care about cursors).
func sdkNopEmit(context.Context, *askerv1.Document) error { return nil }
