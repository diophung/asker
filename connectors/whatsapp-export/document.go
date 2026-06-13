package whatsappexport

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

const (
	// systemSender is the participant-less marker recorded in metadata for
	// system lines ("Messages and calls are end-to-end encrypted", "Alice added
	// Bob"). We emit these (rather than skip) so a search for them still hits;
	// they carry no participant.
	systemSender = "system"

	// titleMaxRunes caps the derived title length (~60 chars per the contract).
	titleMaxRunes = 60
)

// nativeID is the stable, POSITION-INDEPENDENT source-native id for a message:
// sha256(chatName + "|" + rawSentAt + "|" + sender + "|" + text) in lowercase
// hex, with a ":<occurrence>" suffix appended ONLY for the 2nd and later exact
// duplicate within the same export. It binds the message to its chat and to its
// content/identity (timestamp, sender, text) but NOT to its ordinal position, so:
//
//   - re-importing a larger export of the same chat re-derives the SAME id for
//     each already-seen message (same chat, timestamp, sender, text), making
//     upserts idempotent and the append-only tail the only new docs;
//   - inserting or deleting a message in the MIDDLE of a re-export shifts every
//     later message's line index but does NOT change any later id — the bug this
//     replaces, where lineIndex was part of the id, orphaned/duplicated every
//     message after the edit point; and
//   - two different messages stay distinct (different content => different hash),
//     while a LEGITIMATE duplicate (same sender+timestamp+text sent twice) is
//     disambiguated by the occurrence index assigned at parse time among exact
//     duplicates, so identical messages still get distinct, stable ids.
//
// chatName scopes the id so two chats imported into the same tenant never
// collide. The "|" separators are unambiguous because the components are hashed,
// not parsed back out.
func nativeID(chatName, rawSentAt, sender, text string, occurrence int) string {
	h := sha256.New()
	_, _ = h.Write([]byte(chatName))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(rawSentAt))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(sender))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(text))
	id := hex.EncodeToString(h.Sum(nil))
	if occurrence > 0 {
		id += ":" + strconv.Itoa(occurrence)
	}
	return id
}

// messageEtag is the per-message version etag: sha256(sender + "\x00" +
// rawSentAt + "\x00" + text) in lowercase hex. It is non-empty and changes iff
// the sender, timestamp, or text changes, so downstream upserts on
// (doc_id, version_etag) are idempotent.
func messageEtag(m message) string {
	h := sha256.New()
	_, _ = h.Write([]byte(m.sender))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(m.rawSentAt))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(m.text))
	return hex.EncodeToString(h.Sum(nil))
}

// toDocument maps one parsed message to the canonical CHAT_MESSAGE Document for
// the given chat. proto3 strings must be valid UTF-8, so all source-derived
// text is sanitized.
func toDocument(tenant, chatName string, m message) *askerv1.Document {
	text := strings.ToValidUTF8(m.text, "")
	sender := strings.ToValidUTF8(m.sender, "")
	native := nativeID(chatName, m.rawSentAt, sender, text, m.occurrence)

	doc := &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, native),
		ConnectorId:    connectorID,
		SourceNativeId: native,
		Type:           askerv1.DocType_CHAT_MESSAGE,
		Title:          title(chatName, text),
		BodyText:       text,
		Participants:   participants(sender, m.system),
		Metadata:       metadata(chatName, sender, m),
		VersionEtag:    messageEtag(m),
	}
	if m.hasTime {
		doc.Ts = &askerv1.Timestamps{Created: timestamppb.New(m.sentAt)}
	}
	return doc
}

// title is the first ~60 characters of the message text, or "<chatName>
// message" when the text is empty (e.g. a "<Media omitted>" line still has
// text, but a truly empty body falls back).
func title(chatName, text string) string {
	t := strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if t == "" {
		if chatName != "" {
			return chatName + " message"
		}
		return "message"
	}
	if utf8.RuneCountInString(t) <= titleMaxRunes {
		return t
	}
	// Truncate at a rune boundary.
	count := 0
	for i := range t {
		if count == titleMaxRunes {
			return strings.TrimSpace(t[:i])
		}
		count++
	}
	return t
}

// participants returns the single sender as a "sender"-role participant (name
// and handle both the export's display name — WhatsApp exports carry no stable
// handle/phone separate from the display name). A system line has no
// participant.
func participants(sender string, system bool) []*askerv1.Participant {
	if system || sender == "" {
		return nil
	}
	return []*askerv1.Participant{{
		Name:   sender,
		Handle: sender,
		Role:   "sender",
	}}
}

// metadata records the flat facets: chat_name, sender (or "system"), sent_at
// (the raw header timestamp as exported), line_index, and msg_format. Empty
// values are omitted, except sender which is always set (system marker).
func metadata(chatName, sender string, m message) map[string]string {
	md := map[string]string{
		"line_index": strconv.Itoa(m.lineIndex),
	}
	if chatName != "" {
		md["chat_name"] = chatName
	}
	if m.system {
		md["sender"] = systemSender
	} else if sender != "" {
		md["sender"] = sender
	}
	if m.rawSentAt != "" {
		md["sent_at"] = strings.ToValidUTF8(m.rawSentAt, "")
	}
	if m.format != "" {
		md["msg_format"] = m.format
	}
	return md
}
