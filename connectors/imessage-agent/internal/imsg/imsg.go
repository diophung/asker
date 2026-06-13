// Package imsg is the pure, side-effect-free core of the iMessage local
// agent: it maps a row read from the macOS Messages SQLite database
// (~/Library/Messages/chat.db) to a canonical Asker Document, converts Apple's
// epoch timestamps to wall-clock time, and renders a chat transcript suitable
// for upload as a searchable FILE document.
//
// Nothing in this package touches the database, the network, or the
// filesystem: the agent's main package owns sqlite3 invocation, HTTP upload,
// and state persistence, and calls into here with already-decoded rows. That
// split keeps the Document-mapping contract unit-testable with hand-authored
// fixture rows and no chat.db present (the M2 contract-test requirement).
//
// # Apple epoch
//
// chat.db's message.date column is nanoseconds since the Apple/Cocoa epoch
// 2001-01-01T00:00:00Z (Mac OS X 10.6+ stores nanoseconds; older databases
// stored whole seconds). [AppleTimeToTime] converts the nanosecond form; the
// agent normalizes seconds-form values before calling it.
//
// # Document mapping
//
// [RowToDocument] turns one [Row] into an *askerv1.Document of type
// CHAT_MESSAGE. The doc_id is sdk.DocID("imessage", chatGUID + ":" + rowID) so
// re-reads of the same message converge on one indexed document; version_etag
// is sha256(text + date) so an edited message (iMessage edits change the text,
// and a re-send changes the date) re-indexes while an unchanged re-read is a
// no-op upsert. A 1:1 or group chat on a personal device is private by
// construction, so acl is left unset (ADR-012).
//
// Note: in M2 the agent does not emit these Documents directly — it renders a
// transcript and uploads it through the gateway's /v1/upload endpoint, which
// produces a FILE document. RowToDocument and the CHAT_MESSAGE mapping exist
// for a future bulk-document ingestion API (see the agent README) and are the
// contract-tested core today.
package imsg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ConnectorID is the stable connector identifier baked into every doc_id via
// sdk.DocID. It must never change once documents exist.
const ConnectorID = "imessage"

// meHandle is the synthetic participant handle for the device owner: chat.db
// stores is_from_me rather than the owner's own address, so outbound messages
// are attributed to a stable "me" handle.
const meHandle = "me"

// appleEpoch is 2001-01-01T00:00:00Z, the zero point of chat.db's date column.
var appleEpoch = time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)

// secondsThreshold distinguishes the two chat.db date encodings. Modern
// databases store nanoseconds since the Apple epoch; a nanosecond value for any
// plausible message is far larger than this, whereas the legacy whole-second
// encoding is far smaller. A value at or below the threshold is treated as
// seconds and scaled to nanoseconds.
//
// The boundary is chosen so the two encodings never collide for real
// timestamps: a seconds value is at most ~1e10 (year ~2318), while a nanosecond
// value is at least ~1e16 within the first year after the epoch. 1e12 sits
// safely between them. Scaling a value at the threshold (1e12 s) to nanoseconds
// is 1e21, which overflows int64; but no real message reaches even 1e10 s, so
// the in-range seconds values the agent actually sees scale without overflow.
const secondsThreshold = 1_000_000_000_000 // 1e12

// Row is one decoded chat.db row: the join of message, handle, and chat that
// the agent's sqlite3 query produces. Fields use the chat.db column semantics.
// It is the agent's responsibility to populate this from a sqlite3 -json row;
// imsg only maps it.
type Row struct {
	// RowID is message.ROWID, the monotonic primary key used as the
	// incremental high-water mark.
	RowID int64
	// ChatGUID is chat.guid, the stable chat identifier (e.g.
	// "iMessage;-;+15551234567"). Combined with RowID it forms the doc_id.
	ChatGUID string
	// ChatName is chat.display_name (group chats) when set; may be empty for
	// 1:1 chats.
	ChatName string
	// Handle is handle.id, the other party's address (phone number or email).
	// Empty for messages with no associated handle.
	Handle string
	// Text is message.text, the message body. May be empty (e.g. an
	// attachment-only message).
	Text string
	// IsFromMe is message.is_from_me: true for messages the device owner sent.
	IsFromMe bool
	// Service is message.service, "iMessage" or "SMS".
	Service string
	// Date is message.date: nanoseconds (modern) or seconds (legacy) since the
	// Apple epoch. NormalizeDate handles either.
	Date int64
}

