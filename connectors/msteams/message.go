package msteams

import (
	"crypto/sha256"
	"encoding/hex"
	"html"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// titleMaxRunes bounds the synthesized title to roughly the first line of the
// message body.
const titleMaxRunes = 60

// graphIdentitySet is Graph's {user,application,device} identity wrapper used
// by chatMessage.from and elsewhere. Only the user facet is mapped.
type graphIdentitySet struct {
	User *graphIdentity `json:"user"`
}

// graphIdentity is one identity (a user) in a Graph identitySet.
type graphIdentity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// graphConversationMember is a member of a chat (GET /me/chats?$expand=members
// members[]). The displayName/userId fields carry the person.
type graphConversationMember struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	UserID      string `json:"userId"`
	Email       string `json:"email"`
}

// graphChat is one entry of GET /me/chats?$expand=members.
type graphChat struct {
	ID       string                    `json:"id"`
	Topic    string                    `json:"topic"`
	ChatType string                    `json:"chatType"` // oneOnOne | group | meeting
	WebURL   string                    `json:"webUrl"`
	Members  []graphConversationMember `json:"members"`
}

// graphItemBody is a chatMessage body (content + contentType html|text).
type graphItemBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

// graphMessage is one chatMessage from GET /chats/{id}/messages (or the delta
// endpoint). A delta-removed message carries either deletedDateTime or an
// "@removed" annotation.
type graphMessage struct {
	ID                   string            `json:"id"`
	ETag                 string            `json:"etag"`
	MessageType          string            `json:"messageType"` // message | systemEventMessage | ...
	CreatedDateTime      string            `json:"createdDateTime"`
	LastModifiedDateTime string            `json:"lastModifiedDateTime"`
	DeletedDateTime      string            `json:"deletedDateTime"`
	Importance           string            `json:"importance"`
	WebURL               string            `json:"webUrl"`
	From                 *graphIdentitySet `json:"from"`
	Body                 graphItemBody     `json:"body"`
	Removed              *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
}

// removed reports whether this message represents a deletion in a delta feed.
func (m graphMessage) removed() bool {
	return m.Removed != nil || strings.TrimSpace(m.DeletedDateTime) != ""
}

// nativeID is the stable source-native id for a message: "<chatId>:<messageId>"
// (the build contract's doc_id key). It is unique across chats because a Teams
// message id is only unique within its chat.
func nativeID(chatID, messageID string) string {
	return chatID + ":" + messageID
}

// messageDocument maps one chatMessage in a chat to the canonical Document:
// type CHAT_MESSAGE, doc_id sdk.DocID("msteams", chatId+":"+messageId), title
// from the first ~60 chars of the body text, body from body.content (HTML
// stripped when contentType is html), participants from from.user plus the
// chat members, metadata with chat/message identifiers, and ts from
// created/lastModified. The version_etag is the message etag, falling back to
// lastModifiedDateTime, then to a content hash.
func messageDocument(tenant string, chat graphChat, msg graphMessage) *askerv1.Document {
	body := messageBody(msg.Body)
	doc := &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, nativeID(chat.ID, msg.ID)),
		ConnectorId:    connectorID,
		SourceNativeId: nativeID(chat.ID, msg.ID),
		Type:           askerv1.DocType_CHAT_MESSAGE,
		Title:          messageTitle(body, chat),
		BodyText:       body,
		Participants:   participants(chat, msg),
		Metadata:       metadata(chat, msg),
		VersionEtag:    messageEtag(msg, body),
	}
	doc.Ts = timestamps(msg)
	if acl := chatACL(chat); acl != nil {
		doc.Acl = acl
	}
	return doc
}

// tombstoneDocument builds the deletion Document for one message: identity
// fields plus tombstone.deleted/deleted_at and no body. The version_etag is
// the message etag (falling back to the deletion timestamp), which is newer
// than any prior upsert for the same doc_id.
func (c *Connector) tombstoneDocument(tenant, chatID string, msg graphMessage) *askerv1.Document {
	etag := strings.TrimSpace(msg.ETag)
	if etag == "" {
		etag = strings.TrimSpace(msg.DeletedDateTime)
	}
	if etag == "" {
		// No source version at all: stamp deletion time so the tombstone still
		// wins the idempotent merge.
		etag = c.now().UTC().Format(time.RFC3339Nano)
	}
	deletedAt := parseGraphTime(msg.DeletedDateTime)
	if deletedAt == nil {
		deletedAt = timestamppb.New(c.now().UTC())
	}
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, nativeID(chatID, msg.ID)),
		ConnectorId:    connectorID,
		SourceNativeId: nativeID(chatID, msg.ID),
		Type:           askerv1.DocType_CHAT_MESSAGE,
		VersionEtag:    etag,
		Tombstone: &askerv1.Tombstone{
			Deleted:   true,
			DeletedAt: deletedAt,
		},
	}
}

// messageBody returns plain searchable text from a chatMessage body: HTML
// content is crudely stripped, text content is passed through trimmed.
func messageBody(b graphItemBody) string {
	content := b.Content
	if strings.EqualFold(b.ContentType, "html") {
		content = stripHTML(content)
	}
	return strings.TrimSpace(content)
}

// messageTitle synthesizes a title: the first ~60 characters (first line) of
// the body text, or "<topic> message" / "<chatType> message" when the body is
// empty (e.g. a system message or an attachment-only post).
func messageTitle(body string, chat graphChat) string {
	first := body
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(first)
	if first != "" {
		return truncateRunes(first, titleMaxRunes)
	}
	label := strings.TrimSpace(chat.Topic)
	if label == "" {
		label = chatLabel(chat)
	}
	return label + " message"
}

