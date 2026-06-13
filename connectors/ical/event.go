package ical

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

const (
	// statusCancelled is the VEVENT STATUS value that denotes a deletion; it
	// maps to a tombstone on incremental sync. RFC 5545 spells it with the
	// British double-l, so the constant value matches verbatim (the nolint keeps
	// the US-locale spell check off the literal).
	statusCancelled = "CANCELLED" //nolint:misspell // RFC 5545 STATUS value, spelled verbatim

	// noTitle is the display title for an event with no SUMMARY.
	noTitle = "(no title)"
)

// nativeID is the source-native id used in the doc_id: "<instanceID>:<UID>".
// Scoping by the hub's stable per-instance id (rather than the raw feed URL)
// keeps doc_ids stable across feed-URL rotations — many providers embed a
// rotating private token in the feed URL, so scoping by URL would orphan every
// document the moment the token rotated. The instance id is stable for the life
// of the configured instance and still keeps the same UID distinct across two
// feeds a tenant connects (they are two separate instances).
func nativeID(instanceID, uid string) string {
	return instanceID + ":" + uid
}

// eventDocument maps one VEVENT to the canonical Document: type CALENDAR_EVENT,
// doc_id sdk.DocID("ical", instanceID+":"+UID), title from SUMMARY, body from
// DESCRIPTION + LOCATION, participants from ORGANIZER + ATTENDEE,
// ts.created/modified from CREATED/LAST-MODIFIED, and a version_etag that changes
// iff the event changed (SEQUENCE, else LAST-MODIFIED, else a sha256 of the
// event block).
//
// A canceled event (the statusCancelled STATUS value) yields a tombstone
// instead (no body) so the pipeline removes the document.
func (c *Connector) eventDocument(tenant, instanceID string, ev *vevent) *askerv1.Document {
	uid := strings.TrimSpace(ev.value("UID"))

	if strings.EqualFold(strings.TrimSpace(ev.value("STATUS")), statusCancelled) {
		return c.tombstoneDocument(tenant, instanceID, ev, uid)
	}

	title := strings.TrimSpace(ev.value("SUMMARY"))
	if title == "" {
		title = noTitle
	}

	doc := &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, nativeID(instanceID, uid)),
		ConnectorId:    connectorID,
		SourceNativeId: nativeID(instanceID, uid),
		Type:           askerv1.DocType_CALENDAR_EVENT,
		Title:          title,
		BodyText:       eventBody(ev),
		Participants:   participants(ev),
		Metadata:       metadata(ev),
		VersionEtag:    versionEtag(ev),
	}
	if ts := eventTimestamps(ev); ts != nil {
		doc.Ts = ts
	}
	return doc
}

// tombstoneDocument builds the deletion Document for a canceled/disappeared
// event: identity fields plus tombstone.deleted and deleted_at — no body.
func (c *Connector) tombstoneDocument(tenant, instanceID string, ev *vevent, uid string) *askerv1.Document {
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, nativeID(instanceID, uid)),
		ConnectorId:    connectorID,
		SourceNativeId: nativeID(instanceID, uid),
		Type:           askerv1.DocType_CALENDAR_EVENT,
		VersionEtag:    versionEtag(ev),
		Tombstone: &askerv1.Tombstone{
			Deleted:   true,
			DeletedAt: timestamppb.New(c.now().UTC()),
		},
	}
}

// disappearedTombstone builds a tombstone for a UID that was present at the
// cursor baseline but is absent from the current feed. We only know its UID, so
// the version_etag is a stable hash of the UID; the deleted_at marks the poll.
func (c *Connector) disappearedTombstone(tenant, instanceID, uid string) *askerv1.Document {
	sum := sha256.Sum256([]byte("deleted:" + uid))
	return &askerv1.Document{
		TenantId:       tenant,
		DocId:          sdk.DocID(connectorID, nativeID(instanceID, uid)),
		ConnectorId:    connectorID,
		SourceNativeId: nativeID(instanceID, uid),
		Type:           askerv1.DocType_CALENDAR_EVENT,
		VersionEtag:    hex.EncodeToString(sum[:]),
		Tombstone: &askerv1.Tombstone{
			Deleted:   true,
			DeletedAt: timestamppb.New(c.now().UTC()),
		},
	}
}

