package slack

import (
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// titleMaxRunes bounds the synthesized message title.
const titleMaxRunes = 80

// channelInfo is the subset of a conversations.list / channel object the
// connector needs: id, human name, privacy, and membership for ACL capture.
type channelInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	IsPrivate bool     `json:"is_private"`
	IsIM      bool     `json:"is_im"`
	IsMPIM    bool     `json:"is_mpim"`
	Members   []string `json:"members"`
}

// displayName returns the channel's display name with a leading '#'; DMs and
// group DMs have no name and fall back to their id.
func (ch channelInfo) displayName() string {
	if ch.Name != "" {
		return "#" + ch.Name
	}
	return ch.ID
}

// message is the subset of a conversations.history message object the
// connector maps. Slack delivers ts and edited.ts as "seconds.micros" strings.
type message struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	Team     string `json:"team"`
	Edited   *struct {
		User string `json:"user"`
		TS   string `json:"ts"`
	} `json:"edited"`
}

// nativeID is the stable source-native id for a message: "<channel>:<ts>". A
// message ts is unique only within its channel, so the channel is part of the
// id (matching doc_id=sdk.DocID("slack", channel+":"+ts) from the task).
func nativeID(channel, ts string) string {
	return channel + ":" + ts
}

// author is the message's sending principal id: a human user id, or a bot id
// for app/integration messages.
func (m message) author() string {
	if m.User != "" {
		return m.User
	}
	return m.BotID
}

// messageDocument maps one channel message to the canonical Document: type
// CHAT_MESSAGE, doc_id sdk.DocID("slack", channel+":"+ts), title from the
// first ~80 chars of text (or "<channel> message"), body from text, a single
// user participant, channel/ts/thread metadata, ts.created from the message
// ts, and version_etag from edited.ts (when the message was edited) else ts so
// an edit produces a new etag for the same doc_id. Shared channels (public or
// private with a member list) carry an AclInfo per ADR-012.
func messageDocument(tenant string, ch channelInfo, m message) *askerv1.Document {
	native := nativeID(ch.ID, m.TS)
	doc := &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, native),
		ConnectorId:    connectorID,
		SourceNativeId: native,
		Type:           askerv1.DocType_CHAT_MESSAGE,
		Title:          messageTitle(ch, m.Text),
		BodyText:       strings.TrimSpace(m.Text),
		Participants:   messageParticipants(m),
		Metadata:       messageMetadata(ch, m),
		VersionEtag:    versionEtag(m),
	}
	if ts := parseSlackTS(m.TS); ts != nil {
		doc.Ts = &askerv1.Timestamps{Created: ts}
		if m.Edited != nil {
			if mod := parseSlackTS(m.Edited.TS); mod != nil {
				doc.Ts.Modified = mod
			}
		}
	}
	if acl := channelACL(ch); acl != nil {
		doc.Acl = acl
	}
	return doc
}

// tombstoneDocument builds the deletion Document for one message: identity
// fields plus tombstone.deleted and deleted_at — no body, no chunks. The
// version_etag is the deletion event ts, which is newer than any prior upsert
// etag for the same doc_id so the delete wins the idempotent merge.
func (c *Connector) tombstoneDocument(tenant, channel, ts, eventTS string) *askerv1.Document {
	native := nativeID(channel, ts)
	etag := eventTS
	if etag == "" {
		etag = ts
	}
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, native),
		ConnectorId:    connectorID,
		SourceNativeId: native,
		Type:           askerv1.DocType_CHAT_MESSAGE,
		VersionEtag:    etag,
		Tombstone: &askerv1.Tombstone{
			Deleted:   true,
			DeletedAt: timestamppb.New(c.now().UTC()),
		},
	}
}

// versionEtag returns a value that changes iff the message content changes:
// edited.ts when the message was edited, else the message ts. Both are
// monotonic per message, so an edit always yields a newer etag.
func versionEtag(m message) string {
	if m.Edited != nil && m.Edited.TS != "" {
		return m.Edited.TS
	}
	return m.TS
}

// messageTitle returns the first ~80 runes of trimmed text on a single line,
// or "<channel> message" when the message has no text (e.g. a share/file-only
// post).
func messageTitle(ch channelInfo, text string) string {
	t := strings.TrimSpace(collapseWhitespace(text))
	if t == "" {
		return ch.displayName() + " message"
	}
	runes := []rune(t)
	if len(runes) > titleMaxRunes {
		return strings.TrimSpace(string(runes[:titleMaxRunes]))
	}
	return t
}

// collapseWhitespace turns any run of whitespace into a single space so a
// multi-line message yields a single-line title.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// messageParticipants returns the message author as a single "from"
// participant. The id (a Slack user id like "U123" or a bot id) goes in the
// handle; name resolution via users.info is intentionally skipped (it is an
// extra round-trip per author), so the id is the searchable handle and is also
// kept in metadata.
func messageParticipants(m message) []*askerv1.Participant {
	author := m.author()
	if author == "" {
		return nil
	}
	return []*askerv1.Participant{{Handle: author, Role: "from"}}
}

// messageMetadata builds the flat metadata map: channel id, channel name, ts,
// thread_ts, team, and a best-effort permalink. Empty values are omitted.
func messageMetadata(ch channelInfo, m message) map[string]string {
	md := make(map[string]string, 6)
	put := func(k, v string) {
		if v != "" {
			md[k] = v
		}
	}
	put("channel", ch.ID)
	put("channel_name", ch.Name)
	put("ts", m.TS)
	put("thread_ts", m.ThreadTS)
	put("team", m.Team)
	put("user", m.author())
	put("permalink", permalink(m.Team, ch.ID, m.TS))
	return md
}

// permalink synthesizes the archive URL Slack uses for a message
// (https://<team>.slack.com/archives/<channel>/p<ts-without-dot>). It is
// best-effort: without the team domain it returns "".
func permalink(team, channel, ts string) string {
	if team == "" || channel == "" || ts == "" {
		return ""
	}
	p := strings.ReplaceAll(ts, ".", "")
	return "https://app.slack.com/archives/" + channel + "/p" + p
}

// channelACL captures who-may-see-it for shared channels (ADR-012). Public and
// private channels are shared sources: their members are the allowed
// principals, and a private channel is_private. 1:1 DMs are
// private-by-construction and leave the ACL unset (returns nil).
func channelACL(ch channelInfo) *askerv1.AclInfo {
	if ch.IsIM {
		return nil
	}
	acl := &askerv1.AclInfo{IsPrivate: ch.IsPrivate}
	if len(ch.Members) > 0 {
		acl.AllowedPrincipals = append([]string(nil), ch.Members...)
	}
	return acl
}

// parseSlackTS converts a Slack "seconds.micros" timestamp to a protobuf
// Timestamp (UTC). It returns nil for an empty or unparseable value.
func parseSlackTS(ts string) *timestamppb.Timestamp {
	if ts == "" {
		return nil
	}
	secStr, fracStr, _ := strings.Cut(ts, ".")
	sec, err := strconv.ParseInt(secStr, 10, 64)
	if err != nil {
		return nil
	}
	var nanos int64
	if fracStr != "" {
		// Slack fractions are microseconds (6 digits); pad/truncate to nanos.
		frac := (fracStr + "000000000")[:9]
		nanos, err = strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return nil
		}
	}
	return timestamppb.New(time.Unix(sec, nanos).UTC())
}
