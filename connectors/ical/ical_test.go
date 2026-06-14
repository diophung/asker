package ical

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/asker/asker/connectors/sdk"
	"github.com/asker/asker/connectors/sdk/connectortest"
	"github.com/asker/asker/platform/tenancy"
)

// testInstanceID is the stable per-instance id the test configs carry. doc_ids
// are scoped by it (not the feed URL), so it appears in every wantDocID call.
const testInstanceID = "inst-1"

// newTestConnector returns a connector with a quiet logger.
func newTestConnector() sdk.Connector {
	return New(WithLogger(slog.New(slog.DiscardHandler)))
}

// configFor builds an sdk.Config whose feed_url points at baseURL.
func configFor(t *testing.T, baseURL string) sdk.Config {
	t.Helper()
	tc, err := tenancy.FromClaims(map[string]any{"tenant_id": "tenant-a", "sub": "user-a"})
	if err != nil {
		t.Fatalf("tenancy.FromClaims: %v", err)
	}
	raw, err := json.Marshal(map[string]string{"feed_url": baseURL})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return sdk.Config{
		Tenant:     tc,
		InstanceID: testInstanceID,
		ConfigJSON: raw,
		Checkpoint: sdk.NopCheckpoint,
	}
}

func loadServer(t *testing.T, cassette string) *connectortest.ReplayServer {
	t.Helper()
	cas, err := connectortest.LoadCassette(cassette)
	if err != nil {
		t.Fatalf("LoadCassette(%s): %v", cassette, err)
	}
	return connectortest.NewReplayServer(t, cas)
}

func TestSpec(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	connectortest.RunSpecChecks(t, c)

	spec := c.Spec()
	if spec.ID != "ical" {
		t.Errorf("spec.ID = %q, want %q", spec.ID, "ical")
	}
	if spec.AuthType != sdk.AuthNone {
		t.Errorf("spec.AuthType = %v, want AuthNone", spec.AuthType)
	}
	if spec.SupportsWebhook {
		t.Error("spec.SupportsWebhook = true, want false (feeds are pull-only)")
	}

	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(spec.ConfigSchema, &schema); err != nil {
		t.Fatalf("ConfigSchema does not parse: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("schema type = %q, want object", schema.Type)
	}
	if p, ok := schema.Properties["feed_url"]; !ok || p.Type != "string" {
		t.Errorf("schema property feed_url = %+v, want string", p)
	}
}

func TestParseConfig(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		conf, err := parseConfig([]byte(`{"feed_url":"https://cal.example.com/x.ics"}`))
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if conf.FeedURL != "https://cal.example.com/x.ics" {
			t.Errorf("feed_url = %q", conf.FeedURL)
		}
	})

	t.Run("errors", func(t *testing.T) {
		t.Parallel()
		for name, raw := range map[string]string{
			"not json":   "{",
			"missing":    `{}`,
			"empty":      `{"feed_url":"  "}`,
			"not a url":  `{"feed_url":"::nope"}`,
			"ftp scheme": `{"feed_url":"ftp://host/x.ics"}`,
			"relative":   `{"feed_url":"/just/a/path.ics"}`,
		} {
			if _, err := parseConfig([]byte(raw)); err == nil {
				t.Errorf("parseConfig accepted %s config %q", name, raw)
			}
		}
	})
}

func TestValidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newTestConnector()

	t.Run("config error surfaces", func(t *testing.T) {
		t.Parallel()
		cfg := sdk.Config{ConfigJSON: []byte(`{}`), Checkpoint: sdk.NopCheckpoint}
		if err := c.Validate(ctx, cfg); err == nil {
			t.Fatal("Validate accepted a config without feed_url")
		}
	})

	t.Run("ok feed", func(t *testing.T) {
		t.Parallel()
		rs := loadServer(t, "testdata/validate.json")
		cfg := configFor(t, rs.URL())
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("non-calendar body", func(t *testing.T) {
		t.Parallel()
		rs := loadServer(t, "testdata/validate.json")
		cfg := configFor(t, rs.URL())
		// First call consumes the valid VCALENDAR interaction.
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("first Validate: %v", err)
		}
		// Second call hits the HTML interaction: not iCalendar data.
		if err := c.Validate(ctx, cfg); err == nil {
			t.Fatal("Validate accepted a non-calendar body")
		}
	})
}

