package main

import (
	"testing"
)

func TestParseRows(t *testing.T) {
	in := []byte(`[
      {"rowid":1,"text":"hi","handle":"+15551112222","is_from_me":0,"date":645000000000000000,"chat_guid":"g1","chat_name":"","service":"iMessage"},
      {"rowid":2,"text":"yo","handle":"","is_from_me":1,"date":645000060000000000,"chat_guid":"g1","chat_name":"","service":"SMS"}
    ]`)
	rows, err := parseRows(in)
	if err != nil {
		t.Fatalf("parseRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].RowID != 1 || rows[0].Text != "hi" || rows[0].Handle != "+15551112222" || rows[0].IsFromMe {
		t.Errorf("row0 = %+v", rows[0])
	}
	if rows[1].RowID != 2 || !rows[1].IsFromMe || rows[1].Service != "SMS" {
		t.Errorf("row1 = %+v", rows[1])
	}
}

func TestParseRowsEmpty(t *testing.T) {
	for _, in := range [][]byte{nil, []byte(""), []byte("  \n "), []byte("[]")} {
		rows, err := parseRows(in)
		if err != nil {
			t.Fatalf("parseRows(%q): %v", in, err)
		}
		if len(rows) != 0 {
			t.Errorf("parseRows(%q) = %d rows, want 0", in, len(rows))
		}
	}
}

func TestParseRowsTrailingNewline(t *testing.T) {
	rows, err := parseRows([]byte("[{\"rowid\":7,\"text\":\"x\"}]\n"))
	if err != nil {
		t.Fatalf("parseRows: %v", err)
	}
	if len(rows) != 1 || rows[0].RowID != 7 {
		t.Errorf("rows = %+v", rows)
	}
}

func TestParseRowsBadJSON(t *testing.T) {
	if _, err := parseRows([]byte("not json")); err == nil {
		t.Error("parseRows accepted non-JSON, want error")
	}
}

func TestGroupByChat(t *testing.T) {
	rows := sampleRows()
	groups := groupByChat(rows)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	// First-seen order preserved: g1 then g2.
	if groups[0].guid != "g1" || groups[1].guid != "g2" {
		t.Errorf("group order = %q, %q; want g1, g2", groups[0].guid, groups[1].guid)
	}
	if len(groups[0].rows) != 2 {
		t.Errorf("g1 rows = %d, want 2", len(groups[0].rows))
	}
	if len(groups[1].rows) != 1 {
		t.Errorf("g2 rows = %d, want 1", len(groups[1].rows))
	}
}

func TestMaxRowID(t *testing.T) {
	if got := maxRowID(nil); got != 0 {
		t.Errorf("maxRowID(nil) = %d, want 0", got)
	}
	if got := maxRowID(sampleRows()); got != 5 {
		t.Errorf("maxRowID = %d, want 5", got)
	}
}
