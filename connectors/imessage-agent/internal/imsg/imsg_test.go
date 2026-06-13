package imsg

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

const testTenant = "tenant-a"

// sampleRow is a fully-populated inbound iMessage used as a base for tests that
// vary one field at a time.
func sampleRow() Row {
	return Row{
		RowID:    42,
		ChatGUID: "iMessage;-;+15551234567",
		ChatName: "",
		Handle:   "+15551234567",
		Text:     "hey, are we still on for lunch?",
		IsFromMe: false,
		Service:  "iMessage",
		// 2021-06-01T12:00:00Z in nanoseconds since 2001-01-01.
		Date: int64(time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC).Sub(appleEpoch)),
	}
}

func TestAppleTimeToTime(t *testing.T) {
	tests := []struct {
		name string
		ns   int64
		want time.Time
	}{
		{"epoch-plus-zero-is-zero-time", 0, time.Time{}},
		{"negative-is-zero-time", -1, time.Time{}},
		{
			name: "one-second-after-epoch",
			ns:   int64(time.Second),
			want: time.Date(2001, 1, 1, 0, 0, 1, 0, time.UTC),
		},
		{
			name: "known-2021-instant",
			ns:   int64(time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC).Sub(appleEpoch)),
			want: time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC),
		},
		{
			name: "nanosecond-precision-preserved",
			ns:   int64(time.Second) + 123456789,
			want: time.Date(2001, 1, 1, 0, 0, 1, 123456789, time.UTC),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AppleTimeToTime(tc.ns)
			if !got.Equal(tc.want) {
				t.Errorf("AppleTimeToTime(%d) = %v, want %v", tc.ns, got, tc.want)
			}
			if !got.IsZero() && got.Location() != time.UTC {
				t.Errorf("AppleTimeToTime(%d) location = %v, want UTC", tc.ns, got.Location())
			}
		})
	}
}

func TestNormalizeDate(t *testing.T) {
	tests := []struct {
		name string
		date int64
		want int64
	}{
		{"zero-passes-through", 0, 0},
		{"negative-passes-through", -5, -5},
		{
			name: "legacy-seconds-scaled-to-nanoseconds",
			date: 1000,
			want: 1000 * int64(time.Second),
		},
		{
			name: "realistic-2010-seconds-scaled",
			date: int64(time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC).Sub(appleEpoch).Seconds()),
			want: int64(time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC).Sub(appleEpoch).Seconds()) * int64(time.Second),
		},
		{
			name: "modern-nanoseconds-pass-through",
			date: secondsThreshold + 1,
			want: secondsThreshold + 1,
		},
		{
			name: "real-nanosecond-value-unchanged",
			date: int64(time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC).Sub(appleEpoch)),
			want: int64(time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC).Sub(appleEpoch)),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeDate(tc.date); got != tc.want {
				t.Errorf("NormalizeDate(%d) = %d, want %d", tc.date, got, tc.want)
			}
		})
	}
}

func TestNormalizeDateThenConvertAgree(t *testing.T) {
	// A legacy seconds value and the equivalent nanosecond value must convert
	// to the same instant once normalized.
	want := time.Date(2010, 3, 4, 5, 6, 7, 0, time.UTC)
	seconds := int64(want.Sub(appleEpoch).Seconds())
	nanos := int64(want.Sub(appleEpoch))

	gotSeconds := AppleTimeToTime(NormalizeDate(seconds))
	gotNanos := AppleTimeToTime(NormalizeDate(nanos))
	if !gotSeconds.Equal(want) {
		t.Errorf("seconds path = %v, want %v", gotSeconds, want)
	}
	if !gotNanos.Equal(want) {
		t.Errorf("nanos path = %v, want %v", gotNanos, want)
	}
}