// AppleTimeToTime converts a chat.db nanosecond timestamp (nanoseconds since
// the Apple epoch 2001-01-01T00:00:00Z) to a UTC time.Time. A zero or negative
// input yields the zero Time, which the caller treats as "no timestamp".
func AppleTimeToTime(ns int64) time.Time {
	if ns <= 0 {
		return time.Time{}
	}
	return appleEpoch.Add(time.Duration(ns) * time.Nanosecond).UTC()
}

// NormalizeDate coerces a chat.db date value to nanoseconds since the Apple
// epoch, accepting both the modern nanosecond encoding and the legacy whole-
// second encoding (Mac OS X 10.5 and earlier). A value at or below
// secondsThreshold is interpreted as seconds and scaled up. Zero and negative
// values pass through unchanged so AppleTimeToTime maps them to the zero Time.
func NormalizeDate(date int64) int64 {
	if date > 0 && date <= secondsThreshold {
		return date * int64(time.Second/time.Nanosecond)
	}
	return date
}

// senderHandle is the handle attributed to a row's author: "me" for outbound
// messages, otherwise the row's handle (trimmed). It returns "" when an inbound
// message carries no handle, so the caller can decide whether to record a
// participant.
func senderHandle(r Row) string {
	if r.IsFromMe {
		return meHandle
	}
	return strings.TrimSpace(r.Handle)
}

// snippet is the one-line preview used in titles: the first line of text,
// trimmed and truncated to maxSnippet runes (a trailing ellipsis marks
// truncation). Newlines collapse to spaces so a multi-line message stays on one
// title line.
const maxSnippet = 80

func snippet(text string) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	runes := []rune(collapsed)
	if len(runes) <= maxSnippet {
		return collapsed
	}
	return strings.TrimSpace(string(runes[:maxSnippet])) + "…"
}

// chatLabel is the human-readable chat name for titles and metadata: the
// display name when set, else the chat GUID, else "(unknown chat)".
func chatLabel(r Row) string {
	if name := strings.TrimSpace(r.ChatName); name != "" {
		return name
	}
	if guid := strings.TrimSpace(r.ChatGUID); guid != "" {
		return guid
	}
	return "(unknown chat)"
}

// RowToDocument maps one chat.db Row to a canonical CHAT_MESSAGE Document for
// tenant. The mapping:
//
//   - doc_id = sdk.DocID("imessage", chatGUID + ":" + rowID); both
//     connector_id and source_native_id are set so ValidateDocument passes.
//   - title = "<chat label>: <snippet of text>".
//   - body_text = the message text verbatim.
//   - participants = the sender handle (role "from"); for an inbound message
//     "me" is added as the recipient (role "to"), and for an outbound message
//     the chat's other party is recorded as "to" when known.
//   - metadata = chat, chat_guid, handle, service, is_from_me, rowid.
//   - version_etag = sha256(text + ":" + date) so edits re-index and unchanged
//     re-reads are idempotent no-ops.
//   - ts.created = the converted Apple date (omitted when the date is absent).
//   - acl is unset: a personal-device chat is private by construction
//     (ADR-012).
func RowToDocument(tenant string, r Row) *askerv1.Document {
	nativeID := r.ChatGUID + ":" + strconv.FormatInt(r.RowID, 10)

	doc := &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(ConnectorID, nativeID),
		ConnectorId:    ConnectorID,
		SourceNativeId: nativeID,
		Type:           askerv1.DocType_CHAT_MESSAGE,
		Title:          documentTitle(r),
		BodyText:       r.Text,
		Participants:   participants(r),
		Metadata:       metadata(r),
		VersionEtag:    versionEtag(r),
	}

	if created := AppleTimeToTime(NormalizeDate(r.Date)); !created.IsZero() {
		doc.Ts = &askerv1.Timestamps{Created: timestamppb.New(created)}
	}
	return doc
}

// documentTitle builds the display title: the chat label and a snippet of the
// message text. A message with no text shows only the chat label.
func documentTitle(r Row) string {
	label := chatLabel(r)
	if s := snippet(r.Text); s != "" {
		return label + ": " + s
	}
	return label
}