// chatLabel is a human label for a chat without a topic.
func chatLabel(chat graphChat) string {
	switch strings.ToLower(chat.ChatType) {
	case "oneonone":
		return "Chat"
	case "group":
		return "Group chat"
	case "meeting":
		return "Meeting chat"
	default:
		return "Chat"
	}
}

// truncateRunes returns the first n runes of s, appending an ellipsis when it
// truncates, so the title stays valid UTF-8.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// messageEtag returns a version_etag that changes when content changes: the
// Graph etag, else lastModifiedDateTime, else a content hash.
func messageEtag(msg graphMessage, body string) string {
	if e := strings.TrimSpace(msg.ETag); e != "" {
		return e
	}
	if lm := strings.TrimSpace(msg.LastModifiedDateTime); lm != "" {
		return lm
	}
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// timestamps maps created/lastModified to Document.ts. ts.ingested is left
// unset — the hub stamps it.
func timestamps(msg graphMessage) *askerv1.Timestamps {
	created := parseGraphTime(msg.CreatedDateTime)
	modified := parseGraphTime(msg.LastModifiedDateTime)
	if created == nil && modified == nil {
		return nil
	}
	return &askerv1.Timestamps{Created: created, Modified: modified}
}

// parseGraphTime parses a Graph ISO-8601 timestamp (RFC 3339) to a protobuf
// Timestamp, returning nil for an empty or unparseable value.
func parseGraphTime(s string) *timestamppb.Timestamp {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return timestamppb.New(t.UTC())
		}
	}
	return nil
}

// participants maps the message sender (role "from") and the chat members
// (role "member") to typed Participants. The sender is de-duplicated from the
// member list by user id so an author who is also a member appears once, as
// "from".
func participants(chat graphChat, msg graphMessage) []*askerv1.Participant {
	var out []*askerv1.Participant
	seen := make(map[string]bool)

	if msg.From != nil && msg.From.User != nil {
		u := msg.From.User
		out = append(out, &askerv1.Participant{Name: u.DisplayName, Handle: u.ID, Role: "from"})
		if u.ID != "" {
			seen[u.ID] = true
		}
	}
	for _, m := range chat.Members {
		if m.UserID != "" && seen[m.UserID] {
			continue
		}
		if m.DisplayName == "" && m.UserID == "" && m.Email == "" {
			continue
		}
		out = append(out, &askerv1.Participant{
			Name:   m.DisplayName,
			Email:  m.Email,
			Handle: m.UserID,
			Role:   "member",
		})
		if m.UserID != "" {
			seen[m.UserID] = true
		}
	}
	return out
}

// metadata builds the flat metadata map. Empty values are omitted.
func metadata(chat graphChat, msg graphMessage) map[string]string {
	md := make(map[string]string, 5)
	put := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			md[k] = v
		}
	}
	put("chat_id", chat.ID)
	put("message_id", msg.ID)
	put("web_url", msg.WebURL)
	put("chat_type", chat.ChatType)
	put("importance", msg.Importance)
	if len(md) == 0 {
		return nil
	}
	return md
}

// chatACL captures who may see a chat's messages (ADR-012): a group/meeting
// chat is shared among its members, so its member user-ids are the allowed
// principals. A one-on-one (or a chat whose membership Graph did not expand) is
// private by construction, so acl is left unset (nil).
func chatACL(chat graphChat) *askerv1.AclInfo {
	if strings.EqualFold(chat.ChatType, "oneOnOne") {
		return nil
	}
	if len(chat.Members) == 0 {
		return nil
	}
	var principals []string
	for _, m := range chat.Members {
		if m.UserID != "" {
			principals = append(principals, m.UserID)
		}
	}
	if len(principals) == 0 {
		return nil
	}
	return &askerv1.AclInfo{AllowedPrincipals: principals, IsPrivate: true}
}

// stripHTML is the crude text/html fallback shared with the tutorial's worked
// example: it drops <script>/<style> blocks, removes all tags (turning block
// boundaries into newlines), unescapes entities, and tidies whitespace. It is
// not a sanitizer — just enough to salvage searchable text from Teams' HTML
// message bodies.
func stripHTML(s string) string {
	lower := strings.ToLower(s)
	for _, blocked := range []string{"script", "style"} {
		for {
			start := strings.Index(lower, "<"+blocked)
			if start < 0 {
				break
			}
			end := strings.Index(lower[start:], "</"+blocked+">")
			if end < 0 {
				s = s[:start]
				lower = lower[:start]
				break
			}
			cut := start + end + len("</"+blocked+">")
			s = s[:start] + s[cut:]
			lower = lower[:start] + lower[cut:]
		}
	}

	var b strings.Builder
	b.Grow(len(s))
	for {
		open := strings.IndexByte(s, '<')
		if open < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:open])
		closeIdx := strings.IndexByte(s[open:], '>')
		if closeIdx < 0 {
			break // unterminated tag: drop the rest
		}
		if fields := strings.Fields(s[open+1 : open+closeIdx]); len(fields) > 0 {
			if tag := strings.ToLower(strings.Trim(fields[0], "/")); blockTags[tag] {
				b.WriteByte('\n')
			}
		}
		s = s[open+closeIdx+1:]
	}

	text := html.UnescapeString(b.String())
	lines := strings.Split(text, "\n")
	out := lines[:0]
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// blockTags are HTML elements whose boundary becomes a newline when tags are
// stripped, so paragraphs survive as chunking boundaries downstream.
var blockTags = map[string]bool{
	"br": true, "p": true, "div": true, "li": true, "ul": true, "ol": true,
	"tr": true, "table": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "blockquote": true,
}
