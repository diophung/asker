package gdrive

import (
	"fmt"
	"strings"

	"github.com/asker/asker/connectors/sdk"
)

// Cursor wire format (documented in the package comment):
//
//	"page:<changesPageToken>"                       steady-state incremental cursor
//	"start:<changesStartToken>|files:<pageToken>"   mid-backfill checkpoint
const (
	cursorPagePrefix  = "page:"
	cursorStartPrefix = "start:"
	cursorFilesSep    = "|files:"
)

// cursorState is a parsed cursor.
type cursorState struct {
	// changesToken is the Drive changes page token IncrementalSync replays
	// from (steady state) or the start token captured before a backfill.
	changesToken string
	// filesPageToken is the next files.list page to process; set only for a
	// mid-backfill checkpoint cursor.
	filesPageToken string
	// backfill is true for the "start:...|files:..." shape.
	backfill bool
}

// incrementalCursor renders the steady-state cursor from a changes page token.
func incrementalCursor(changesToken string) sdk.Cursor {
	return sdk.Cursor(cursorPagePrefix + changesToken)
}

// backfillCursor renders a mid-backfill checkpoint cursor.
func backfillCursor(startToken, filesPageToken string) sdk.Cursor {
	return sdk.Cursor(cursorStartPrefix + startToken + cursorFilesSep + filesPageToken)
}

// parseCursor decodes either cursor shape. Anything this connector did not
// produce is an error; IncrementalSync maps that to sdk.ErrCursorExpired so the
// hub recovers with a full sync instead of looping on a poison cursor.
func parseCursor(cur sdk.Cursor) (cursorState, error) {
	s := string(cur)
	if s == "" {
		return cursorState{}, fmt.Errorf("gdrive: empty cursor")
	}

	if rest, ok := strings.CutPrefix(s, cursorStartPrefix); ok {
		start, page, found := strings.Cut(rest, cursorFilesSep)
		if !found {
			return cursorState{}, fmt.Errorf("gdrive: backfill cursor %q is missing the %q separator", s, cursorFilesSep)
		}
		if start == "" {
			return cursorState{}, fmt.Errorf("gdrive: backfill cursor %q has an empty start token", s)
		}
		if page == "" {
			return cursorState{}, fmt.Errorf("gdrive: backfill cursor %q has an empty files page token", s)
		}
		return cursorState{changesToken: start, filesPageToken: page, backfill: true}, nil
	}

	if token, ok := strings.CutPrefix(s, cursorPagePrefix); ok {
		if token == "" {
			return cursorState{}, fmt.Errorf("gdrive: cursor %q has an empty page token", s)
		}
		return cursorState{changesToken: token}, nil
	}

	return cursorState{}, fmt.Errorf("gdrive: cursor %q has an unrecognized prefix", s)
}