// TestFullSync runs the backfill against the recorded feed and asserts the exact
// emitted doc_ids, types, mapped fields, and the checkpointed cursor. doc_ids are
// scoped by the stable instance id (testInstanceID), NOT the dynamic replay URL.
func TestFullSync(t *testing.T) {
	t.Parallel()
	rs := loadServer(t, "testdata/fullsync.json")
	c := newTestConnector()
	cfg := configFor(t, rs.URL())

	var rec connectortest.EmitRecorder
	cur, err := c.FullSync(context.Background(), cfg, rec.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	docs := rec.Docs()
	if len(docs) != 3 {
		t.Fatalf("emitted %d docs, want 3", len(docs))
	}
	for _, d := range docs {
		connectortest.ValidateDocument(t, cfg, d)
		if d.GetType() != docTypeCalendarEvent() {
			t.Errorf("doc %q type = %v, want CALENDAR_EVENT", d.GetDocId(), d.GetType())
		}
	}

	byID := indexByDocID(docs)

	// evt-standup: escaped DESCRIPTION, two attendees (CHAIR + REQ-PARTICIPANT),
	// organizer, timed event.
	standup := byID[wantDocID(testInstanceID, "evt-standup@example.com")]
	if standup == nil {
		t.Fatal("evt-standup not emitted")
	}
	if standup.GetTitle() != "Daily standup" {
		t.Errorf("standup title = %q", standup.GetTitle())
	}
	if want := "Quick sync.\n Bring blockers, notes; and questions.\n\nZoom"; standup.GetBodyText() != want {
		t.Errorf("standup body = %q, want %q", standup.GetBodyText(), want)
	}
	if standup.GetVersionEtag() != "seq:0" {
		t.Errorf("standup version_etag = %q, want seq:0", standup.GetVersionEtag())
	}
	if got := standup.GetMetadata()["uid"]; got != "evt-standup@example.com" {
		t.Errorf("standup metadata uid = %q", got)
	}
	if got := standup.GetMetadata()["url"]; got != "https://example.com/standup" {
		t.Errorf("standup metadata url = %q", got)
	}
	assertParticipant(t, standup, 0, "Alice Anderson", "alice@example.com", "organizer")
	assertParticipant(t, standup, 1, "Alice Anderson", "alice@example.com", "chair")
	assertParticipant(t, standup, 2, "Bob Brown", "bob@example.com", "req-participant")

	// evt-launch: SEQUENCE:2 version etag.
	launch := byID[wantDocID(testInstanceID, "evt-launch@example.com")]
	if launch == nil {
		t.Fatal("evt-launch not emitted")
	}
	if launch.GetVersionEtag() != "seq:2" {
		t.Errorf("launch version_etag = %q, want seq:2", launch.GetVersionEtag())
	}

	// evt-allhands: all-day (VALUE=DATE), RRULE recorded, no SEQUENCE -> etag from
	// LAST-MODIFIED absent too, so a sha256 fingerprint; metadata records rrule +
	// all_day.
	allhands := byID[wantDocID(testInstanceID, "evt-allhands@example.com")]
	if allhands == nil {
		t.Fatal("evt-allhands not emitted")
	}
	if got := allhands.GetMetadata()["all_day"]; got != "true" {
		t.Errorf("allhands all_day = %q, want true", got)
	}
	if got := allhands.GetMetadata()["rrule"]; got != "FREQ=MONTHLY;BYMONTHDAY=10" {
		t.Errorf("allhands rrule = %q", got)
	}
	if got := allhands.GetMetadata()["dtstart"]; got != "20260610" {
		t.Errorf("allhands dtstart = %q, want all-day date", got)
	}

	// Cursor round-trips and pins the three UIDs.
	st, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor(returned): %v", err)
	}
	if len(st.Events) != 3 {
		t.Errorf("cursor tracks %d events, want 3", len(st.Events))
	}
	if st.MaxDTStamp != "20260604T080000Z" {
		t.Errorf("cursor max_dtstamp = %q, want the largest DTSTAMP", st.MaxDTStamp)
	}
}

