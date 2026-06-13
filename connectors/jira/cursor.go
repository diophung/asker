package jira

import (
	"fmt"
	"strings"
	"time"

	"github.com/asker/asker/connectors/sdk"
)

// Cursor wire format (documented in the package comment):
//
//	"updated:<yyyy/MM/dd HH:mm>|key:<lastIssueKey>"
//
// <updated> is the JQL-formatted updated time of the newest issue seen so far,
// and <key> is that issue's key, used to dedupe the inclusive JQL boundary on
// the next incremental pass.
const (
	cursorUpdatedPrefix = "updated:"
	cursorKeySep        = "|key:"

	// jqlTimeLayout is Jira's documented JQL date-time format, minute
	// precision. JQL has no second granularity, so this is the finest bound
	// "updated >=" accepts.
	jqlTimeLayout = "2006/01/02 15:04"
)

// cursorState is a parsed cursor.
type cursorState struct {
	// updatedJQL is the JQL-formatted "updated >=" lower bound. Empty means
	// "from the beginning" (FullSync's starting position).
	updatedJQL string
	// boundaryKey is the issue key at exactly updatedJQL; it is skipped on the
	// next pass to avoid re-emitting the boundary issue (the JQL bound is
	// inclusive and minute-granular).
	boundaryKey string
}

// makeCursor renders a cursor from an issue's updated time (JQL-formatted) and
// key.
func makeCursor(updatedJQL, key string) sdk.Cursor {
	return sdk.Cursor(cursorUpdatedPrefix + updatedJQL + cursorKeySep + key)
}

// cursorForIssue renders the cursor that points at iss as the newest issue
// seen. It returns ("", false) when the issue's updated time does not parse,
// so the caller keeps the previous cursor rather than emitting a broken one.
func cursorForIssue(iss *issue) (sdk.Cursor, bool) {
	t, ok := parseJiraTime(iss.Fields.Updated)
	if !ok {
		return "", false
	}
	return makeCursor(t.Format(jqlTimeLayout), iss.Key), true
}

// parseCursor decodes a cursor. The empty cursor is the valid "from the
// beginning" position. Any other shape this connector did not produce is an
// error; IncrementalSync maps that to sdk.ErrCursorExpired so the hub recovers
// with a full sync instead of looping on a poison cursor.
func parseCursor(cur sdk.Cursor) (cursorState, error) {
	s := string(cur)
	if s == "" {
		return cursorState{}, nil
	}
	rest, ok := strings.CutPrefix(s, cursorUpdatedPrefix)
	if !ok {
		return cursorState{}, fmt.Errorf("jira: cursor %q does not start with %q", s, cursorUpdatedPrefix)
	}
	updated, key, found := strings.Cut(rest, cursorKeySep)
	if !found {
		return cursorState{}, fmt.Errorf("jira: cursor %q is missing the %q separator", s, cursorKeySep)
	}
	if updated == "" {
		return cursorState{}, fmt.Errorf("jira: cursor %q has an empty updated bound", s)
	}
	if _, err := time.Parse(jqlTimeLayout, updated); err != nil {
		return cursorState{}, fmt.Errorf("jira: cursor %q has a malformed updated bound: %w", s, err)
	}
	return cursorState{updatedJQL: updated, boundaryKey: key}, nil
}

// jql builds the search JQL for a cursor state. The empty state backfills the
// whole site in updated-ascending order; a bounded state scans forward from
// the cursor's updated time.
func (st cursorState) jql() string {
	if st.updatedJQL == "" {
		return "order by updated asc"
	}
	return fmt.Sprintf("updated >= '%s' order by updated asc", st.updatedJQL)
}
