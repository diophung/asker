package slack

import (
	"strings"
	"testing"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func TestMessageDocumentMapping(t *testing.T) {
	t.Parallel()
	ch := channelInfo{ID: "C100", Name: "general", IsPrivate: false, Members: []string{"U_ALICE", "U_BOB"}}
	m := message{
		Type:     "message",
		User:     "U_ALICE",
		Text:     "morning team, standup at 10",
		TS:       "1700000300.000300",
		ThreadTS: "1700000100.000100",
		Team:     "T1",
	}

	doc := messageDocument("tenant-a", ch, m)

	if doc.GetType() != askerv1.DocType_CHAT_MESSAGE {
		t.Errorf("type = %v, want CHAT_MESSAGE", doc.GetType())
	}
	if doc.GetTenantId() != "tenant-a" {
		t.Errorf("tenant_id = %q", doc.GetTenantId())
	}
	if doc.GetSourceNativeId() != "C100:1700000300.000300" {
		t.Errorf("source_native_id = %q", doc.GetSourceNativeId())
	}
	if doc.GetDocId() != docID("C100", "1700000300.000300") {
		t.Errorf("doc_id = %q", doc.GetDocId())
	}
	if doc.GetTitle() != "morning team, standup at 10" {
		t.Errorf("title = %q", doc.GetTitle())
	}
	if doc.GetBodyText() != "morning team, standup at 10" {
		t.Errorf("body = %q", doc.GetBodyText())
	}
	if doc.GetVersionEtag() != "1700000300.000300" {
		t.Errorf("version_etag = %q, want the ts", doc.GetVersionEtag())
	}
	if doc.GetTs().GetCreated() == nil {
		t.Error("ts.created unset")
	}
	if doc.GetTs().GetIngested() != nil {
		t.Error("ts.ingested must be left unset for the hub")
	}

	// Participant: author handle, role from.
	if ps := doc.GetParticipants(); len(ps) != 1 || ps[0].GetHandle() != "U_ALICE" || ps[0].GetRole() != "from" {
		t.Errorf("participants = %+v", ps)
	}

	// Metadata facets.
	md := doc.GetMetadata()
	for k, want := range map[string]string{
		"channel":      "C100",
		"channel_name": "general",
		"ts":           "1700000300.000300",
		"thread_ts":    "1700000100.000100",
		"team":         "T1",
		"user":         "U_ALICE",
	} {
		if md[k] != want {
			t.Errorf("metadata[%q] = %q, want %q", k, md[k], want)
		}
	}
	if !strings.Contains(md["permalink"], "C100") {
		t.Errorf("permalink = %q, want one containing the channel", md["permalink"])
	}

	// Public channel with members -> ACL captured (not private), per ADR-012.
	acl := doc.GetAcl()
	if acl == nil || acl.GetIsPrivate() {
		t.Errorf("acl = %+v, want a non-private AclInfo", acl)
	}
	if got := acl.GetAllowedPrincipals(); len(got) != 2 {
		t.Errorf("allowed_principals = %v, want 2 members", got)
	}
}

func TestEditedMessageEtagChanges(t *testing.T) {
	t.Parallel()
	ch := channelInfo{ID: "C100", Name: "general"}
	base := message{Type: "message", User: "U_ALICE", Text: "v1", TS: "1700000300.000300"}
	edited := base
	edited.Text = "v2"
	edited.Edited = &struct {
		User string `json:"user"`
		TS   string `json:"ts"`
	}{User: "U_ALICE", TS: "1700000500.000500"}

	d1 := messageDocument("t", ch, base)
	d2 := messageDocument("t", ch, edited)

	if d1.GetDocId() != d2.GetDocId() {
		t.Error("an edit must keep the same doc_id (upsert)")
	}
	if d1.GetVersionEtag() == d2.GetVersionEtag() {
		t.Error("an edit must change version_etag")
	}
	if d2.GetVersionEtag() != "1700000500.000500" {
		t.Errorf("edited etag = %q, want edited.ts", d2.GetVersionEtag())
	}
	if d2.GetTs().GetModified() == nil {
		t.Error("edited message must set ts.modified")
	}
}

func TestPrivateChannelACL(t *testing.T) {
	t.Parallel()
	ch := channelInfo{ID: "C200", Name: "secrets", IsPrivate: true, Members: []string{"U_ALICE"}}
	doc := messageDocument("t", ch, message{Type: "message", User: "U_ALICE", Text: "shh", TS: "1.0"})
	acl := doc.GetAcl()
	if acl == nil || !acl.GetIsPrivate() {
		t.Errorf("private channel acl = %+v, want is_private", acl)
	}
}

func TestDirectMessageNoACL(t *testing.T) {
	t.Parallel()
	ch := channelInfo{ID: "D900", IsIM: true, Members: []string{"U_ALICE", "U_BOB"}}
	doc := messageDocument("t", ch, message{Type: "message", User: "U_BOB", Text: "hi", TS: "1.0"})
	if doc.GetAcl() != nil {
		t.Errorf("1:1 DM must leave acl unset (private by construction), got %+v", doc.GetAcl())
	}
}

func TestTitleFallbackAndTruncation(t *testing.T) {
	t.Parallel()
	ch := channelInfo{ID: "C100", Name: "general"}

	// Empty text -> "<channel> message".
	d := messageDocument("t", ch, message{Type: "message", User: "U", TS: "1.0"})
	if d.GetTitle() != "#general message" {
		t.Errorf("empty-text title = %q, want %q", d.GetTitle(), "#general message")
	}

	// Long, multi-line text -> single-line, <= 80 runes.
	long := "line one\n" + strings.Repeat("x", 200)
	d = messageDocument("t", ch, message{Type: "message", User: "U", Text: long, TS: "1.0"})
	if title := d.GetTitle(); len([]rune(title)) > titleMaxRunes {
		t.Errorf("title length = %d runes, want <= %d", len([]rune(title)), titleMaxRunes)
	}
	if strings.Contains(d.GetTitle(), "\n") {
		t.Error("title must be a single line")
	}
}

func TestParseSlackTS(t *testing.T) {
	t.Parallel()
	if parseSlackTS("") != nil {
		t.Error("empty ts must be nil")
	}
	if parseSlackTS("not-a-ts") != nil {
		t.Error("garbage ts must be nil")
	}
	got := parseSlackTS("1700000300.000300")
	if got == nil {
		t.Fatal("valid ts decoded to nil")
	}
	if got.GetSeconds() != 1700000300 {
		t.Errorf("seconds = %d, want 1700000300", got.GetSeconds())
	}
	// 000300 micros == 300_000 nanos.
	if got.GetNanos() != 300000 {
		t.Errorf("nanos = %d, want 300000", got.GetNanos())
	}
}

func TestTombstoneHasNoBody(t *testing.T) {
	t.Parallel()
	c := newTestConnector()
	tomb := c.tombstoneDocument("tenant-a", "C100", "1700000200.000200", "1700000600.000600")
	if !tomb.GetTombstone().GetDeleted() {
		t.Error("tombstone.deleted must be true")
	}
	if tomb.GetBodyText() != "" || len(tomb.GetChunks()) != 0 {
		t.Error("tombstone must carry no body")
	}
	if tomb.GetDocId() != docID("C100", "1700000200.000200") {
		t.Error("tombstone doc_id must match the message doc_id")
	}
	if tomb.GetVersionEtag() != "1700000600.000600" {
		t.Errorf("tombstone etag = %q, want the event ts", tomb.GetVersionEtag())
	}
	if tomb.GetTombstone().GetDeletedAt() == nil {
		t.Error("tombstone must set deleted_at")
	}
}
