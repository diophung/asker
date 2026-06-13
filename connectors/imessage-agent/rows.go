package main

import (
	"encoding/json"
	"fmt"

	"github.com/asker/asker/connectors/imessage-agent/internal/imsg"
)

// chatQuery is the SQL run against chat.db. It joins message → handle → chat
// (via the chat_message_join bridge) to gather, per message: the text, the
// other party's handle, the is_from_me flag, the Apple-epoch date, the chat
// GUID and display name, and the service. Messages are ordered by ROWID so the
// last row carries the new high-water mark, and only rows above :since are
// returned (the incremental cursor). chat.db can attach a message to more than
// one chat row; the GROUP BY collapses to one row per message, taking the
// minimum chat ROWID for determinism.
//
// Parameterization: %d is the since high-water rowid. We interpolate it as an
// integer we control (never user free-text) because the sqlite3 CLI takes the
// SQL as a single argv string.
const chatQuery = `SELECT
    m.ROWID            AS rowid,
    COALESCE(m.text, '')          AS text,
    COALESCE(h.id, '')            AS handle,
    m.is_from_me       AS is_from_me,
    m.date             AS date,
    COALESCE(c.guid, '')          AS chat_guid,
    COALESCE(c.display_name, '')  AS chat_name,
    COALESCE(m.service, '')       AS service
  FROM message m
  LEFT JOIN handle h ON h.ROWID = m.handle_id
  LEFT JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
  LEFT JOIN chat c ON c.ROWID = cmj.chat_id
  WHERE m.ROWID > %d
  GROUP BY m.ROWID
  ORDER BY m.ROWID ASC;`

// rawRow is the JSON shape sqlite3 -json emits for one chatQuery row. SQLite's
// JSON output renders integers as JSON numbers and text as JSON strings;
// is_from_me is 0/1. Some columns can be JSON null when COALESCE is bypassed by
// older schemas, so strings are decoded leniently.
type rawRow struct {
	RowID    int64  `json:"rowid"`
	Text     string `json:"text"`
	Handle   string `json:"handle"`
	IsFromMe int    `json:"is_from_me"`
	Date     int64  `json:"date"`
	ChatGUID string `json:"chat_guid"`
	ChatName string `json:"chat_name"`
	Service  string `json:"service"`
}

// toRow converts a decoded sqlite3 JSON row to the imsg.Row the mapper
// consumes.
func (r rawRow) toRow() imsg.Row {
	return imsg.Row{
		RowID:    r.RowID,
		ChatGUID: r.ChatGUID,
		ChatName: r.ChatName,
		Handle:   r.Handle,
		Text:     r.Text,
		IsFromMe: r.IsFromMe != 0,
		Service:  r.Service,
		Date:     r.Date,
	}
}

// parseRows decodes the JSON array sqlite3 -json prints and converts it to
// imsg.Rows. sqlite3 emits an empty result set as either "[]" or no output at
// all; both decode to zero rows.
func parseRows(stdout []byte) ([]imsg.Row, error) {
	trimmed := trimSpace(stdout)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var raw []rawRow
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return nil, fmt.Errorf("decode sqlite3 -json output: %w", err)
	}
	rows := make([]imsg.Row, 0, len(raw))
	for _, rr := range raw {
		rows = append(rows, rr.toRow())
	}
	return rows, nil
}

// trimSpace trims leading/trailing ASCII whitespace without importing strings
// just for this; keeps the JSON-decode tolerant of a trailing newline from the
// CLI.
func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// groupByChat groups rows by chat GUID, preserving first-seen chat order and
// per-chat chronological order (the query already orders by ROWID ascending).
// The returned slice of chats lets the agent render one transcript per chat.
func groupByChat(rows []imsg.Row) []chatGroup {
	index := make(map[string]int)
	var groups []chatGroup
	for _, r := range rows {
		i, ok := index[r.ChatGUID]
		if !ok {
			i = len(groups)
			index[r.ChatGUID] = i
			groups = append(groups, chatGroup{guid: r.ChatGUID})
		}
		groups[i].rows = append(groups[i].rows, r)
	}
	return groups
}

// chatGroup is the rows of one chat, in chronological order.
type chatGroup struct {
	guid string
	rows []imsg.Row
}

// maxRowID returns the largest ROWID in rows, the new incremental high-water
// mark. Returns 0 for an empty slice.
func maxRowID(rows []imsg.Row) int64 {
	var max int64
	for _, r := range rows {
		if r.RowID > max {
			max = r.RowID
		}
	}
	return max
}
