package outlookcal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// noSubjectTitle is the display title for events without a subject.
const noSubjectTitle = "(no subject)"

// graphEvent is the subset of a Microsoft Graph event resource the connector
// maps. Field names mirror the Graph JSON exactly so cassettes stay realistic.
// The "@removed" facet is present only in delta-query responses for deletions.
type graphEvent struct {
	ID                   string         `json:"id"`
	ODataEtag            string         `json:"@odata.etag"`
	ChangeKey            string         `json:"changeKey"`
	Subject              string         `json:"subject"`
	Body                 *itemBody      `json:"body"`
	BodyPreview          string         `json:"bodyPreview"`
	Location             *location      `json:"location"`
	Start                *dateTimeZone  `json:"start"`
	End                  *dateTimeZone  `json:"end"`
	IsAllDay             bool           `json:"isAllDay"`
	Organizer            *recipient     `json:"organizer"`
	Attendees            []attendee     `json:"attendees"`
	WebLink              string         `json:"webLink"`
	CreatedDateTime      string         `json:"createdDateTime"`
	LastModifiedDateTime string         `json:"lastModifiedDateTime"`
	Removed              *removedReason `json:"@removed"`
}

// removedReason is the Graph delta "@removed" facet on a deleted item.
type removedReason struct {
	Reason string `json:"reason"`
}

// itemBody is a Graph itemBody (contentType "html" or "text").
type itemBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

// location is the subset of a Graph location resource the connector maps.
type location struct {
	DisplayName string `json:"displayName"`
}

// dateTimeZone is a Graph dateTimeTimeZone value.
type dateTimeZone struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// recipient wraps a Graph emailAddress.
type recipient struct {
	EmailAddress emailAddress `json:"emailAddress"`
}

// emailAddress is a Graph emailAddress resource.
type emailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// attendee is a Graph attendee resource.
type attendee struct {
	Type         string        `json:"type"` // required | optional | resource
	Status       *responseInfo `json:"status"`
	EmailAddress emailAddress  `json:"emailAddress"`
}

// responseInfo is a Graph responseStatus resource.
type responseInfo struct {
	Response string `json:"response"` // none | accepted | tentativelyAccepted | declined | ...
	Time     string `json:"time"`
}

// isRemoved reports whether the delta item is a deletion.
func (e graphEvent) isRemoved() bool { return e.Removed != nil }

// eventDocument maps one Graph event to the canonical Document: type
// CALENDAR_EVENT, doc_id sdk.DocID("outlook-cal", event id), title from
// subject, body from body.content (HTML stripped to text) plus the location
// display name, participants from organizer + attendees, and version_etag from
// @odata.etag (changeKey fallback, then a body hash). acl is left unset: a
// personal calendar is single-owner / private-by-construction (ADR-012).
func eventDocument(tenant string, e graphEvent) *askerv1.Document {
	title := strings.TrimSpace(e.Subject)
	if title == "" {
		title = noSubjectTitle
	}

	doc := &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, e.ID),
		ConnectorId:    connectorID,
		SourceNativeId: e.ID,
		Type:           askerv1.DocType_CALENDAR_EVENT,
		Title:          title,
		BodyText:       bodyText(e),
		Participants:   participants(e),
		Metadata:       metadata(e),
		VersionEtag:    versionEtag(e),
		Ts:             timestamps(e),
	}
	return doc
}

// tombstoneDocument builds the deletion Document for one event: identity fields
// plus tombstone.deleted and deleted_at — no body, no chunks. The version_etag
// is the event's @odata.etag when the delta payload carries one (it is newer
// than any prior upsert etag); otherwise a stable "deleted:<id>" marker so the
// field is always non-empty.
func (c *Connector) tombstoneDocument(tenant string, e graphEvent) *askerv1.Document {
	etag := firstNonEmpty(e.ODataEtag, e.ChangeKey, "deleted:"+e.ID)
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, e.ID),
		ConnectorId:    connectorID,
		SourceNativeId: e.ID,
		Type:           askerv1.DocType_CALENDAR_EVENT,
		VersionEtag:    etag,
		Tombstone: &askerv1.Tombstone{
			Deleted:   true,
			DeletedAt: timestamppb.New(c.now().UTC()),
		},
	}
}

// versionEtag returns a value that changes iff the event changed: the Graph
// @odata.etag, the changeKey, or a sha256 of the mapped body as a last resort
// (so the field is never empty, keeping upserts idempotent).
func versionEtag(e graphEvent) string {
	if v := firstNonEmpty(e.ODataEtag, e.ChangeKey); v != "" {
		return v
	}
	sum := sha256.Sum256([]byte(e.Subject + "\x00" + bodyText(e)))
	return hex.EncodeToString(sum[:])
}

// bodyText extracts searchable text: the event body (HTML stripped to text
// when contentType is html), then the location display name appended on its
// own line. bodyPreview is used as a fallback when body.content is empty.
func bodyText(e graphEvent) string {
	var parts []string
	if e.Body != nil && strings.TrimSpace(e.Body.Content) != "" {
		content := e.Body.Content
		if strings.EqualFold(e.Body.ContentType, "html") {
			content = stripHTML(content)
		} else {
			content = strings.TrimSpace(content)
		}
		if content != "" {
			parts = append(parts, content)
		}
	} else if p := strings.TrimSpace(e.BodyPreview); p != "" {
		parts = append(parts, p)
	}
	if e.Location != nil {
		if loc := strings.TrimSpace(e.Location.DisplayName); loc != "" {
			parts = append(parts, loc)
		}
	}
	return strings.Join(parts, "\n")
}

