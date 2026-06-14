package confluence

import (
	"time"

	"github.com/asker/asker/connectors/sdk"
)

// cqlTimeLayout is the timestamp form Confluence Query Language compares
// lastModified against: "yyyy-MM-dd HH:mm". CQL does not accept seconds or a
// timezone offset in a lastModified literal, so the cursor is rendered at
// minute granularity. The asc ordering plus a >= comparison makes re-running
// from the same minute idempotent (the (doc_id, version_etag) upsert dedupes).
const cqlTimeLayout = "2006-01-02 15:04"

// renderCursor formats a modified time as the CQL-comparable cursor string.
func renderCursor(t time.Time) sdk.Cursor {
	return sdk.Cursor(t.UTC().Format(cqlTimeLayout))
}

// parseCursor parses a cursor previously produced by renderCursor. The empty
// cursor (no position yet) maps to the zero time, ok=true, so a first
// IncrementalSync with no prior cursor scans from the beginning. A non-empty
// cursor that does not parse returns ok=false; callers fall back to a full
// window rather than failing, because CQL-by-timestamp cursors never expire at
// the source (so ErrCursorExpired is never returned — see IncrementalSync).
func parseCursor(cur sdk.Cursor) (time.Time, bool) {
	s := string(cur)
	if s == "" {
		return time.Time{}, true
	}
	if t, err := time.Parse(cqlTimeLayout, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}
