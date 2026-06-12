package main

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func typesEqual(a, b []askerv1.DocType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestExtractInlineFilters(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want parsedQuery
	}{
		{
			name: "plain text passes through",
			raw:  "quarterly planning meeting",
			want: parsedQuery{Text: "quarterly planning meeting"},
		},
		{
			name: "empty query",
			raw:  "",
			want: parsedQuery{},
		},
		{
			name: "whitespace normalized",
			raw:  "  hello \t  world  ",
			want: parsedQuery{Text: "hello world"},
		},
		{
			name: "from filter",
			raw:  "from:alice quarterly report",
			want: parsedQuery{Text: "quarterly report", Participant: "alice"},
		},
		{
			name: "from prefix case-insensitive, value case preserved",
			raw:  "From:Alice@Example.com report",
			want: parsedQuery{Text: "report", Participant: "Alice@Example.com"},
		},
		{
			name: "empty from value stays in text",
			raw:  "from: alice",
			want: parsedQuery{Text: "from: alice"},
		},
		{
			name: "last from wins",
			raw:  "from:alice from:bob",
			want: parsedQuery{Participant: "bob"},
		},
		{
			name: "type filter",
			raw:  "type:email budget",
			want: parsedQuery{Text: "budget", DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}},
		},
		{
			name: "type name case-insensitive",
			raw:  "TYPE:Email budget",
			want: parsedQuery{Text: "budget", DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}},
		},
		{
			name: "plural type tolerated",
			raw:  "type:emails budget",
			want: parsedQuery{Text: "budget", DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}},
		},
		{
			name: "underscore type and plural",
			raw:  "type:chat_messages standup",
			want: parsedQuery{Text: "standup", DocTypes: []askerv1.DocType{askerv1.DocType_CHAT_MESSAGE}},
		},
		{
			name: "calendar events plural",
			raw:  "type:calendar_events sync",
			want: parsedQuery{Text: "sync", DocTypes: []askerv1.DocType{askerv1.DocType_CALENDAR_EVENT}},
		},
		{
			name: "multiple types accumulate",
			raw:  "type:email type:file budget",
			want: parsedQuery{Text: "budget", DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL, askerv1.DocType_FILE}},
		},
		{
			name: "duplicate types deduped",
			raw:  "type:email type:emails budget",
			want: parsedQuery{Text: "budget", DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}},
		},
		{
			name: "unknown type stays in text",
			raw:  "type:bogus budget",
			want: parsedQuery{Text: "type:bogus budget"},
		},
		{
			name: "unspecified type not addressable",
			raw:  "type:doc_type_unspecified budget",
			want: parsedQuery{Text: "type:doc_type_unspecified budget"},
		},
		{
			name: "empty type value stays in text",
			raw:  "type: budget",
			want: parsedQuery{Text: "type: budget"},
		},
		{
			name: "after filter",
			raw:  "after:2024-01-02 report",
			want: parsedQuery{Text: "report", From: day(2024, time.January, 2)},
		},
		{
			name: "before filter",
			raw:  "before:2024-05-01 report",
			want: parsedQuery{Text: "report", To: day(2024, time.May, 1)},
		},
		{
			name: "date range",
			raw:  "after:2024-01-01 before:2024-02-01 report",
			want: parsedQuery{Text: "report", From: day(2024, time.January, 1), To: day(2024, time.February, 1)},
		},
		{
			name: "malformed date stays in text",
			raw:  "before:notadate report",
			want: parsedQuery{Text: "before:notadate report"},
		},
		{
			name: "out-of-range date stays in text",
			raw:  "before:2024-13-40 report",
			want: parsedQuery{Text: "before:2024-13-40 report"},
		},
		{
			name: "non-padded date rejected",
			raw:  "after:2024-1-2 report",
			want: parsedQuery{Text: "after:2024-1-2 report"},
		},
		{
			name: "everything combined, residual order preserved",
			raw:  "roadmap from:bob type:email after:2024-01-01 before:2024-06-01 review",
			want: parsedQuery{
				Text:        "roadmap review",
				Participant: "bob",
				DocTypes:    []askerv1.DocType{askerv1.DocType_EMAIL},
				From:        day(2024, time.January, 1),
				To:          day(2024, time.June, 1),
			},
		},
		{
			name: "filter-only query",
			raw:  "type:email from:alice",
			want: parsedQuery{Participant: "alice", DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}},
		},
		{
			name: "filter-like word mid-token is not a filter",
			raw:  "platform:from:alice",
			want: parsedQuery{Text: "platform:from:alice"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractInlineFilters(tt.raw)
			if got.Text != tt.want.Text {
				t.Errorf("Text = %q, want %q", got.Text, tt.want.Text)
			}
			if !typesEqual(got.DocTypes, tt.want.DocTypes) {
				t.Errorf("DocTypes = %v, want %v", got.DocTypes, tt.want.DocTypes)
			}
			if !got.From.Equal(tt.want.From) {
				t.Errorf("From = %v, want %v", got.From, tt.want.From)
			}
			if !got.To.Equal(tt.want.To) {
				t.Errorf("To = %v, want %v", got.To, tt.want.To)
			}
			if got.Participant != tt.want.Participant {
				t.Errorf("Participant = %q, want %q", got.Participant, tt.want.Participant)
			}
		})
	}
}