func TestRowToDocumentInbound(t *testing.T) {
	r := sampleRow()
	doc := RowToDocument(testTenant, r)

	wantNative := "iMessage;-;+15551234567:42"
	if doc.GetSourceNativeId() != wantNative {
		t.Errorf("source_native_id = %q, want %q", doc.GetSourceNativeId(), wantNative)
	}
	if want := sdk.DocID(ConnectorID, wantNative); doc.GetDocId() != want {
		t.Errorf("doc_id = %q, want %q", doc.GetDocId(), want)
	}
	if doc.GetConnectorId() != ConnectorID {
		t.Errorf("connector_id = %q, want %q", doc.GetConnectorId(), ConnectorID)
	}
	if doc.GetTenantId() != testTenant {
		t.Errorf("tenant_id = %q, want %q", doc.GetTenantId(), testTenant)
	}
	if doc.GetType() != askerv1.DocType_CHAT_MESSAGE {
		t.Errorf("type = %v, want CHAT_MESSAGE", doc.GetType())
	}
	if doc.GetBodyText() != r.Text {
		t.Errorf("body_text = %q, want %q", doc.GetBodyText(), r.Text)
	}
	if got := doc.GetTitle(); got != "iMessage;-;+15551234567: hey, are we still on for lunch?" {
		t.Errorf("title = %q", got)
	}
	if doc.GetVersionEtag() == "" {
		t.Error("version_etag is empty")
	}
	if doc.GetTs().GetCreated() == nil {
		t.Error("ts.created is nil, want the converted Apple date")
	}
	if doc.GetTs().GetIngested() != nil {
		t.Error("ts.ingested must be left unset for the hub")
	}
	if doc.GetAcl() != nil {
		t.Error("acl must be unset for a private personal chat (ADR-012)")
	}

	// Inbound: sender is the handle (role from), me is the recipient (role to).
	wantParts := []*askerv1.Participant{
		{Handle: "+15551234567", Role: "from"},
		{Name: meHandle, Handle: meHandle, Role: "to"},
	}
	assertParticipants(t, doc.GetParticipants(), wantParts)

	md := doc.GetMetadata()
	if md["handle"] != "+15551234567" {
		t.Errorf("metadata handle = %q", md["handle"])
	}
	if md["service"] != "iMessage" {
		t.Errorf("metadata service = %q", md["service"])
	}
	if md["is_from_me"] != "false" {
		t.Errorf("metadata is_from_me = %q, want false", md["is_from_me"])
	}
	if md["rowid"] != "42" {
		t.Errorf("metadata rowid = %q, want 42", md["rowid"])
	}
	if md["chat_guid"] != r.ChatGUID {
		t.Errorf("metadata chat_guid = %q", md["chat_guid"])
	}
	if _, ok := md["chat"]; ok {
		t.Error("metadata chat present for empty display name; want omitted")
	}
}

func TestRowToDocumentOutbound(t *testing.T) {
	r := sampleRow()
	r.IsFromMe = true
	r.Text = "yes! see you at noon"
	doc := RowToDocument(testTenant, r)

	if doc.GetMetadata()["is_from_me"] != "true" {
		t.Errorf("metadata is_from_me = %q, want true", doc.GetMetadata()["is_from_me"])
	}
	// Outbound: me is the sender (from), the chat handle is the recipient (to).
	wantParts := []*askerv1.Participant{
		{Name: meHandle, Handle: meHandle, Role: "from"},
		{Handle: "+15551234567", Role: "to"},
	}
	assertParticipants(t, doc.GetParticipants(), wantParts)
}

func TestRowToDocumentEmailHandle(t *testing.T) {
	r := sampleRow()
	r.Handle = "friend@example.com"
	doc := RowToDocument(testTenant, r)

	parts := doc.GetParticipants()
	if len(parts) == 0 {
		t.Fatal("no participants")
	}
	from := parts[0]
	if from.GetEmail() != "friend@example.com" {
		t.Errorf("email handle mapped to email = %q, want friend@example.com", from.GetEmail())
	}
	if from.GetHandle() != "" {
		t.Errorf("email handle should not populate Handle, got %q", from.GetHandle())
	}
}

