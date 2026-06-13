package outlookcal

import (
	"fmt"
	"strings"

	"github.com/asker/asker/connectors/sdk"
)

// Cursor wire format (documented in the package comment):
//
//	"delta:<deltaLink>"   steady-state incremental cursor (full @odata.deltaLink URL)
//	"page:<nextLink>"     mid-backfill checkpoint (full @odata.nextLink URL)
const (
	deltaPrefix = "delta:"
	pagePrefix  = "page:"
)

// cursorState is a parsed cursor.
type cursorState struct {
	// link is the Graph URL to resume from (a deltaLink or a nextLink).
	link string
	// backfill is true for the "page:<nextLink>" shape (resume the backfill);
	// false for the "delta:<deltaLink>" shape (resume the delta query).
	backfill bool
}

// deltaCursor renders the steady-state cursor from a Graph @odata.deltaLink.
func deltaCursor(deltaLink string) sdk.Cursor {
	return sdk.Cursor(deltaPrefix + deltaLink)
}

// pageCursor renders a mid-backfill checkpoint cursor from a Graph
// @odata.nextLink.
func pageCursor(nextLink string) sdk.Cursor {
	return sdk.Cursor(pagePrefix + nextLink)
}

// parseCursor decodes either cursor shape. Anything this connector did not
// produce is an error; IncrementalSync maps that to sdk.ErrCursorExpired so the
// hub recovers with a full sync instead of looping on a poison cursor.
func parseCursor(cur sdk.Cursor) (cursorState, error) {
	s := string(cur)
	if link, ok := strings.CutPrefix(s, deltaPrefix); ok {
		if link == "" {
			return cursorState{}, fmt.Errorf("outlook-cal: delta cursor has an empty link")
		}
		return cursorState{link: link, backfill: false}, nil
	}
	if link, ok := strings.CutPrefix(s, pagePrefix); ok {
		if link == "" {
			return cursorState{}, fmt.Errorf("outlook-cal: page cursor has an empty link")
		}
		return cursorState{link: link, backfill: true}, nil
	}
	return cursorState{}, fmt.Errorf("outlook-cal: cursor %q has an unknown prefix", s)
}
