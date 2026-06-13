package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func TestBuildYQL(t *testing.T) {
	tests := []struct {
		name    string
		q       vespaQuery
		want    string
		wantErr error
	}{
		{
			name: "keyword no filters",
			q:    vespaQuery{Kind: retrieveKeyword},
			want: `select * from sources * where userQuery()`,
		},
		{
			name: "hybrid clause",
			q:    vespaQuery{Kind: retrieveHybrid},
			want: `select * from sources * where rank(userQuery(), ({targetHits:100}nearestNeighbor(embedding,q)))`,
		},
		{
			name: "vector clause alone",
			q:    vespaQuery{Kind: retrieveVector},
			want: `select * from sources * where ({targetHits:100}nearestNeighbor(embedding,q))`,
		},
		{
			name: "clip text->image clause",
			q:    vespaQuery{Kind: retrieveCLIP},
			want: `select * from sources * where ({targetHits:100}nearestNeighbor(clip_embedding,qclip))`,
		},
		{
			name: "clip clause respects filters",
			q: vespaQuery{
				Kind:     retrieveCLIP,
				DocTypes: []askerv1.DocType{askerv1.DocType_IMAGE},
			},
			want: `select * from sources * where ({targetHits:100}nearestNeighbor(clip_embedding,qclip)) and type contains "IMAGE"`,
		},
		{
			name: "filter-only true clause",
			q:    vespaQuery{Kind: retrieveFilterOnly, DocTypes: []askerv1.DocType{askerv1.DocType_EMAIL}},
			want: `select * from sources * where true and type contains "EMAIL"`,
		},
		{
			name: "multiple types OR-grouped",
			q: vespaQuery{Kind: retrieveKeyword, DocTypes: []askerv1.DocType{
				askerv1.DocType_EMAIL, askerv1.DocType_FILE,
			}},
			want: `select * from sources * where userQuery() and (type contains "EMAIL" or type contains "FILE")`,
		},
		{
			name: "date range",
			q: vespaQuery{
				Kind: retrieveKeyword,
				From: time.Unix(1718000000, 0).UTC(),
				To:   time.Unix(1718600000, 0).UTC(),
			},
			want: `select * from sources * where userQuery() and created_at >= 1718000000 and created_at <= 1718600000`,
		},
		{
			name: "participant substring filter",
			q:    vespaQuery{Kind: retrieveKeyword, Participant: "alice"},
			want: `select * from sources * where userQuery() and participants contains ({substring:true}"alice")`,
		},
		{
			name: "participant quotes escaped",
			q:    vespaQuery{Kind: retrieveKeyword, Participant: `ali"ce`},
			want: `select * from sources * where userQuery() and participants contains ({substring:true}"ali\"ce")`,
		},
		{
			name: "participant backslash escaped",
			q:    vespaQuery{Kind: retrieveKeyword, Participant: `a\"b`},
			want: `select * from sources * where userQuery() and participants contains ({substring:true}"a\\\"b")`,
		},
		{
			name: "injection attempt stays inside the literal",
			q:    vespaQuery{Kind: retrieveKeyword, Participant: `x") or true or participants contains ("y`},
			want: `select * from sources * where userQuery() and participants contains ({substring:true}"x\") or true or participants contains (\"y")`,
		},
		{
			name:    "participant control character rejected",
			q:       vespaQuery{Kind: retrieveKeyword, Participant: "ali\nce"},
			wantErr: errInvalidFilterValue,
		},
		{
			name: "all filters combined",
			q: vespaQuery{
				Kind:        retrieveHybrid,
				DocTypes:    []askerv1.DocType{askerv1.DocType_CALENDAR_EVENT},
				From:        time.Unix(100, 0).UTC(),
				Participant: "bob@example.com",
			},
			want: `select * from sources * where rank(userQuery(), ({targetHits:100}nearestNeighbor(embedding,q))) and type contains "CALENDAR_EVENT" and created_at >= 100 and participants contains ({substring:true}"bob@example.com")`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildYQL(tt.q)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("buildYQL() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildYQL() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("buildYQL() =\n  %s\nwant\n  %s", got, tt.want)
			}
		})
	}
}

