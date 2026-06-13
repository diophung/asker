package whatsappexport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/asker/asker/connectors/sdk"
)

// Cursor format
//
// A cursor is "<count>:<prefixHash>" where:
//
//   - count is the number of messages parsed in the export that produced it, and
//   - prefixHash is sha256(perMessageEtag[0] + "\n" + … + perMessageEtag[count-1])
//     in lowercase hex — a fingerprint of the first count messages.
//
// WhatsApp's "Export chat" is append-only in normal use: a later, larger export
// of the same chat shares the earlier export's prefix verbatim. So on an
// incremental pass the connector re-fetches the export, parses it, and:
//
//   - same count, same prefixHash  -> no change (return the same cursor, emit
//     nothing);
//   - grew, same prefixHash on the first <count> messages -> emit only the new
//     tail (messages [count:]) and advance the cursor;
//   - the prefix changed (a re-export that edited/deleted earlier messages, or a
//     different chat uploaded to the same instance) -> sdk.ErrCursorExpired, so
//     the hub clears the cursor and re-runs FullSync (full re-import).
//
// The empty cursor means "no position yet" (FullSync starts from zero).

// makeCursor builds the cursor for the first n messages of msgs (n == len(msgs)
// for a complete sync). It hashes the per-message version etags so a changed
// message anywhere in the prefix changes the cursor.
func makeCursor(msgs []message, n int) sdk.Cursor {
	if n > len(msgs) {
		n = len(msgs)
	}
	h := sha256.New()
	for i := 0; i < n; i++ {
		_, _ = h.Write([]byte(messageEtag(msgs[i])))
		_, _ = h.Write([]byte{'\n'})
	}
	prefix := hex.EncodeToString(h.Sum(nil))
	return sdk.Cursor(strconv.Itoa(n) + ":" + prefix)
}

// parsedCursor is a decoded cursor.
type parsedCursor struct {
	count      int
	prefixHash string
}

// parseCursor decodes "<count>:<prefixHash>". An empty cursor decodes to
// {count:0} (start from the beginning). A malformed cursor is an error, which
// IncrementalSync maps to sdk.ErrCursorExpired (the hub then full-re-imports).
func parseCursor(cur sdk.Cursor) (parsedCursor, error) {
	s := string(cur)
	if s == "" {
		return parsedCursor{}, nil
	}
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		return parsedCursor{}, fmt.Errorf("whatsapp-export: cursor %q is not \"<count>:<hash>\"", s)
	}
	n, err := strconv.Atoi(s[:idx])
	if err != nil || n < 0 {
		return parsedCursor{}, fmt.Errorf("whatsapp-export: cursor %q has a bad count", s)
	}
	hash := s[idx+1:]
	if len(hash) != 64 {
		return parsedCursor{}, fmt.Errorf("whatsapp-export: cursor %q has a bad prefix hash", s)
	}
	return parsedCursor{count: n, prefixHash: hash}, nil
}

// prefixHashOf returns the prefix hash component of makeCursor(msgs, n) without
// allocating the full cursor string — used to compare a re-fetched export's
// prefix against the cursor the hub replayed.
func prefixHashOf(msgs []message, n int) string {
	c := makeCursor(msgs, n)
	s := string(c)
	if idx := strings.IndexByte(s, ':'); idx >= 0 {
		return s[idx+1:]
	}
	return ""
}
