package gcal

import (
	"fmt"
	"strings"

	"github.com/asker/asker/connectors/sdk"
)

// Cursor wire format (documented in the package comment):
//
//	"sync:<syncToken>"   steady-state incremental cursor (events.list?syncToken=)
//	"page:<pageToken>"   mid-backfill checkpoint (resume events.list?pageToken=)
const (
	syncPrefix = "sync:"
	pagePrefix = "page:"
)

// cursorState is a parsed cursor. Exactly one of syncToken / pageToken is set.
type cursorState struct {
	syncToken string
	pageToken string
	backfill  bool // true for the "page:<token>" shape
}

// syncCursor renders the steady-state cursor from a calendar sync token.
func syncCursor(syncToken string) sdk.Cursor {
	return sdk.Cursor(syncPrefix + syncToken)
}

// pageCursor renders a mid-backfill checkpoint cursor from a page token.
func pageCursor(pageToken string) sdk.Cursor {
	return sdk.Cursor(pagePrefix + pageToken)
}

// parseCursor decodes either cursor shape. Anything this connector did not
// produce is an error; IncrementalSync maps that to sdk.ErrCursorExpired so the
// hub recovers with a full sync instead of looping on a poison cursor.
func parseCursor(cur sdk.Cursor) (cursorState, error) {
	s := string(cur)
	if token, ok := strings.CutPrefix(s, pagePrefix); ok {
		if token == "" {
			return cursorState{}, fmt.Errorf("gcal: backfill cursor %q has an empty page token", s)
		}
		return cursorState{pageToken: token, backfill: true}, nil
	}
	if token, ok := strings.CutPrefix(s, syncPrefix); ok {
		if token == "" {
			return cursorState{}, fmt.Errorf("gcal: sync cursor %q has an empty sync token", s)
		}
		return cursorState{syncToken: token}, nil
	}
	return cursorState{}, fmt.Errorf("gcal: cursor %q has an unrecognized prefix", s)
}