func TestBuildYQLNeverContainsQueryText(t *testing.T) {
	// The free-text query travels ONLY via the query= body parameter and
	// userQuery(); buildYQL does not interpolate it, so even hostile text
	// cannot reach the YQL string.
	q := vespaQuery{Kind: retrieveKeyword, Text: `evil" or true --`}
	yql, err := buildYQL(q)
	if err != nil {
		t.Fatalf("buildYQL: %v", err)
	}
	if strings.Contains(yql, "evil") {
		t.Errorf("query text leaked into YQL: %s", yql)
	}
}

func TestYQLStringLiteral(t *testing.T) {
	for _, tt := range []struct {
		in, want string
	}{
		{`plain`, `"plain"`},
		{`with"quote`, `"with\"quote"`},
		{`back\slash`, `"back\\slash"`},
		{`both\"`, `"both\\\""`},
	} {
		got, err := yqlStringLiteral(tt.in)
		if err != nil {
			t.Errorf("yqlStringLiteral(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("yqlStringLiteral(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
	for _, bad := range []string{"a\nb", "a\rb", "a\x00b", "a\x7fb", "tab\tval"} {
		if _, err := yqlStringLiteral(bad); !errors.Is(err, errInvalidFilterValue) {
			t.Errorf("yqlStringLiteral(%q) error = %v, want errInvalidFilterValue", bad, err)
		}
	}
}

const vespaFixture = `{
  "root": {
    "id": "toplevel",
    "fields": {"totalCount": 2},
    "children": [
      {
        "id": "index:asker/0/aaa",
        "relevance": 0.87,
        "fields": {
          "doc_id": "doc-1",
          "connector_id": "gmail",
          "type": "EMAIL",
          "title": "Quarterly planning",
          "snippet": "about the <hi>quarterly</hi> plan",
          "chunk_snippets": ["full unmatched chunk text", "chunk with <hi>quarterly</hi> term"],
          "metadata_json": "{\"sender\":\"alice@example.com\",\"count\":3}",
          "created_at": 1718000000,
          "modified_at": 1718000600
        }
      },
      {
        "id": "index:asker/0/bbb",
        "relevance": 0.42,
        "fields": {
          "doc_id": "doc-2",
          "connector_id": "upload",
          "type": "SOMETHING_NEW",
          "title": "Untitled",
          "chunk_snippets": [""]
        }
      }
    ]
  }
}`

// clipFixture is a CLIP-arm (text->image) response: an image doc whose
// visually-matched chunk is named by the closest(clip_embedding) summary
// feature, carrying parallel chunk_starts_ms/ends_ms/modalities arrays and a
// doc-level thumbnail_key (ADR-013).
const clipFixture = `{
  "root": {
    "id": "toplevel",
    "fields": {"totalCount": 1},
    "children": [
      {
        "id": "index:asker/0/img",
        "relevance": 0.93,
        "fields": {
          "doc_id": "doc-img",
          "connector_id": "drive",
          "type": "IMAGE",
          "title": "Beach sunset.jpg",
          "thumbnail_key": "thumb/doc-img.jpg",
          "chunk_starts_ms": [0, 0],
          "chunk_ends_ms": [0, 0],
          "chunk_modalities": ["ocr", "caption"],
          "summaryfeatures": {
            "closest(clip_embedding)": {"type": "tensor(chunk{})", "cells": {"1": 1.0}}
          }
        }
      }
    ]
  }
}`

// asrFixture is a text/ASR-arm response: a video doc whose matched transcript
// chunk is the one carrying the <hi> highlight; its start_ms/modality come
// from the parallel arrays at that index.
const asrFixture = `{
  "root": {
    "id": "toplevel",
    "fields": {"totalCount": 1},
    "children": [
      {
        "id": "index:asker/0/vid",
        "relevance": 0.71,
        "fields": {
          "doc_id": "doc-vid",
          "connector_id": "drive",
          "type": "VIDEO",
          "title": "All hands.mp4",
          "thumbnail_key": "thumb/doc-vid.jpg",
          "chunk_snippets": ["intro no match", "the <hi>budget</hi> discussion", "outro"],
          "chunk_starts_ms": [0, 42000, 90000],
          "chunk_ends_ms": [42000, 90000, 120000],
          "chunk_modalities": ["asr", "asr", "asr"]
        }
      }
    ]
  }
}`

// textDocFixture is a realistic text/hybrid-arm response for a plain EMAIL doc
// as the index writer actually emits it: every chunk carries a modality label,
// and for non-media docs that label is "text" (the parallel chunk_starts_ms/
// ends_ms are absent — text chunks have no time offset). The matched chunk is
// the highlighted one at index 1. Per the Hit contract (query.proto) this is
// the whole-document / non-media case: modality and offsets must stay zero.
const textDocFixture = `{
  "root": {
    "id": "toplevel",
    "fields": {"totalCount": 1},
    "children": [
      {
        "id": "index:asker/0/aaa",
        "relevance": 0.87,
        "fields": {
          "doc_id": "doc-1",
          "connector_id": "gmail",
          "type": "EMAIL",
          "title": "Quarterly planning",
          "snippet": "about the <hi>quarterly</hi> plan",
          "chunk_snippets": ["full unmatched chunk text", "chunk with <hi>quarterly</hi> term"],
          "chunk_modalities": ["text", "text"],
          "metadata_json": "{\"sender\":\"alice@example.com\"}",
          "created_at": 1718000000
        }
      }
    ]
  }
}`

func TestParseVespaResponseClipMediaFields(t *testing.T) {
	res, err := parseVespaResponse([]byte(clipFixture), retrieveCLIP)
	if err != nil {
		t.Fatalf("parseVespaResponse(clip): %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("len(Hits) = %d, want 1", len(res.Hits))
	}
	h := res.Hits[0]
	// closest(clip_embedding) named chunk index 1 -> the "caption" chunk.
	if h.GetModality() != "caption" {
		t.Errorf("Modality = %q, want caption (the matched CLIP chunk)", h.GetModality())
	}
	if h.GetThumbnailKey() != "thumb/doc-img.jpg" {
		t.Errorf("ThumbnailKey = %q, want the doc thumbnail", h.GetThumbnailKey())
	}
}

func TestParseVespaResponseAsrMediaFields(t *testing.T) {
	res, err := parseVespaResponse([]byte(asrFixture), retrieveHybrid)
	if err != nil {
		t.Fatalf("parseVespaResponse(asr): %v", err)
	}
	h := res.Hits[0]
	// The matched chunk is index 1 (the only highlighted chunk_snippets element).
	if h.GetStartMs() != 42000 || h.GetEndMs() != 90000 {
		t.Errorf("start/end = %d/%d, want 42000/90000 (the matched ASR chunk)", h.GetStartMs(), h.GetEndMs())
	}
	if h.GetModality() != "asr" {
		t.Errorf("Modality = %q, want asr", h.GetModality())
	}
	if h.GetThumbnailKey() != "thumb/doc-vid.jpg" {
		t.Errorf("ThumbnailKey = %q, want the poster thumbnail", h.GetThumbnailKey())
	}
}

func TestMatchedClipChunkIndex(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"tensor cells form", `{"type":"tensor(chunk{})","cells":{"3":1.0}}`, 3},
		{"bare map form", `{"2":1.0}`, 2},
		{"smallest label wins", `{"cells":{"5":1.0,"2":1.0}}`, 2},
		{"empty", `{}`, -1},
		{"non-numeric label", `{"cells":{"x":1.0}}`, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			feats := map[string]json.RawMessage{"closest(clip_embedding)": json.RawMessage(tt.raw)}
			if got := matchedClipChunkIndex(feats); got != tt.want {
				t.Errorf("matchedClipChunkIndex(%s) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
	if got := matchedClipChunkIndex(nil); got != -1 {
		t.Errorf("matchedClipChunkIndex(nil) = %d, want -1", got)
	}
}

func TestPopulateMediaFieldsTextDocStaysZero(t *testing.T) {
	// A plain EMAIL doc, exactly as the index writer emits it: every chunk is
	// labeled, and the matched (highlighted) chunk's modality is "text". Per
	// the Hit contract that is the whole-document / non-media case — modality
	// and offsets MUST come back zero rather than echoing the "text" label.
	res, err := parseVespaResponse([]byte(textDocFixture), retrieveHybrid)
	if err != nil {
		t.Fatalf("parseVespaResponse: %v", err)
	}
	h := res.Hits[0]
	if h.GetType() != askerv1.DocType_EMAIL {
		t.Fatalf("Type = %v, want EMAIL (wrong fixture)", h.GetType())
	}
	if h.GetModality() != "" {
		t.Errorf("Modality = %q, want \"\" for a non-media text hit", h.GetModality())
	}
	if h.GetStartMs() != 0 || h.GetEndMs() != 0 {
		t.Errorf("start/end = %d/%d, want 0/0 for a non-media text hit", h.GetStartMs(), h.GetEndMs())
	}
	if h.GetThumbnailKey() != "" {
		t.Errorf("ThumbnailKey = %q, want \"\" (text doc has no thumbnail)", h.GetThumbnailKey())
	}
}

// TestPopulateMediaFieldsMediaStaysPopulated is the companion to the text-doc
// case: a genuine media (asr) chunk must still be fully populated. The fix that
// zeroes "text" modalities must not regress real media hits.
func TestPopulateMediaFieldsMediaStaysPopulated(t *testing.T) {
	res, err := parseVespaResponse([]byte(asrFixture), retrieveHybrid)
	if err != nil {
		t.Fatalf("parseVespaResponse(asr): %v", err)
	}
	h := res.Hits[0]
	if h.GetModality() != "asr" {
		t.Errorf("Modality = %q, want asr (matched media chunk)", h.GetModality())
	}
	if h.GetStartMs() <= 0 {
		t.Errorf("StartMs = %d, want > 0 for the matched ASR chunk", h.GetStartMs())
	}
	if h.GetEndMs() != 90000 {
		t.Errorf("EndMs = %d, want 90000 for the matched ASR chunk", h.GetEndMs())
	}
}

func TestParseVespaResponse(t *testing.T) {
	res, err := parseVespaResponse([]byte(vespaFixture), retrieveHybrid)
	if err != nil {
		t.Fatalf("parseVespaResponse: %v", err)
	}
	if res.Total != 2 {
		t.Errorf("Total = %d, want 2", res.Total)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("len(Hits) = %d, want 2", len(res.Hits))
	}

	h := res.Hits[0]
	if h.GetDocId() != "doc-1" || h.GetConnectorId() != "gmail" {
		t.Errorf("hit identity = %q/%q, want doc-1/gmail", h.GetDocId(), h.GetConnectorId())
	}
	if h.GetType() != askerv1.DocType_EMAIL {
		t.Errorf("Type = %v, want EMAIL", h.GetType())
	}
	if h.GetSnippet() != "about the <hi>quarterly</hi> plan" {
		t.Errorf("Snippet = %q, want highlighted body snippet", h.GetSnippet())
	}
	if h.GetScore() != 0.87 {
		t.Errorf("Score = %v, want 0.87", h.GetScore())
	}
	if got := h.GetCreated().AsTime().Unix(); got != 1718000000 {
		t.Errorf("Created = %d, want 1718000000", got)
	}
	if got := h.GetModified().AsTime().Unix(); got != 1718000600 {
		t.Errorf("Modified = %d, want 1718000600", got)
	}
	if h.GetMetadata()["sender"] != "alice@example.com" {
		t.Errorf("Metadata[sender] = %q, want alice@example.com", h.GetMetadata()["sender"])
	}
	if h.GetMetadata()["count"] != "3" {
		t.Errorf("Metadata[count] = %q, want JSON-stringified \"3\"", h.GetMetadata()["count"])
	}

	h2 := res.Hits[1]
	if h2.GetType() != askerv1.DocType_DOC_TYPE_UNSPECIFIED {
		t.Errorf("unknown type name parsed to %v, want DOC_TYPE_UNSPECIFIED", h2.GetType())
	}
	if h2.GetSnippet() != "Untitled" {
		t.Errorf("Snippet fallback = %q, want title", h2.GetSnippet())
	}
	if h2.GetCreated() != nil {
		t.Errorf("Created = %v, want nil for missing epoch", h2.GetCreated())
	}
}

func TestParseVespaResponseErrorsOnly(t *testing.T) {
	raw := `{"root":{"errors":[{"code":8,"summary":"Error in search reply","message":"boom"}]}}`
	if _, err := parseVespaResponse([]byte(raw), retrieveHybrid); err == nil {
		t.Fatal("parseVespaResponse with errors and no children must fail")
	}
}

func TestChooseSnippet(t *testing.T) {
	tests := []struct {
		name string
		f    vespaHitFields
		want string
	}{
		{
			name: "body snippet with highlight wins",
			f:    vespaHitFields{Snippet: "a <hi>x</hi>", ChunkSnippets: []string{"b <hi>x</hi>"}, Title: "t"},
			want: "a <hi>x</hi>",
		},
		{
			name: "chunk fragment with highlight beats unhighlighted body snippet",
			f:    vespaHitFields{Snippet: "plain body", ChunkSnippets: []string{"plain", "has <hi>x</hi>"}, Title: "t"},
			want: "has <hi>x</hi>",
		},
		{
			name: "unhighlighted body snippet as fallback",
			f:    vespaHitFields{Snippet: "plain body", ChunkSnippets: []string{"plain"}, Title: "t"},
			want: "plain body",
		},
		{
			name: "first chunk as fallback",
			f:    vespaHitFields{ChunkSnippets: []string{"first chunk", "second"}, Title: "t"},
			want: "first chunk",
		},
		{
			name: "title as last resort",
			f:    vespaHitFields{Title: "t"},
			want: "t",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chooseSnippet(tt.f); got != tt.want {
				t.Errorf("chooseSnippet() = %q, want %q", got, tt.want)
			}
		})
	}

	long := strings.Repeat("x", 2048)
	got := chooseSnippet(vespaHitFields{ChunkSnippets: []string{long}})
	if len([]rune(got)) > snippetFallbackMaxRunes+1 {
		t.Errorf("fallback snippet not truncated: %d runes", len([]rune(got)))
	}
}

func TestVespaClientErrorClassification(t *testing.T) {
	t.Run("5xx is degradable", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "internal", http.StatusInternalServerError)
		}))
		defer ts.Close()
		_, err := newVespaClient(ts.URL).Search(context.Background(), vespaQuery{Kind: retrieveHybrid, Text: "x"})
		if !isDegradable(err) {
			t.Errorf("5xx error = %v, want degradable", err)
		}
	})
	t.Run("4xx is terminal", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "bad query", http.StatusBadRequest)
		}))
		defer ts.Close()
		_, err := newVespaClient(ts.URL).Search(context.Background(), vespaQuery{Kind: retrieveHybrid, Text: "x"})
		if err == nil || isDegradable(err) {
			t.Errorf("4xx error = %v, want terminal non-degradable", err)
		}
	})
	t.Run("transport error is degradable", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		ts.Close() // refuse connections
		_, err := newVespaClient(ts.URL).Search(context.Background(), vespaQuery{Kind: retrieveHybrid, Text: "x"})
		if !isDegradable(err) {
			t.Errorf("transport error = %v, want degradable", err)
		}
	})
	t.Run("invalid filter is terminal and typed", func(t *testing.T) {
		_, err := newVespaClient("http://unused").Search(context.Background(),
			vespaQuery{Kind: retrieveKeyword, Participant: "a\nb"})
		if !errors.Is(err, errInvalidFilterValue) {
			t.Errorf("error = %v, want errInvalidFilterValue", err)
		}
		if isDegradable(err) {
			t.Error("invalid filter must not be degradable")
		}
	})
}
