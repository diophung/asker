package gmail

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"html"
	"net/mail"
	"strconv"
	"strings"
	"time"

	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// noSubjectTitle is the display title for messages without a Subject header.
const noSubjectTitle = "(no subject)"

// messageDocument maps one users.messages.get(format=full) result to the
// canonical Document: type EMAIL, doc_id sdk.DocID("gmail", messageID),
// title from Subject, body from the text/plain part (HTML fallback),
// participants from From/To/Cc, ts.created from internalDate, and
// version_etag from the message's historyId (sha256 of the body when the
// source supplies none).
func messageDocument(tenant string, msg *gmailapi.Message) (*askerv1.Document, error) {
	if msg == nil || msg.Id == "" {
		return nil, errors.New("gmail: messages.get returned a message without an id")
	}

	title := headerValue(msg.Payload, "Subject")
	if strings.TrimSpace(title) == "" {
		title = noSubjectTitle
	}
	body := extractBody(msg.Payload)

	etag := strconv.FormatUint(msg.HistoryId, 10)
	if msg.HistoryId == 0 {
		sum := sha256.Sum256([]byte(body))
		etag = hex.EncodeToString(sum[:])
	}

	doc := &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, msg.Id),
		ConnectorId:    connectorID,
		SourceNativeId: msg.Id,
		Type:           askerv1.DocType_EMAIL,
		Title:          title,
		BodyText:       body,
		Participants:   participants(msg.Payload),
		Metadata:       metadata(msg),
		VersionEtag:    etag,
	}
	if msg.InternalDate > 0 {
		doc.Ts = &askerv1.Timestamps{Created: timestamppb.New(time.UnixMilli(msg.InternalDate).UTC())}
	}
	return doc, nil
}

// tombstoneDocument builds the deletion Document for one message: identity
// fields plus tombstone.deleted and deleted_at — no body, no chunks. The
// version_etag is the history record id that observed the deletion, which is
// always newer than any prior upsert etag for the same doc_id.
func (c *Connector) tombstoneDocument(tenant, msgID string, historyID uint64) *askerv1.Document {
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, msgID),
		ConnectorId:    connectorID,
		SourceNativeId: msgID,
		Type:           askerv1.DocType_EMAIL,
		VersionEtag:    strconv.FormatUint(historyID, 10),
		Tombstone: &askerv1.Tombstone{
			Deleted:   true,
			DeletedAt: timestamppb.New(c.now().UTC()),
		},
	}
}

// metadata builds the flat metadata map: from, to, thread_id, message_id, and
// web_link — a browser-openable deep link to the message in the Gmail web UI.
// The #all/<id> view opens the message regardless of which label/folder holds
// it; u/0 targets the browser's first signed-in Google account. Empty values
// are omitted (the caller guarantees msg.Id is non-empty before this runs).
func metadata(msg *gmailapi.Message) map[string]string {
	md := make(map[string]string, 5)
	put := func(k, v string) {
		if v != "" {
			md[k] = v
		}
	}
	put("from", headerValue(msg.Payload, "From"))
	put("to", headerValue(msg.Payload, "To"))
	put("thread_id", msg.ThreadId)
	put("message_id", msg.Id)
	put("web_link", "https://mail.google.com/mail/u/0/#all/"+msg.Id)
	return md
}

// participants parses the From/To/Cc headers (RFC 5322 address lists via
// net/mail) into typed participants. A header that does not parse degrades
// to a single participant whose name is the raw header value, so malformed
// senders are still searchable.
func participants(payload *gmailapi.MessagePart) []*askerv1.Participant {
	var out []*askerv1.Participant
	for _, h := range []struct{ header, role string }{
		{"From", "from"},
		{"To", "to"},
		{"Cc", "cc"},
	} {
		raw := strings.TrimSpace(headerValue(payload, h.header))
		if raw == "" {
			continue
		}
		addrs, err := mail.ParseAddressList(raw)
		if err != nil {
			out = append(out, &askerv1.Participant{Name: raw, Role: h.role})
			continue
		}
		for _, a := range addrs {
			out = append(out, &askerv1.Participant{Name: a.Name, Email: a.Address, Role: h.role})
		}
	}
	return out
}

// headerValue returns the first value of name among the top-level payload
// headers (where Gmail puts the RFC 2822 message headers), matched
// case-insensitively.
func headerValue(payload *gmailapi.MessagePart, name string) string {
	if payload == nil {
		return ""
	}
	for _, h := range payload.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// extractBody walks the MIME part tree and returns the message text: all
// text/plain parts joined when any exist, otherwise all text/html parts with
// tags crudely stripped. Attachment parts (body delivered separately) and
// undecodable data are skipped.
func extractBody(payload *gmailapi.MessagePart) string {
	var plains, htmls []string
	walkParts(payload, func(part *gmailapi.MessagePart) {
		if part.Body == nil || part.Body.Data == "" {
			return
		}
		mt := strings.ToLower(part.MimeType)
		switch {
		case strings.HasPrefix(mt, "text/plain"):
			if text, ok := decodeBase64(part.Body.Data); ok {
				plains = append(plains, string(text))
			}
		case strings.HasPrefix(mt, "text/html"):
			if text, ok := decodeBase64(part.Body.Data); ok {
				htmls = append(htmls, stripHTML(string(text)))
			}
		}
	})
	if len(plains) > 0 {
		return strings.TrimSpace(strings.Join(plains, "\n\n"))
	}
	return strings.TrimSpace(strings.Join(htmls, "\n\n"))
}

// walkParts visits payload and every nested part depth-first.
func walkParts(part *gmailapi.MessagePart, visit func(*gmailapi.MessagePart)) {
	if part == nil {
		return
	}
	visit(part)
	for _, child := range part.Parts {
		walkParts(child, visit)
	}
}

// base64Codecs are tried in order: the Gmail API serves unpadded base64url
// (RFC 4648 §5), but tolerate the padded and standard variants too.
var base64Codecs = []*base64.Encoding{
	base64.RawURLEncoding,
	base64.URLEncoding,
	base64.StdEncoding,
	base64.RawStdEncoding,
}

// decodeBase64 decodes s with the first accepting codec.
func decodeBase64(s string) ([]byte, bool) {
	for _, enc := range base64Codecs {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

// blockTags are HTML elements whose boundary becomes a newline when tags are
// stripped, so paragraphs survive as chunking boundaries downstream.
var blockTags = map[string]bool{
	"br": true, "p": true, "div": true, "li": true, "ul": true, "ol": true,
	"tr": true, "table": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "blockquote": true,
}

// stripHTML is the crude text/html fallback: it drops <script>/<style>
// blocks, removes all tags (turning block boundaries into newlines),
// unescapes entities, and tidies whitespace. It is not a sanitizer and not
// meant to render anything — just to salvage searchable text.
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
