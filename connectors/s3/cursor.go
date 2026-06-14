package s3

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/asker/asker/connectors/sdk"
)

// cursorState is the parsed JSON cursor (wire format documented in the package
// comment). It carries everything IncrementalSync needs to reconcile a fresh
// re-list against the previous pass without a source change feed.
type cursorState struct {
	// LastKey is the lexicographically-last key listed so far. A mid-backfill
	// checkpoint replays it as ListObjects StartAfter to resume. Empty once a
	// pass has completed.
	LastKey string `json:"k,omitempty"`
	// MaxModified is the maximum LastModified seen across the keyset, as
	// RFC3339Nano. IncrementalSync re-emits objects strictly newer than this.
	MaxModified string `json:"m,omitempty"`
	// Keys is the compact, sorted set of keys present at the end of the pass.
	// IncrementalSync diffs it against the fresh keyset to find deletions.
	Keys []string `json:"keys,omitempty"`
}

// maxModifiedTime parses MaxModified; a missing/blank value is the zero time
// (so every listed object counts as newer on the first incremental pass).
func (s cursorState) maxModifiedTime() time.Time {
	if s.MaxModified == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s.MaxModified)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// keySet returns Keys as a set for O(1) membership during the deletion diff.
func (s cursorState) keySet() map[string]struct{} {
	set := make(map[string]struct{}, len(s.Keys))
	for _, k := range s.Keys {
		set[k] = struct{}{}
	}
	return set
}

// completedCursor renders the steady-state cursor at the end of a completed
// pass: no LastKey (nothing left to resume), the max LastModified, and the full
// sorted keyset. Keys are sorted for a stable, diff-able cursor.
func completedCursor(maxMod time.Time, keys []string) sdk.Cursor {
	sort.Strings(keys)
	st := cursorState{Keys: keys}
	if !maxMod.IsZero() {
		st.MaxModified = maxMod.UTC().Format(time.RFC3339Nano)
	}
	return encodeCursor(st)
}

// checkpointCursor renders a mid-backfill checkpoint: the last key listed so a
// resume continues past it, plus the progress accumulated so far.
func checkpointCursor(lastKey string, maxMod time.Time, keys []string) sdk.Cursor {
	sort.Strings(keys)
	st := cursorState{LastKey: lastKey, Keys: keys}
	if !maxMod.IsZero() {
		st.MaxModified = maxMod.UTC().Format(time.RFC3339Nano)
	}
	return encodeCursor(st)
}

// encodeCursor marshals a cursorState to the opaque sdk.Cursor wire form.
func encodeCursor(st cursorState) sdk.Cursor {
	raw, err := json.Marshal(st)
	if err != nil {
		// cursorState is plain JSON-able data; marshal cannot fail in practice.
		return ""
	}
	return sdk.Cursor(raw)
}

// parseCursor decodes a cursor this connector produced. Anything else is an
// error; IncrementalSync maps that to sdk.ErrCursorExpired so the hub restarts
// a full sync rather than looping on a poison cursor.
func parseCursor(cur sdk.Cursor) (cursorState, error) {
	s := string(cur)
	if s == "" {
		return cursorState{}, fmt.Errorf("s3: empty cursor")
	}
	var st cursorState
	if err := json.Unmarshal([]byte(s), &st); err != nil {
		return cursorState{}, fmt.Errorf("s3: cursor is not valid JSON: %w", err)
	}
	return st, nil
}