// versionEtag returns a value that changes iff the event changed: SEQUENCE when
// present, else LAST-MODIFIED, else a sha256 of the raw event block. It is always
// non-empty.
func versionEtag(ev *vevent) string {
	if seq := strings.TrimSpace(ev.value("SEQUENCE")); seq != "" {
		return "seq:" + seq
	}
	if mod := strings.TrimSpace(ev.value("LAST-MODIFIED")); mod != "" {
		return "mod:" + mod
	}
	sum := sha256.Sum256([]byte(ev.raw))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// eventBody is the searchable text: the DESCRIPTION followed by the LOCATION.
func eventBody(ev *vevent) string {
	var parts []string
	if desc := strings.TrimSpace(ev.value("DESCRIPTION")); desc != "" {
		parts = append(parts, desc)
	}
	if loc := strings.TrimSpace(ev.value("LOCATION")); loc != "" {
		parts = append(parts, loc)
	}
	return strings.Join(parts, "\n\n")
}

// participants builds the typed participant facet: the ORGANIZER (role
// "organizer") followed by every ATTENDEE (role from the ROLE param, default
// "attendee"). A CN= parameter becomes the display name; a "mailto:" value
// becomes the email.
func participants(ev *vevent) []*askerv1.Participant {
	var out []*askerv1.Participant
	if org, ok := ev.get("ORGANIZER"); ok {
		if p := actor(org, "organizer"); p != nil {
			out = append(out, p)
		}
	}
	for _, att := range ev.getAll("ATTENDEE") {
		role := "attendee"
		if r, ok := att.param("ROLE"); ok && strings.TrimSpace(r) != "" {
			role = strings.ToLower(strings.TrimSpace(r))
		}
		if p := actor(att, role); p != nil {
			out = append(out, p)
		}
	}
	return out
}

// actor maps an ORGANIZER/ATTENDEE property to a Participant: CN= -> name,
// mailto: value -> email. Returns nil when neither is present.
func actor(p property, role string) *askerv1.Participant {
	name := ""
	if cn, ok := p.param("CN"); ok {
		name = strings.TrimSpace(cn)
	}
	email := mailtoAddr(p.value)
	if name == "" && email == "" {
		return nil
	}
	return &askerv1.Participant{Name: name, Email: email, Role: role}
}

// mailtoAddr extracts the address from a "mailto:alice@example.com" CAL-ADDRESS
// value (the scheme is case-insensitive per RFC 5545). A value without the
// mailto scheme returns "".
func mailtoAddr(v string) string {
	v = strings.TrimSpace(v)
	if rest, ok := cutPrefixFold(v, "mailto:"); ok {
		return strings.TrimSpace(rest)
	}
	return ""
}

// cutPrefixFold is strings.CutPrefix with a case-insensitive prefix test.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// metadata builds the flat metadata map: uid, location, dtstart, dtend, status,
// url, sequence, plus the RRULE verbatim when present (recurrence expansion is
// deferred — see parse.go / README). Empty values are omitted.
func metadata(ev *vevent) map[string]string {
	md := make(map[string]string, 8)
	put := func(k, v string) {
		if v = strings.TrimSpace(v); v != "" {
			md[k] = v
		}
	}
	put("uid", ev.value("UID"))
	put("location", ev.value("LOCATION"))
	put("dtstart", ev.value("DTSTART"))
	put("dtend", ev.value("DTEND"))
	put("status", ev.value("STATUS"))
	put("url", ev.value("URL"))
	put("sequence", ev.value("SEQUENCE"))
	put("rrule", ev.value("RRULE"))
	// Record whether DTSTART is all-day (VALUE=DATE) so downstream display can
	// distinguish it from a timed event.
	if p, ok := ev.get("DTSTART"); ok {
		if v, ok := p.param("VALUE"); ok && strings.EqualFold(strings.TrimSpace(v), "DATE") {
			md["all_day"] = "true"
		}
	}
	return md
}

// eventTimestamps maps CREATED/LAST-MODIFIED into Timestamps. A value that does
// not parse is skipped rather than failing the whole event.
func eventTimestamps(ev *vevent) *askerv1.Timestamps {
	created := parseICalTime(ev.value("CREATED"))
	modified := parseICalTime(ev.value("LAST-MODIFIED"))
	if created == nil && modified == nil {
		return nil
	}
	return &askerv1.Timestamps{Created: created, Modified: modified}
}

// icalLayouts are the date-time forms RFC 5545 uses for UTC/floating timestamps
// in CREATED/LAST-MODIFIED/DTSTAMP (the "Z" UTC form is by far the most common in
// feeds). Local-time-with-TZID forms are parsed as floating wall-clock.
var icalLayouts = []string{
	"20060102T150405Z",
	"20060102T150405",
	"20060102",
}

// parseICalTime parses an iCalendar date or date-time into a protobuf Timestamp
// (UTC), or nil when empty/unparseable. A trailing "Z" denotes UTC; a value
// without one is treated as UTC for the purposes of the (coarse) created/modified
// timestamps the pipeline records.
func parseICalTime(s string) *timestamppb.Timestamp {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range icalLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return timestamppb.New(t.UTC())
		}
	}
	return nil
}

// dtstamp returns the event's DTSTAMP value (the source's "this iteration was
// generated at" marker), used to compute the cursor's max-DTSTAMP component. It
// falls back to LAST-MODIFIED then CREATED when DTSTAMP is absent.
func dtstamp(ev *vevent) string {
	for _, name := range []string{"DTSTAMP", "LAST-MODIFIED", "CREATED"} {
		if v := strings.TrimSpace(ev.value(name)); v != "" {
			return v
		}
	}
	return ""
}
