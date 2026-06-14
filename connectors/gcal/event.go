package gcal

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

const (
	// statusCanceled is the Calendar event status that denotes a deletion on
	// an incremental sync; it maps to a tombstone. The API spells the status
	// value with the British double-l, so the constant value matches it
	// verbatim (the nolint keeps the US-locale spell check off that literal).
	statusCanceled = "cancelled" //nolint:misspell // Calendar API status value, spelled verbatim

	// noTitle is the display title for an event without a summary.
	noTitle = "(no title)"
)

// nativeID is the source-native id used in the doc_id: "<calendarId>:<eventId>".
// Scoping by calendar id keeps events from two calendars an instance might sync
// distinct, exactly as the build contract requires.
func nativeID(calendarID, eventID string) string {
	return calendarID + ":" + eventID
}

// eventDocument maps one Calendar event to the canonical Document: type
// CALENDAR_EVENT, doc_id sdk.DocID("gcal", calendarId+":"+eventId), title from
// summary, body from description + location, participants from organizer +
// attendees, and ts.created/modified from the event's created/updated. The
// version_etag is the event etag (sha256 of the body when the source omits one,
// so upserts stay idempotent).
//
// A canceled event yields a tombstone instead (no body), so the pipeline
// removes the document.
func (c *Connector) eventDocument(tenant, calendarID string, ev *event) *askerv1.Document {
	if ev.Status == statusCanceled {
		return c.tombstoneDocument(tenant, calendarID, ev)
	}

	title := strings.TrimSpace(ev.Summary)
	if title == "" {
		title = noTitle
	}
	body := eventBody(ev)

	doc := &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, nativeID(calendarID, ev.ID)),
		ConnectorId:    connectorID,
		SourceNativeId: nativeID(calendarID, ev.ID),
		Type:           askerv1.DocType_CALENDAR_EVENT,
		Title:          title,
		BodyText:       body,
		Participants:   participants(ev),
		Metadata:       metadata(calendarID, ev),
		VersionEtag:    versionEtag(ev, body),
	}
	if ts := eventTimestamps(ev); ts != nil {
		doc.Ts = ts
	}
	return doc
}

// tombstoneDocument builds the deletion Document for a canceled event:
// identity fields plus tombstone.deleted and deleted_at — no body, no chunks.
func (c *Connector) tombstoneDocument(tenant, calendarID string, ev *event) *askerv1.Document {
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, nativeID(calendarID, ev.ID)),
		ConnectorId:    connectorID,
		SourceNativeId: nativeID(calendarID, ev.ID),
		Type:           askerv1.DocType_CALENDAR_EVENT,
		VersionEtag:    versionEtag(ev, ""),
		Tombstone: &askerv1.Tombstone{
			Deleted:   true,
			DeletedAt: timestamppb.New(c.now().UTC()),
		},
	}
}

// versionEtag returns a value that changes iff the event changed: the event's
// etag when present, else a sha256 of the body so re-syncs of an etag-less
// fixture still converge. It is always non-empty (the event id seeds the hash
// for a body-less canceled event with no etag).
func versionEtag(ev *event, body string) string {
	if etag := strings.TrimSpace(ev.Etag); etag != "" {
		return etag
	}
	seed := body
	if seed == "" {
		seed = ev.ID + ":" + ev.Updated
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// eventBody is the searchable text: the description (HTML stripped to text,
// since Calendar descriptions may contain HTML) followed by the location.
func eventBody(ev *event) string {
	var parts []string
	if desc := strings.TrimSpace(stripHTML(ev.Description)); desc != "" {
		parts = append(parts, desc)
	}
	if loc := strings.TrimSpace(ev.Location); loc != "" {
		parts = append(parts, loc)
	}
	return strings.Join(parts, "\n\n")
}

// participants builds the typed participant facet: the organizer (role
// "organizer") followed by every attendee (role "attendee"). An attendee that
// is also flagged organizer keeps the "attendee" role here; the dedicated
// organizer entry already records that relationship. Resource attendees (rooms)
// are included — they are searchable as participants too.
func participants(ev *event) []*askerv1.Participant {
	var out []*askerv1.Participant
	if o := ev.Organizer; o != nil && (o.Email != "" || o.DisplayName != "") {
		out = append(out, &askerv1.Participant{
			Name:  o.DisplayName,
			Email: o.Email,
			Role:  "organizer",
		})
	}
	for _, a := range ev.Attendees {
		if a == nil || (a.Email == "" && a.DisplayName == "") {
			continue
		}
		out = append(out, &askerv1.Participant{
			Name:  a.DisplayName,
			Email: a.Email,
			Role:  "attendee",
		})
	}
	return out
}

// metadata builds the flat metadata map: source ids, link, location, start/end,
// calendar id, status, plus each attendee's responseStatus keyed by their
// email/name. Empty values are omitted.
func metadata(calendarID string, ev *event) map[string]string {
	md := make(map[string]string, 8)
	put := func(k, v string) {
		if v != "" {
			md[k] = v
		}
	}
	put("event_id", ev.ID)
	put("calendar_id", calendarID)
	put("html_link", ev.HTMLLink)
	put("location", ev.Location)
	put("status", ev.Status)
	put("start", ev.Start.value())
	put("end", ev.End.value())
	for _, a := range ev.Attendees {
		if a == nil || a.ResponseStatus == "" {
			continue
		}
		key := a.Email
		if key == "" {
			key = a.DisplayName
		}
		if key != "" {
			put("response_status:"+key, a.ResponseStatus)
		}
	}
	return md
}

// eventTimestamps maps created/updated (RFC3339) into Timestamps. A field that
// does not parse is skipped rather than failing the whole event.
func eventTimestamps(ev *event) *askerv1.Timestamps {
	created := parseRFC3339(ev.Created)
	modified := parseRFC3339(ev.Updated)
	if created == nil && modified == nil {
		return nil
	}
	return &askerv1.Timestamps{Created: created, Modified: modified}
}

// parseRFC3339 parses an RFC3339 timestamp into a protobuf Timestamp (UTC), or
// nil when empty/unparseable.
func parseRFC3339(s string) *timestamppb.Timestamp {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return timestamppb.New(t.UTC())
}

// blockTags are HTML elements whose boundary becomes a newline when tags are
// stripped, so paragraphs survive as chunking boundaries downstream.
var blockTags = map[string]bool{
	"br": true, "p": true, "div": true, "li": true, "ul": true, "ol": true,
	"tr": true, "table": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "blockquote": true,
}

// stripHTML salvages searchable text from an HTML-or-plain description: it
// removes <script>/<style> blocks, drops all tags (turning block boundaries
// into newlines), unescapes entities, and tidies whitespace. It is not a
// sanitizer; plain text passes through essentially unchanged.
func stripHTML(s string) string {
	if !strings.ContainsAny(s, "<&") {
		// No tags and no entities: plain text, nothing to strip or unescape.
		return strings.TrimSpace(s)
	}
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