// versionEtag is sha256(text + ":" + date) in lowercase hex. iMessage edits
// change the text and re-sends change the date, so the etag advances whenever
// the message content the agent can observe changes, while an unchanged re-read
// reproduces the same etag (idempotent upsert).
func versionEtag(r Row) string {
	sum := sha256.Sum256([]byte(r.Text + ":" + strconv.FormatInt(r.Date, 10)))
	return hex.EncodeToString(sum[:])
}

// participants returns the typed people facet for a row. The sender is always
// recorded with role "from" (handle "me" for outbound). The counterparty is
// recorded with role "to" when known: "me" receives an inbound message, and the
// chat handle receives an outbound message. Unknown counterparties are omitted
// rather than guessed.
func participants(r Row) []*askerv1.Participant {
	var out []*askerv1.Participant
	if from := senderHandle(r); from != "" {
		out = append(out, participantForHandle(from, "from"))
	}
	if r.IsFromMe {
		if to := strings.TrimSpace(r.Handle); to != "" {
			out = append(out, participantForHandle(to, "to"))
		}
	} else {
		out = append(out, participantForHandle(meHandle, "to"))
	}
	return out
}

// participantForHandle builds a Participant for a chat.db handle. The synthetic
// "me" handle becomes a named participant; an "@"-bearing handle is treated as
// an email address, otherwise it is a platform handle (typically a phone
// number).
func participantForHandle(handle, role string) *askerv1.Participant {
	if handle == meHandle {
		return &askerv1.Participant{Name: meHandle, Handle: meHandle, Role: role}
	}
	p := &askerv1.Participant{Role: role}
	if strings.Contains(handle, "@") {
		p.Email = handle
	} else {
		p.Handle = handle
	}
	return p
}

// metadata builds the flat metadata map. Empty values are omitted, except
// is_from_me and rowid which are always present (they are never meaningless).
func metadata(r Row) map[string]string {
	md := make(map[string]string, 6)
	put := func(k, v string) {
		if v != "" {
			md[k] = v
		}
	}
	put("chat", strings.TrimSpace(r.ChatName))
	put("chat_guid", strings.TrimSpace(r.ChatGUID))
	put("handle", strings.TrimSpace(r.Handle))
	put("service", strings.TrimSpace(r.Service))
	md["is_from_me"] = strconv.FormatBool(r.IsFromMe)
	md["rowid"] = strconv.FormatInt(r.RowID, 10)
	return md
}

// RenderTranscript renders an ordered slice of rows (assumed to be one chat,
// in chronological order) to a plain-text transcript suitable for upload as a
// FILE document. The header names the chat and service; each line is
//
//	[2006-01-02 15:04] <sender>: <text>
//
// where <sender> is "me" for outbound messages and the handle otherwise. A
// message with no timestamp omits the bracketed time; a message with no text
// renders "(no text)" so attachment-only messages still appear in order.
// Rendering is deterministic for a given input (golden-testable).
func RenderTranscript(rows []Row) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	header := rows[0]
	fmt.Fprintf(&b, "Chat: %s\n", chatLabel(header))
	if svc := strings.TrimSpace(header.Service); svc != "" {
		fmt.Fprintf(&b, "Service: %s\n", svc)
	}
	b.WriteString("\n")

	for _, r := range rows {
		sender := senderHandle(r)
		if sender == "" {
			sender = "(unknown)"
		}
		text := r.Text
		if strings.TrimSpace(text) == "" {
			text = "(no text)"
		}
		text = strings.ReplaceAll(text, "\n", " ")
		if ts := AppleTimeToTime(NormalizeDate(r.Date)); !ts.IsZero() {
			fmt.Fprintf(&b, "[%s] %s: %s\n", ts.Format("2006-01-02 15:04"), sender, text)
		} else {
			fmt.Fprintf(&b, "%s: %s\n", sender, text)
		}
	}
	return b.String()
}

// TranscriptFilename returns a stable, filesystem-safe filename for a chat's
// transcript upload, derived from the chat GUID. It is used as the multipart
// upload filename so re-uploads of the same chat carry a consistent name.
func TranscriptFilename(chatGUID string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, chatGUID)
	safe = strings.Trim(safe, "_")
	if safe == "" {
		safe = "chat"
	}
	return "imessage-" + safe + ".txt"
}