func TestRowToDocumentGroupChatName(t *testing.T) {
	r := sampleRow()
	r.ChatName = "Lunch Crew"
	doc := RowToDocument(testTenant, r)

	if got := doc.GetTitle(); !strings.HasPrefix(got, "Lunch Crew: ") {
		t.Errorf("title = %q, want it to lead with the chat display name", got)
	}
	if doc.GetMetadata()["chat"] != "Lunch Crew" {
		t.Errorf("metadata chat = %q, want Lunch Crew", doc.GetMetadata()["chat"])
	}
}

func TestRowToDocumentEmptyText(t *testing.T) {
	r := sampleRow()
	r.Text = ""
	doc := RowToDocument(testTenant, r)

	if doc.GetBodyText() != "" {
		t.Errorf("body_text = %q, want empty", doc.GetBodyText())
	}
	// Title degrades to just the chat label when there is no text.
	if got := doc.GetTitle(); got != r.ChatGUID {
		t.Errorf("title = %q, want bare chat label %q", got, r.ChatGUID)
	}
	if doc.GetVersionEtag() == "" {
		t.Error("version_etag empty for empty text; must still be set")
	}
}

func TestRowToDocumentUnknownChat(t *testing.T) {
	r := sampleRow()
	r.ChatGUID = ""
	r.ChatName = ""
	r.Text = "orphan message"
	doc := RowToDocument(testTenant, r)

	if got := doc.GetTitle(); !strings.HasPrefix(got, "(unknown chat): ") {
		t.Errorf("title = %q, want it to lead with (unknown chat)", got)
	}
	// native id is ":42" when the guid is empty — source_native_id still set.
	if doc.GetSourceNativeId() != ":42" {
		t.Errorf("source_native_id = %q, want \":42\"", doc.GetSourceNativeId())
	}
}

func TestVersionEtagChangesWithContent(t *testing.T) {
	base := sampleRow()
	baseEtag := RowToDocument(testTenant, base).GetVersionEtag()

	edited := base
	edited.Text = base.Text + " (edited)"
	if got := RowToDocument(testTenant, edited).GetVersionEtag(); got == baseEtag {
		t.Error("version_etag did not change when the text changed")
	}

	resent := base
	resent.Date = base.Date + int64(time.Minute)
	if got := RowToDocument(testTenant, resent).GetVersionEtag(); got == baseEtag {
		t.Error("version_etag did not change when the date changed")
	}

	// Identical row reproduces the same etag (idempotent upsert).
	if got := RowToDocument(testTenant, base).GetVersionEtag(); got != baseEtag {
		t.Error("version_etag not stable for an unchanged row")
	}
}

func TestVersionEtagFromMeDoesNotAffectEtag(t *testing.T) {
	// is_from_me is metadata, not content the etag hashes. A flip of the flag
	// alone must not change the etag (text+date define content).
	a := sampleRow()
	b := a
	b.IsFromMe = !a.IsFromMe
	if RowToDocument(testTenant, a).GetVersionEtag() != RowToDocument(testTenant, b).GetVersionEtag() {
		t.Error("version_etag changed on is_from_me flip; only text+date define content")
	}
}

