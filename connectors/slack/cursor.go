package slack

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/asker/asker/connectors/sdk"
)

// cursorState is the per-channel high-water mark: channel id -> the latest
// message ts the connector has emitted for that channel. Slack ts values are
// "seconds.micros" strings, lexicographically orderable, so the newest ts is
// the string max.
type cursorState map[string]string

// parseCursor decodes a connector cursor. The empty cursor (no position yet)
// decodes to an empty map. Anything that is not the connector's own JSON
// object shape is rejected; IncrementalSync maps that to sdk.ErrCursorExpired
// so the hub recovers with a full sync instead of looping on a poison cursor.
func parseCursor(cur sdk.Cursor) (cursorState, error) {
	st := cursorState{}
	s := string(cur)
	if s == "" {
		return st, nil
	}
	if err := json.Unmarshal([]byte(s), &st); err != nil {
		return nil, fmt.Errorf("slack: cursor %q is not a channel->ts JSON object: %w", s, err)
	}
	return st, nil
}

// encode renders the cursor as deterministic JSON (channel ids sorted) so the
// same state always produces the same string — important for cursor equality
// assertions and stable hub persistence.
func (st cursorState) encode() sdk.Cursor {
	if len(st) == 0 {
		return sdk.Cursor("{}")
	}
	keys := make([]string, 0, len(st))
	for k := range st {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b []byte
	b = append(b, '{')
	for i, k := range keys {
		if i > 0 {
			b = append(b, ',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(st[k])
		b = append(b, kb...)
		b = append(b, ':')
		b = append(b, vb...)
	}
	b = append(b, '}')
	return sdk.Cursor(b)
}

// advance records ts as the channel's high-water mark when it is newer
// (lexicographically greater) than the current one. Slack ts strings are
// fixed-width "seconds.micros", so string comparison is chronological.
func (st cursorState) advance(channel, ts string) {
	if ts == "" {
		return
	}
	if cur, ok := st[channel]; !ok || ts > cur {
		st[channel] = ts
	}
}