// participants builds the typed people facet: the organizer (role "organizer")
// followed by each attendee (role "required"/"optional"/"resource" from the
// attendee type, defaulting to "attendee").
func participants(e graphEvent) []*askerv1.Participant {
	var out []*askerv1.Participant
	if e.Organizer != nil {
		if p := participantFromEmail(e.Organizer.EmailAddress, "organizer"); p != nil {
			out = append(out, p)
		}
	}
	for _, a := range e.Attendees {
		role := strings.TrimSpace(a.Type)
		if role == "" {
			role = "attendee"
		}
		if p := participantFromEmail(a.EmailAddress, role); p != nil {
			out = append(out, p)
		}
	}
	return out
}

// participantFromEmail maps a Graph emailAddress to a Participant, or nil when
// it carries neither a name nor an address.
func participantFromEmail(ea emailAddress, role string) *askerv1.Participant {
	name := strings.TrimSpace(ea.Name)
	addr := strings.TrimSpace(ea.Address)
	if name == "" && addr == "" {
		return nil
	}
	return &askerv1.Participant{Name: name, Email: addr, Role: role}
}

// metadata builds the flat metadata map: event id, web link, location, start
// (dateTime/timeZone), end, all-day flag, and per-attendee response statuses.
// Empty values are omitted.
func metadata(e graphEvent) map[string]string {
	md := make(map[string]string, 8)
	put := func(k, v string) {
		if v != "" {
			md[k] = v
		}
	}
	put("event_id", e.ID)
	put("web_link", e.WebLink)
	if e.Location != nil {
		put("location", strings.TrimSpace(e.Location.DisplayName))
	}
	if e.Start != nil {
		put("start", joinDateTime(e.Start))
		put("start_timezone", e.Start.TimeZone)
	}
	if e.End != nil {
		put("end", joinDateTime(e.End))
		put("end_timezone", e.End.TimeZone)
	}
	if e.IsAllDay {
		md["is_all_day"] = "true"
	}
	if statuses := attendeeStatuses(e); statuses != "" {
		md["attendee_responses"] = statuses
	}
	return md
}

// joinDateTime renders a Graph dateTimeTimeZone as "<dateTime> <timeZone>"
// (the dateTime alone when no zone is given), or "" when both are empty.
func joinDateTime(d *dateTimeZone) string {
	dt := strings.TrimSpace(d.DateTime)
	if dt == "" {
		return ""
	}
	if tz := strings.TrimSpace(d.TimeZone); tz != "" {
		return dt + " " + tz
	}
	return dt
}

// attendeeStatuses renders a compact JSON object of address->response so the
// per-attendee response.response is captured in metadata without a nested
// proto. An attendee with no resolvable address or status is skipped.
func attendeeStatuses(e graphEvent) string {
	statuses := map[string]string{}
	for _, a := range e.Attendees {
		addr := strings.TrimSpace(a.EmailAddress.Address)
		if addr == "" || a.Status == nil {
			continue
		}
		if resp := strings.TrimSpace(a.Status.Response); resp != "" {
			statuses[addr] = resp
		}
	}
	if len(statuses) == 0 {
		return ""
	}
	raw, err := json.Marshal(statuses)
	if err != nil {
		return ""
	}
	return string(raw)
}

// timestamps maps createdDateTime/lastModifiedDateTime (RFC 3339) to
// ts.created / ts.modified. ts.ingested is intentionally left unset — the hub
// stamps it. Returns nil when neither timestamp parses.
func timestamps(e graphEvent) *askerv1.Timestamps {
	var ts askerv1.Timestamps
	any := false
	if t, ok := parseGraphTime(e.CreatedDateTime); ok {
		ts.Created = timestamppb.New(t)
		any = true
	}
	if t, ok := parseGraphTime(e.LastModifiedDateTime); ok {
		ts.Modified = timestamppb.New(t)
		any = true
	}
	if !any {
		return nil
	}
	return &ts
}

// graphTimeLayouts are the timestamp formats Graph emits for event datetimes.
// Graph uses RFC 3339 with a "Z" on created/lastModified, but event
// start/end use a fractional-second form without a zone; we accept both.
var graphTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.0000000",
	"2006-01-02T15:04:05",
}

// parseGraphTime parses a Graph timestamp string, returning the UTC time and
// whether parsing succeeded.
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

// blockTags are HTML elements whose boundary becomes a newline when tags are
// stripped, so paragraphs survive as chunking boundaries downstream.
var blockTags = map[string]bool{
	"br": true, "p": true, "div": true, "li": true, "ul": true, "ol": true,
	"tr": true, "table": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "blockquote": true,
}

// stripHTML is the crude text/html-to-text fallback for Graph event bodies: it
// drops <script>/<style> blocks, removes all tags (turning block boundaries
// into newlines), unescapes entities, and tidies whitespace. It is not a
// sanitizer — just enough to salvage searchable text.
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
