package msteams

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func TestMessageTitleFallbacks(t *testing.T) {
	tests := []struct {
		name string
		body string
		chat graphChat
		want string
	}{
		{"first line truncated", strings.Repeat("a", 80), graphChat{}, strings.Repeat("a", 60) + "…"},
		{"first line of multiline", "hello\nworld", graphChat{}, "hello"},
		{"empty body uses topic", "", graphChat{Topic: "Standup"}, "Standup message"},
		{"empty body group label", "", graphChat{ChatType: "group"}, "Group chat message"},
		{"empty body oneOnOne label", "", graphChat{ChatType: "oneOnOne"}, "Chat message"},
		{"empty body meeting label", "", graphChat{ChatType: "meeting"}, "Meeting chat message"},
		{"empty body unknown label", "", graphChat{ChatType: "weird"}, "Chat message"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageTitle(tc.body, tc.chat); got != tc.want {
				t.Errorf("messageTitle(%q, %+v) = %q, want %q", tc.body, tc.chat, got, tc.want)
			}
		})
	}
}

func TestTruncateRunesMultibyte(t *testing.T) {
	// Ensure truncation does not split a multibyte rune.
	s := strings.Repeat("é", 100)
	got := truncateRunes(s, 10)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis, got %q", got)
	}
	// 10 'é' runes + ellipsis, all valid UTF-8.
	if n := len([]rune(strings.TrimSuffix(got, "…"))); n != 10 {
		t.Errorf("truncated to %d runes, want 10", n)
	}

	short := "hi"
	if got := truncateRunes(short, 10); got != short {
		t.Errorf("truncateRunes did not pass short string through: %q", got)
	}
}

func TestMessageEtagFallbacks(t *testing.T) {
	if got := messageEtag(graphMessage{ETag: "e1"}, "body"); got != "e1" {
		t.Errorf("etag preferred = %q, want e1", got)
	}
	if got := messageEtag(graphMessage{LastModifiedDateTime: "2024-01-01T00:00:00Z"}, "body"); got != "2024-01-01T00:00:00Z" {
		t.Errorf("lastModified fallback = %q", got)
	}
	// Content hash when the source supplies no version.
	h1 := messageEtag(graphMessage{}, "alpha")
	h2 := messageEtag(graphMessage{}, "beta")
	if h1 == h2 {
		t.Error("content-hash etag did not change with content")
	}
	if len(h1) != 64 {
		t.Errorf("content-hash etag length = %d, want 64 hex chars", len(h1))
	}
}

func TestParseGraphTime(t *testing.T) {
	if parseGraphTime("") != nil {
		t.Error("empty time should be nil")
	}
	if parseGraphTime("not-a-time") != nil {
		t.Error("unparseable time should be nil")
	}
	if ts := parseGraphTime("2024-01-02T03:04:05Z"); ts == nil {
		t.Error("RFC3339 time should parse")
	}
	if ts := parseGraphTime("2024-01-02T03:04:05.123456Z"); ts == nil {
		t.Error("RFC3339Nano time should parse")
	}
}

func TestTimestampsNilWhenAbsent(t *testing.T) {
	if ts := timestamps(graphMessage{}); ts != nil {
		t.Errorf("timestamps with no times = %+v, want nil", ts)
	}
}

func TestTombstoneDocumentFallbacks(t *testing.T) {
	c := &Connector{log: slog.Default(), now: func() time.Time { return time.Unix(1700000000, 0).UTC() }}

	// Prefer the source etag.
	d := c.tombstoneDocument("tenant-a", "19:c", graphMessage{ID: "9", ETag: "e9", DeletedDateTime: "2024-05-01T00:00:00Z"})
	if d.GetVersionEtag() != "e9" {
		t.Errorf("etag = %q, want e9", d.GetVersionEtag())
	}
	if !d.GetTombstone().GetDeleted() || d.GetTombstone().GetDeletedAt() == nil {
		t.Error("tombstone must be deleted with a deleted_at")
	}
	if d.GetBodyText() != "" || len(d.GetChunks()) != 0 {
		t.Error("tombstone must carry no body")
	}

	// Fall back to deletedDateTime when no etag.
	d = c.tombstoneDocument("tenant-a", "19:c", graphMessage{ID: "9", DeletedDateTime: "2024-05-01T00:00:00Z"})
	if d.GetVersionEtag() != "2024-05-01T00:00:00Z" {
		t.Errorf("etag fallback = %q, want deletedDateTime", d.GetVersionEtag())
	}

	// Fall back to now() when nothing is set.
	d = c.tombstoneDocument("tenant-a", "19:c", graphMessage{ID: "9"})
	if d.GetVersionEtag() == "" {
		t.Error("etag must never be empty (now() fallback)")
	}
	if d.GetType() != askerv1.DocType_CHAT_MESSAGE {
		t.Errorf("tombstone type = %v, want CHAT_MESSAGE", d.GetType())
	}
}

