package gmail

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/asker/asker/connectors/sdk"
)

// Cursor wire format (documented in the package comment):
//
//	"history:<historyId>"                 steady-state incremental cursor
//	"history:<historyId>|page:<token>"    mid-backfill checkpoint
const (
	cursorPrefix  = "history:"
	cursorPageSep = "|page:"
)

// cursorState is a parsed cursor.
type cursorState struct {
	historyID uint64
	pageToken string // next messages.list page; only set when backfill
	backfill  bool   // true for the "history:<id>|page:<token>" shape
}

// incrementalCursor renders the steady-state cursor.
func incrementalCursor(historyID uint64) sdk.Cursor {
	return sdk.Cursor(cursorPrefix + strconv.FormatUint(historyID, 10))
}

// backfillCursor renders a mid-backfill checkpoint cursor.
func backfillCursor(historyID uint64, pageToken string) sdk.Cursor {
	return sdk.Cursor(cursorPrefix + strconv.FormatUint(historyID, 10) + cursorPageSep + pageToken)
}

// parseCursor decodes either cursor shape. Anything this connector did not
// produce is an error; IncrementalSync maps that to sdk.ErrCursorExpired so
// the hub recovers with a full sync instead of looping on a poison cursor.
func parseCursor(cur sdk.Cursor) (cursorState, error) {
	s := string(cur)
	rest, ok := strings.CutPrefix(s, cursorPrefix)
	if !ok {
		return cursorState{}, fmt.Errorf("gmail: cursor %q does not start with %q", s, cursorPrefix)
	}

	var st cursorState
	if idStr, token, found := strings.Cut(rest, cursorPageSep); found {
		if token == "" {
			return cursorState{}, fmt.Errorf("gmail: backfill cursor %q has an empty page token", s)
		}
		st.backfill = true
		st.pageToken = token
		rest = idStr
	}

	id, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return cursorState{}, fmt.Errorf("gmail: cursor %q has a malformed history id: %w", s, err)
	}
	st.historyID = id
	return st, nil
}
