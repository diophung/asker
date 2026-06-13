package outlookmail

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

// noSubjectTitle is the display title for messages without a subject.
const noSubjectTitle = "(no subject)"

// graphMessage mirrors the subset of a Microsoft Graph message resource the
// connector consumes. Field names match the Graph JSON (note the "@odata."
// prefixed keys carried alongside the resource).
type graphMessage struct {
	ID                   string           `json:"id"`
	ODataETag            string           `json:"@odata.etag"`
	ChangeKey            string           `json:"changeKey"`
	Subject              string           `json:"subject"`
	Body                 *graphItemBody   `json:"body"`
	BodyPreview          string           `json:"bodyPreview"`
	From                 *graphRecipient  `json:"from"`
	ToRecipients         []graphRecipient `json:"toRecipients"`
	CcRecipients         []graphRecipient `json:"ccRecipients"`
	WebLink              string           `json:"webLink"`
	ParentFolderID       string           `json:"parentFolderId"`
	ConversationID       string           `json:"conversationId"`
	CreatedDateTime      string           `json:"createdDateTime"`
	LastModifiedDateTime string           `json:"lastModifiedDateTime"`

	// Removed is set by the delta query for a deletion: {"@removed":{"reason":"deleted"}}.
	Removed *graphRemoved `json:"@removed"`
}

// graphItemBody is a message body: contentType is "html" or "text".
type graphItemBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

// graphRecipient is a Graph recipient wrapper.
type graphRecipient struct {
	EmailAddress graphEmailAddress `json:"emailAddress"`
}

// graphEmailAddress is the recipient's name and address.
type graphEmailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// graphRemoved marks a delta item as a deletion.
type graphRemoved struct {
	Reason string `json:"reason"`
}

// isRemoved reports whether this delta item represents a deletion.
func (m *graphMessage) isRemoved() bool { return m.Removed != nil }

// messageDocument maps one Graph message to the canonical Document: type
// EMAIL, doc_id sdk.DocID("outlook-mail", id), title from subject, body from
// body.content (HTML stripped to text when contentType=="html"), participants
// from from/toRecipients/ccRecipients, metadata (message_id, conversation_id,
// web_link, folder), ts.created/modified from createdDateTime/
// lastModifiedDateTime, and version_etag from @odata.etag (changeKey, then a
// sha256 of the body, as fallbacks).
func messageDocument(tenant string, m *graphMessage) *askerv1.Document {
	title := strings.TrimSpace(m.Subject)
	if title == "" {
		title = noSubjectTitle
	}
	body := messageBody(m)

	doc := &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, m.ID),
		ConnectorId:    connectorID,
		SourceNativeId: m.ID,
		Type:           askerv1.DocType_EMAIL,
		Title:          title,
		BodyText:       body,
		Participants:   participants(m),
		Metadata:       metadata(m),
		VersionEtag:    versionEtag(m, body),
	}
	if ts := timestamps(m); ts != nil {
		doc.Ts = ts
	}
	return doc
}

// tombstoneDocument builds the deletion Document for one message id: identity
// fields plus tombstone.deleted and deleted_at — no body, no chunks. The
// version_etag is the source etag/changeKey when present (a delta deletion
// carries one), else a constant so the tombstone still has a non-empty etag.
func tombstoneDocument(tenant string, m *graphMessage, now time.Time) *askerv1.Document {
	etag := firstNonEmpty(m.ODataETag, m.ChangeKey, "deleted")
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, m.ID),
		ConnectorId:    connectorID,
		SourceNativeId: m.ID,
		Type:           askerv1.DocType_EMAIL,
		VersionEtag:    etag,
		Tombstone: &askerv1.Tombstone{
			Deleted:   true,
			DeletedAt: timestamppb.New(now.UTC()),
		},
	}
}

// versionEtag picks a value that changes iff the message content changes: the
// Graph @odata.etag (which embeds the changeKey), then the changeKey alone,
// then a sha256 of the extracted body as a last resort.
func versionEtag(m *graphMessage, body string) string {
	if v := firstNonEmpty(m.ODataETag, m.ChangeKey); v != "" {
		return v
	}
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// messageBody returns the searchable plain text of the message: body.content
// HTML-stripped when contentType is "html", the raw content for "text", or the
// bodyPreview when no body is present.
func messageBody(m *graphMessage) string {
	if m.Body != nil && m.Body.Content != "" {
		if strings.EqualFold(m.Body.ContentType, "html") {
			return stripHTML(m.Body.Content)
		}
		return strings.TrimSpace(m.Body.Content)
	}
	return strings.TrimSpace(m.BodyPreview)
}

// metadata builds the flat metadata map; empty values are omitted.
func metadata(m *graphMessage) map[string]string {
	md := make(map[string]string, 4)
	put := func(k, v string) {
		if v != "" {
			md[k] = v
		}
	}
	put("message_id", m.ID)
	put("conversation_id", m.ConversationID)
	put("web_link", m.WebLink)
	put("folder", m.ParentFolderID)
	return md
}

// participants maps from (role "sender"), toRecipients and ccRecipients (role
// "recipient") to typed participants. Recipients with neither a name nor an
// address are skipped.
func participants(m *graphMessage) []*askerv1.Participant {
	var out []*askerv1.Participant
	add := func(r graphEmailAddress, role string) {
		name := strings.TrimSpace(r.Name)
		addr := strings.TrimSpace(r.Address)
		if name == "" && addr == "" {
			return
		}
		out = append(out, &askerv1.Participant{Name: name, Email: addr, Role: role})
	}
	if m.From != nil {
		add(m.From.EmailAddress, "sender")
	}
	for _, r := range m.ToRecipients {
		add(r.EmailAddress, "recipient")
	}
	for _, r := range m.CcRecipients {
		add(r.EmailAddress, "recipient")
	}
	return out
}

// timestamps maps createdDateTime / lastModifiedDateTime (RFC 3339) to the
// canonical Timestamps. A missing/unparseable value is omitted; an all-empty
// result is reported as nil so the document leaves ts unset.
func timestamps(m *graphMessage) *askerv1.Timestamps {
	var ts askerv1.Timestamps
	any := false
	if t, ok := parseGraphTime(m.CreatedDateTime); ok {
		ts.Created = timestamppb.New(t)
		any = true
	}
	if t, ok := parseGraphTime(m.LastModifiedDateTime); ok {
		ts.Modified = timestamppb.New(t)
		any = true
	}
	if !any {
		return nil
	}
	return &ts
}

// graphTimeLayouts are the timestamp formats Graph emits (RFC 3339, sometimes
// without a zone suffix — treated as UTC, which is what Graph means).
var graphTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.9999999",
	"2006-01-02T15:04:05",
}

// parseGraphTime parses a Graph timestamp into UTC.
func parseGraphTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range graphTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// firstNonEmpty returns the first non-empty trimmed string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// blockTags are HTML elements whose boundary becomes a newline when tags are
// stripped, so paragraphs survive as chunking boundaries downstream.
var blockTags = map[string]bool{
	"br": true, "p": true, "div": true, "li": true, "ul": true, "ol": true,
	"tr": true, "table": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "blockquote": true,
}

// stripHTML salvages searchable text from an HTML body: it drops
// <script>/<style> blocks, removes all tags (turning block boundaries into
// newlines), unescapes entities, and tidies whitespace. It is not a sanitizer
// and not meant to render anything.
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
			// Unterminated tag: drop the rest.
			break
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
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
