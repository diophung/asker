package ical

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/asker/asker/connectors/sdk"
)

// Cursor wire format.
//
// A feed has no incremental API: every sync re-fetches the whole .ics body. The
// cursor therefore carries everything IncrementalSync needs to (a) short-circuit
// when nothing changed and (b) diff the new feed against the previous one without
// re-fetching it:
//
//   - hash       — sha256 of the previous feed body (the content fingerprint).
//     When the new body hashes the same, the feed is byte-identical
//     and IncrementalSync returns immediately with no emissions.
//   - max_dtstamp — the largest DTSTAMP across the previous feed's events, kept
//     as the human-facing freshness marker the build contract asks
//     for (it is part of the cursor identity but the diff is driven
//     by the per-UID fingerprints below).
//   - events     — UID -> version_etag for every event in the previous feed, so
//     IncrementalSync can tell new/changed UIDs (upserts) from those
//     that vanished (tombstones) precisely.
//
// The whole struct is JSON-encoded and base64url-wrapped into a single opaque
// sdk.Cursor string. The hub stores and replays it verbatim; only this connector
// interprets it.
type cursorState struct {
	Hash       string            `json:"hash"`
	MaxDTStamp string            `json:"max_dtstamp"`
	Events     map[string]string `json:"events"`
}

// cursorPrefix tags the encoded cursor so a value this connector did not produce
// is rejected (mapped to sdk.ErrCursorExpired by IncrementalSync) rather than
// mis-parsed.
const cursorPrefix = "ical1:"

// buildCursorState computes the cursor state for a parsed feed: its body hash,
// the max DTSTAMP across events, and the UID -> version_etag fingerprint map. A
// VEVENT without a UID is skipped (it cannot be tracked across syncs).
func buildCursorState(body string, events []*vevent) cursorState {
	st := cursorState{
		Hash:   feedHash(body),
		Events: make(map[string]string, len(events)),
	}
	for _, ev := range events {
		uid := strings.TrimSpace(ev.value("UID"))
		if uid == "" {
			continue
		}
		st.Events[uid] = versionEtag(ev)
		if ds := dtstamp(ev); ds > st.MaxDTStamp {
			st.MaxDTStamp = ds
		}
	}
	return st
}

// encode renders the cursor state as the opaque sdk.Cursor string.
func (s cursorState) encode() sdk.Cursor {
	raw, err := json.Marshal(s)
	if err != nil {
		// cursorState is plain strings/maps; marshaling cannot fail. Fall back
		// to a hash-only cursor so a sync still advances.
		return sdk.Cursor(cursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(`{"hash":"`+s.Hash+`"}`)))
	}
	return sdk.Cursor(cursorPrefix + base64.RawURLEncoding.EncodeToString(raw))
}

// parseCursor decodes a cursor produced by encode. Anything this connector did
// not produce is an error; IncrementalSync maps that to sdk.ErrCursorExpired so
// the hub recovers with a full sync instead of looping on a poison cursor. The
// empty cursor (no position yet) is also an error here — IncrementalSync should
// never be called without a prior FullSync cursor, and treating it as expired
// makes the hub restart cleanly.
func parseCursor(cur sdk.Cursor) (cursorState, error) {
	s := string(cur)
	enc, ok := strings.CutPrefix(s, cursorPrefix)
	if !ok {
		return cursorState{}, fmt.Errorf("ical: cursor has an unrecognized prefix")
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return cursorState{}, fmt.Errorf("ical: cursor is not valid base64: %w", err)
	}
	var st cursorState
	if err := json.Unmarshal(raw, &st); err != nil {
		return cursorState{}, fmt.Errorf("ical: cursor is not valid JSON: %w", err)
	}
	if st.Events == nil {
		st.Events = map[string]string{}
	}
	return st, nil
}

// sortedUIDs returns the UID keys of a fingerprint map in deterministic order,
// used so disappeared-event tombstones are emitted in a stable sequence.
func sortedUIDs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for uid := range m {
		out = append(out, uid)
	}
	sort.Strings(out)
	return out
}