func TestChatACL(t *testing.T) {
	// oneOnOne: no ACL.
	if acl := chatACL(graphChat{ChatType: "oneOnOne", Members: []graphConversationMember{{UserID: "u1"}}}); acl != nil {
		t.Error("oneOnOne chat should have no ACL")
	}
	// group with members: ACL with principals.
	acl := chatACL(graphChat{ChatType: "group", Members: []graphConversationMember{{UserID: "u1"}, {UserID: "u2"}}})
	if acl == nil || !acl.GetIsPrivate() || len(acl.GetAllowedPrincipals()) != 2 {
		t.Errorf("group ACL = %+v, want 2 private principals", acl)
	}
	// group with no expanded members: no ACL.
	if acl := chatACL(graphChat{ChatType: "group"}); acl != nil {
		t.Error("group chat with no members should have no ACL")
	}
	// group whose members have no user ids: no ACL.
	if acl := chatACL(graphChat{ChatType: "group", Members: []graphConversationMember{{DisplayName: "x"}}}); acl != nil {
		t.Error("group chat with member-but-no-userid should have no ACL")
	}
}

func TestParticipantsSkipEmptyMembers(t *testing.T) {
	chat := graphChat{Members: []graphConversationMember{
		{},                                  // fully empty -> skipped
		{DisplayName: "Bob", UserID: "u-b"}, // kept
	}}
	msg := graphMessage{From: &graphIdentitySet{User: &graphIdentity{ID: "u-a", DisplayName: "Alice"}}}
	ps := participants(chat, msg)
	if len(ps) != 2 {
		t.Fatalf("participants = %d, want 2 (from + one member)", len(ps))
	}
	if ps[0].GetRole() != "from" || ps[1].GetRole() != "member" {
		t.Errorf("roles = %q,%q; want from,member", ps[0].GetRole(), ps[1].GetRole())
	}
}

func TestMessageBodyTextPassthrough(t *testing.T) {
	if got := messageBody(graphItemBody{ContentType: "text", Content: "  hi  "}); got != "hi" {
		t.Errorf("text body = %q, want trimmed 'hi'", got)
	}
	if got := messageBody(graphItemBody{ContentType: "html", Content: "<p>hi</p>"}); got != "hi" {
		t.Errorf("html body = %q, want stripped 'hi'", got)
	}
}

func TestWithLoggerOption(t *testing.T) {
	custom := slog.New(slog.NewTextHandler(nil, nil))
	c := New(WithLogger(custom)).(*Connector)
	if c.log != custom {
		t.Error("WithLogger did not set the logger")
	}
	// nil logger is ignored (keeps the default).
	def := New(WithLogger(nil)).(*Connector)
	if def.log == nil {
		t.Error("WithLogger(nil) cleared the default logger")
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("https://h/path?secret=1"); got != "https://h/path" {
		t.Errorf("redactURL = %q, want query stripped", got)
	}
	if got := redactURL("https://h/path"); got != "https://h/path" {
		t.Errorf("redactURL changed a query-less URL: %q", got)
	}
}

func TestResolveRebasesODataLink(t *testing.T) {
	g := &graphClient{base: "http://127.0.0.1:8080"}
	// Absolute v1.0 link: version prefix stripped, path+query rebased.
	got, err := g.resolve("https://graph.microsoft.com/v1.0/chats/19:c/messages/delta?$deltatoken=X")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:8080/chats/19:c/messages/delta?$deltatoken=X" {
		t.Errorf("resolve(absolute) = %q", got)
	}
	// Relative ref: appended to base.
	got, err = g.resolve("/me/chats")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:8080/me/chats" {
		t.Errorf("resolve(relative) = %q", got)
	}
	// Ref without a leading slash still resolves.
	if got, _ = g.resolve("me"); got != "http://127.0.0.1:8080/me" {
		t.Errorf("resolve(no-slash) = %q", got)
	}
}

func TestBaseURLDefault(t *testing.T) {
	if got := (instanceConfig{}).baseURL(); got != defaultGraphBaseURL {
		t.Errorf("default baseURL = %q, want %q", got, defaultGraphBaseURL)
	}
	if got := (instanceConfig{BaseURL: "http://x/"}).baseURL(); got != "http://x" {
		t.Errorf("override baseURL = %q, want trailing slash trimmed", got)
	}
}

func TestParseConfigErrors(t *testing.T) {
	if _, err := parseConfig([]byte("not json")); err == nil {
		t.Error("parseConfig accepted invalid JSON")
	}
	if _, err := parseConfig([]byte(`{"base_url":"://bad"}`)); err == nil {
		t.Error("parseConfig accepted a malformed base_url")
	}
	if _, err := parseConfig(nil); err != nil {
		t.Errorf("parseConfig(nil) should be valid (no config): %v", err)
	}
}