// TestDocIDStableAcrossFeedURLRotation is the regression test for the doc_id
// orphaning bug: many calendar providers embed a rotating private token in the
// feed URL, so a doc_id scoped by the raw feed URL changes on every rotation and
// orphans every previously indexed document. The doc_id must be scoped by the
// stable instance id instead, so the SAME VEVENT synced under TWO different feed
// URLs (same InstanceID) yields the SAME doc_id.
//
// Before the fix (nativeID == feedURL+":"+UID) the two passes produced different
// doc_ids and this test fails.
func TestDocIDStableAcrossFeedURLRotation(t *testing.T) {
	t.Parallel()

	const feedBody = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:evt-rotate@example.com\r\nSUMMARY:Weekly sync\r\nSEQUENCE:0\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	serve := func() *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(feedBody))
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	c := newTestConnector()

	// Two distinct feed URLs (e.g. before and after a token rotation), both for
	// the same configured instance (configFor stamps InstanceID = testInstanceID).
	srvA, srvB := serve(), serve()
	if srvA.URL == srvB.URL {
		t.Fatalf("test setup: the two feed servers share a URL %q", srvA.URL)
	}

	syncOne := func(rawURL string) string {
		t.Helper()
		var rec connectortest.EmitRecorder
		if _, err := c.FullSync(context.Background(), configFor(t, rawURL), rec.Emit); err != nil {
			t.Fatalf("FullSync(%s): %v", rawURL, err)
		}
		docs := rec.Docs()
		if len(docs) != 1 {
			t.Fatalf("emitted %d docs, want 1", len(docs))
		}
		return docs[0].GetDocId()
	}

	gotA := syncOne(srvA.URL)
	gotB := syncOne(srvB.URL)

	if gotA != gotB {
		t.Errorf("doc_id changed across a feed-URL rotation: %q (url A) != %q (url B); "+
			"the native id must be scoped by the stable instance id, not the feed URL", gotA, gotB)
	}
	// And it is exactly the instance-scoped id, never a URL-scoped one.
	if want := wantDocID(testInstanceID, "evt-rotate@example.com"); gotA != want {
		t.Errorf("doc_id = %q, want %q (sdk.DocID over instanceID+\":\"+UID)", gotA, want)
	}
}