func TestRenderTranscript(t *testing.T) {
	day := func(h int) int64 {
		return int64(time.Date(2021, 6, 1, h, 0, 0, 0, time.UTC).Sub(appleEpoch))
	}
	rows := []Row{
		{RowID: 1, ChatGUID: "g1", ChatName: "Lunch Crew", Handle: "+15551234567", Text: "lunch?", Service: "iMessage", Date: day(12)},
		{RowID: 2, ChatGUID: "g1", ChatName: "Lunch Crew", IsFromMe: true, Text: "yes!", Service: "iMessage", Date: day(13)},
		{RowID: 3, ChatGUID: "g1", ChatName: "Lunch Crew", Handle: "+15551234567", Text: "", Service: "iMessage", Date: day(14)},
	}
	got := RenderTranscript(rows)

	want := strings.Join([]string{
		"Chat: Lunch Crew",
		"Service: iMessage",
		"",
		"[2021-06-01 12:00] +15551234567: lunch?",
		"[2021-06-01 13:00] me: yes!",
		"[2021-06-01 14:00] +15551234567: (no text)",
		"",
	}, "\n")
	if got != want {
		t.Errorf("transcript mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestRenderTranscriptCollapsesNewlines(t *testing.T) {
	rows := []Row{
		{RowID: 1, ChatGUID: "g1", Handle: "h", Text: "line one\nline two", Date: int64(time.Second)},
	}
	got := RenderTranscript(rows)
	if strings.Count(got, "line one\nline two") != 0 {
		t.Error("transcript kept an embedded newline inside a message line")
	}
	if !strings.Contains(got, "line one line two") {
		t.Errorf("transcript did not collapse the embedded newline: %q", got)
	}
}

func TestRenderTranscriptNoTimestamp(t *testing.T) {
	rows := []Row{
		{RowID: 1, ChatGUID: "g1", Handle: "h", Text: "no time here", Date: 0},
	}
	got := RenderTranscript(rows)
	if strings.Contains(got, "[") {
		t.Errorf("transcript rendered a bracketed time for a zero date: %q", got)
	}
	if !strings.Contains(got, "h: no time here") {
		t.Errorf("transcript missing the message line: %q", got)
	}
}

func TestRenderTranscriptEmpty(t *testing.T) {
	if got := RenderTranscript(nil); got != "" {
		t.Errorf("RenderTranscript(nil) = %q, want empty", got)
	}
}

func TestRenderTranscriptUnknownSender(t *testing.T) {
	// Inbound message with no handle: sender renders as (unknown).
	rows := []Row{
		{RowID: 1, ChatGUID: "g1", Handle: "", Text: "who am I", Date: int64(time.Second)},
	}
	got := RenderTranscript(rows)
	if !strings.Contains(got, "(unknown): who am I") {
		t.Errorf("transcript missing (unknown) sender line: %q", got)
	}
}

func TestTranscriptFilename(t *testing.T) {
	tests := []struct {
		guid string
		want string
	}{
		{"iMessage;-;+15551234567", "imessage-iMessage_-__15551234567.txt"},
		{"", "imessage-chat.txt"},
		{"chat.with.dots", "imessage-chat.with.dots.txt"},
		{"///", "imessage-chat.txt"},
	}
	for _, tc := range tests {
		if got := TranscriptFilename(tc.guid); got != tc.want {
			t.Errorf("TranscriptFilename(%q) = %q, want %q", tc.guid, got, tc.want)
		}
	}
}

func TestSnippetTruncation(t *testing.T) {
	long := strings.Repeat("a", 200)
	r := sampleRow()
	r.Text = long
	title := RowToDocument(testTenant, r).GetTitle()
	if !strings.HasSuffix(title, "…") {
		t.Errorf("long title not truncated with ellipsis: %q", title)
	}
	// Title = label + ": " + 80 runes + "…".
	if n := len([]rune(title)); n > len([]rune(r.ChatGUID))+len(": ")+maxSnippet+1 {
		t.Errorf("title too long: %d runes", n)
	}
}

// assertParticipants compares participant slices by the fields imsg sets,
// order-sensitively (the mapping emits from-then-to in a fixed order).
func assertParticipants(t *testing.T, got, want []*askerv1.Participant) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("participants len = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].GetName() != want[i].GetName() ||
			got[i].GetEmail() != want[i].GetEmail() ||
			got[i].GetHandle() != want[i].GetHandle() ||
			got[i].GetRole() != want[i].GetRole() {
			t.Errorf("participant[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestNativeIDRoundTrips guards the doc_id derivation against accidental
// formatting changes: the native id is exactly chatGUID + ":" + rowid.
func TestNativeIDRoundTrips(t *testing.T) {
	r := sampleRow()
	r.RowID = 9001
	r.ChatGUID = "guid-x"
	doc := RowToDocument(testTenant, r)
	want := "guid-x:" + strconv.FormatInt(9001, 10)
	if doc.GetSourceNativeId() != want {
		t.Errorf("source_native_id = %q, want %q", doc.GetSourceNativeId(), want)
	}
}