func TestUnderstandRequestFieldsWin(t *testing.T) {
	from := day(2025, time.March, 1)
	to := day(2025, time.April, 1)
	req := normalizeRequest(&queryv1.SearchRequest{
		Query:       "from:bob type:email before:2024-01-01 after:2023-01-01 budget",
		DocTypes:    []askerv1.DocType{askerv1.DocType_FILE, askerv1.DocType_FILE, askerv1.DocType_DOC_TYPE_UNSPECIFIED},
		FromDate:    timestamppb.New(from),
		ToDate:      timestamppb.New(to),
		Participant: "alice",
	})
	p := understand(req)

	if p.Text != "budget" {
		t.Errorf("Text = %q, want %q", p.Text, "budget")
	}
	if p.Participant != "alice" {
		t.Errorf("Participant = %q, want alice (request field wins)", p.Participant)
	}
	if !typesEqual(p.DocTypes, []askerv1.DocType{askerv1.DocType_FILE}) {
		t.Errorf("DocTypes = %v, want [FILE] (request field wins, deduped, no UNSPECIFIED)", p.DocTypes)
	}
	if !p.From.Equal(from) {
		t.Errorf("From = %v, want %v (request field wins)", p.From, from)
	}
	if !p.To.Equal(to) {
		t.Errorf("To = %v, want %v (request field wins)", p.To, to)
	}
}

func TestUnderstandInlineFiltersUsedWhenRequestFieldsAbsent(t *testing.T) {
	req := normalizeRequest(&queryv1.SearchRequest{Query: "from:bob type:ticket after:2024-06-01 standup"})
	p := understand(req)

	if p.Participant != "bob" {
		t.Errorf("Participant = %q, want bob", p.Participant)
	}
	if !typesEqual(p.DocTypes, []askerv1.DocType{askerv1.DocType_TICKET}) {
		t.Errorf("DocTypes = %v, want [TICKET]", p.DocTypes)
	}
	if !p.From.Equal(day(2024, time.June, 1)) {
		t.Errorf("From = %v, want 2024-06-01", p.From)
	}
	if !p.hasFilters() {
		t.Error("hasFilters() = false, want true")
	}
}

func TestHasFilters(t *testing.T) {
	if (parsedQuery{Text: "x"}).hasFilters() {
		t.Error("text-only query must not report filters")
	}
	for name, p := range map[string]parsedQuery{
		"participant": {Participant: "alice"},
		"types":       {DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}},
		"from":        {From: day(2024, time.January, 1)},
		"to":          {To: day(2024, time.January, 1)},
	} {
		if !p.hasFilters() {
			t.Errorf("%s: hasFilters() = false, want true", name)
		}
	}
}

func TestNormalizeRequest(t *testing.T) {
	tests := []struct {
		name                  string
		limit, offset         int32
		mode                  queryv1.SearchMode
		wantLimit, wantOffset int32
		wantMode              queryv1.SearchMode
	}{
		{name: "defaults", wantLimit: 20, wantOffset: 0, wantMode: queryv1.SearchMode_HYBRID},
		{name: "limit capped", limit: 1000, wantLimit: 100, wantMode: queryv1.SearchMode_HYBRID},
		{name: "negative limit defaulted", limit: -3, wantLimit: 20, wantMode: queryv1.SearchMode_HYBRID},
		{name: "offset clamped low", offset: -5, wantLimit: 20, wantOffset: 0, wantMode: queryv1.SearchMode_HYBRID},
		{name: "offset capped", offset: 99999, wantLimit: 20, wantOffset: 1000, wantMode: queryv1.SearchMode_HYBRID},
		{name: "explicit mode kept", mode: queryv1.SearchMode_VECTOR, wantLimit: 20, wantMode: queryv1.SearchMode_VECTOR},
		{name: "in-range values kept", limit: 50, offset: 40, mode: queryv1.SearchMode_KEYWORD, wantLimit: 50, wantOffset: 40, wantMode: queryv1.SearchMode_KEYWORD},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeRequest(&queryv1.SearchRequest{
				Query: "  q  ", Limit: tt.limit, Offset: tt.offset, Mode: tt.mode,
				Participant: " alice ",
			})
			if got.GetLimit() != tt.wantLimit {
				t.Errorf("Limit = %d, want %d", got.GetLimit(), tt.wantLimit)
			}
			if got.GetOffset() != tt.wantOffset {
				t.Errorf("Offset = %d, want %d", got.GetOffset(), tt.wantOffset)
			}
			if got.GetMode() != tt.wantMode {
				t.Errorf("Mode = %v, want %v", got.GetMode(), tt.wantMode)
			}
			if got.GetQuery() != "q" {
				t.Errorf("Query = %q, want trimmed %q", got.GetQuery(), "q")
			}
			if got.GetParticipant() != "alice" {
				t.Errorf("Participant = %q, want trimmed %q", got.GetParticipant(), "alice")
			}
		})
	}
}