// TestIncrementalChangeDelete proves IncrementalSync emits a changed event as an
// upsert, a new event as an upsert, a canceled-STATUS event as a tombstone, and
// a disappeared event as a tombstone.
func TestIncrementalChangeDelete(t *testing.T) {
	t.Parallel()
	rs := loadServer(t, "testdata/incremental.json")
	c := newTestConnector()
	cfg := configFor(t, rs.URL())

	// Baseline cursor: standup@seq:0, launch@seq:2, allhands present.
	baseline := cursorState{
		Hash: "stale-hash-forces-rescan",
		Events: map[string]string{
			"evt-standup@example.com":  "seq:0",
			"evt-launch@example.com":   "seq:2",
			"evt-allhands@example.com": "seq:0",
		},
	}

	var rec connectortest.EmitRecorder
	cur, err := c.IncrementalSync(context.Background(), cfg, baseline.encode(), rec.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	docs := rec.Docs()
	for _, d := range docs {
		connectortest.ValidateDocument(t, cfg, d)
	}

	live, tomb := splitIDs(docs)

	wantLive := map[string]bool{
		wantDocID(testInstanceID, "evt-standup@example.com"): true, // changed (seq 0 -> 1)
		wantDocID(testInstanceID, "evt-retro@example.com"):   true, // brand new
	}
	wantTomb := map[string]bool{
		wantDocID(testInstanceID, "evt-launch@example.com"):   true, // canceled STATUS
		wantDocID(testInstanceID, "evt-allhands@example.com"): true, // disappeared
	}
	assertIDSet(t, "live upserts", live, wantLive)
	assertIDSet(t, "tombstones", tomb, wantTomb)

	// The advanced cursor tracks the new feed's events (standup, launch, retro).
	st, err := parseCursor(cur)
	if err != nil {
		t.Fatalf("parseCursor: %v", err)
	}
	if _, ok := st.Events["evt-allhands@example.com"]; ok {
		t.Error("advanced cursor still tracks the disappeared event")
	}
	if _, ok := st.Events["evt-retro@example.com"]; !ok {
		t.Error("advanced cursor missing the new event")
	}
}

// TestIncrementalPartial proves that when the feed body changed but an
// individual event's version_etag still matches the baseline, that event is NOT
// re-emitted — only the changed event is.
func TestIncrementalPartial(t *testing.T) {
	t.Parallel()
	rs := loadServer(t, "testdata/incremental_partial.json")
	c := newTestConnector()
	cfg := configFor(t, rs.URL())

	baseline := cursorState{
		Hash: "old-hash-differs",
		Events: map[string]string{
			"evt-stable@example.com": "seq:0", // unchanged
			"evt-moved@example.com":  "seq:1", // changed -> now seq:4
		},
	}

	var rec connectortest.EmitRecorder
	if _, err := c.IncrementalSync(context.Background(), cfg, baseline.encode(), rec.Emit); err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	docs := rec.Docs()
	if len(docs) != 1 {
		t.Fatalf("emitted %d docs, want 1 (only the changed event)", len(docs))
	}
	if docs[0].GetDocId() != wantDocID(testInstanceID, "evt-moved@example.com") {
		t.Errorf("re-emitted the wrong event: %q", docs[0].GetDocId())
	}
}

// TestIncrementalUnchanged proves a byte-identical feed emits nothing and returns
// the same cursor.
func TestIncrementalUnchanged(t *testing.T) {
	t.Parallel()
	rs := loadServer(t, "testdata/unchanged.json")
	c := newTestConnector()
	cfg := configFor(t, rs.URL())

	// First fetch establishes the baseline cursor (FullSync).
	var first connectortest.EmitRecorder
	cur, err := c.FullSync(context.Background(), cfg, first.Emit)
	if err != nil {
		t.Fatalf("FullSync: %v", err)
	}

	// Second fetch (same body) must short-circuit: no emissions, same cursor.
	var second connectortest.EmitRecorder
	cur2, err := c.IncrementalSync(context.Background(), cfg, cur, second.Emit)
	if err != nil {
		t.Fatalf("IncrementalSync(unchanged): %v", err)
	}
	if n := len(second.Docs()); n != 0 {
		t.Errorf("unchanged feed emitted %d docs, want 0", n)
	}
	if cur2 != cur {
		t.Errorf("unchanged cursor = %q, want %q (same)", cur2, cur)
	}
}

// TestIncrementalStaleCursor proves a cursor this connector did not produce
// surfaces as sdk.ErrCursorExpired and makes no HTTP call.
func TestIncrementalStaleCursor(t *testing.T) {
	t.Parallel()
	// No replay server is needed: the cursor is rejected before any fetch.
	c := newTestConnector()
	cfg := configFor(t, "https://feed.invalid/x.ics")

	var rec connectortest.EmitRecorder
	_, err := c.IncrementalSync(context.Background(), cfg, sdk.Cursor("not-an-ical-cursor"), rec.Emit)
	if !errors.Is(err, sdk.ErrCursorExpired) {
		t.Errorf("IncrementalSync error = %v, want sdk.ErrCursorExpired", err)
	}
	if n := len(rec.Docs()); n != 0 {
		t.Errorf("emitted %d docs on stale cursor, want 0", n)
	}
}

func TestHandleWebhookUnsupported(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	var rec connectortest.EmitRecorder
	err := c.HandleWebhook(context.Background(), sdk.Config{
		ConfigJSON: []byte(`{"feed_url":"https://feed.invalid/x.ics"}`),
		Checkpoint: sdk.NopCheckpoint,
	}, nil, rec.Emit)
	if !errors.Is(err, sdk.ErrWebhookUnsupported) {
		t.Errorf("HandleWebhook error = %v, want sdk.ErrWebhookUnsupported", err)
	}
	if n := len(rec.Docs()); n != 0 {
		t.Errorf("HandleWebhook emitted %d docs, want 0", n)
	}
}

// TestContract drives the connector through the standard one-call harness for
// the FullSync pass, asserting count and per-doc validity. doc_ids depend on the
// (dynamic) replay URL, so the exact-id assertions live in TestFullSync; here we
// pin the count and let RunConnectorContract enforce ValidateDocument on every
// emitted document. feed_url is the BaseURLField the harness injects the replay
// URL into.
func TestContract(t *testing.T) {
	t.Parallel()
	connectortest.RunSpecChecks(t, newTestConnector())

	connectortest.RunConnectorContract(t, newTestConnector(), connectortest.ContractCase{
		Name:         "fullsync",
		Cassette:     "testdata/fullsync.json",
		BaseURLField: "feed_url",
		ConfigJSON:   json.RawMessage(`{}`),
		FullSync: &connectortest.SyncExpectation{
			WantCount: 3,
		},
	})
}
