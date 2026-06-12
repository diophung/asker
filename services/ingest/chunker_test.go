package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func chunkDoc(docType askerv1.DocType, body string) *askerv1.Document {
	return &askerv1.Document{
		TenantId: "tenant-a",
		DocId:    "doc-1",
		Type:     docType,
		BodyText: body,
	}
}

// verifyChunkInvariants checks the structural contract every chunk list must
// satisfy: ids are doc_id#index, offsets are byte offsets slicing the body to
// the chunk text exactly, all cuts land on rune boundaries, consecutive
// chunks overlap or touch (no gaps), and starts strictly advance. When
// fullCoverage is set it additionally requires the chunks to tile the body
// from byte 0 to len(body), and reconstructs the body from the chunk texts.
func verifyChunkInvariants(t *testing.T, body string, chunks []*askerv1.Chunk, fullCoverage bool) {
	t.Helper()
	for i, c := range chunks {
		wantID := fmt.Sprintf("doc-1#%d", i)
		if c.GetChunkId() != wantID {
			t.Errorf("chunk %d: id = %q, want %q", i, c.GetChunkId(), wantID)
		}
		start, end := int(c.GetCharStart()), int(c.GetCharEnd())
		if start < 0 || end > len(body) || start >= end {
			t.Fatalf("chunk %d: bad offsets [%d, %d) for body of %d bytes", i, start, end, len(body))
		}
		if body[start:end] != c.GetText() {
			t.Errorf("chunk %d: body[%d:%d] does not equal chunk text", i, start, end)
		}
		if !utf8.ValidString(c.GetText()) {
			t.Errorf("chunk %d: text is not valid UTF-8 (cut inside a rune)", i)
		}
		if i > 0 {
			prev := chunks[i-1]
			if start > int(prev.GetCharEnd()) {
				t.Errorf("chunk %d: gap: starts at %d after previous end %d", i, start, prev.GetCharEnd())
			}
			if start <= int(prev.GetCharStart()) {
				t.Errorf("chunk %d: no progress: starts at %d, previous start %d", i, start, prev.GetCharStart())
			}
		}
	}
	if fullCoverage && len(chunks) > 0 {
		if chunks[0].GetCharStart() != 0 {
			t.Errorf("first chunk starts at %d, want 0", chunks[0].GetCharStart())
		}
		if got := chunks[len(chunks)-1].GetCharEnd(); int(got) != len(body) {
			t.Errorf("last chunk ends at %d, want %d", got, len(body))
		}
		// Golden reconstruction: stitching the chunks back together using
		// their offsets (dropping each chunk's overlap with its predecessor)
		// must reproduce the body byte for byte.
		var sb strings.Builder
		coveredTo := 0
		for _, c := range chunks {
			sb.WriteString(c.GetText()[coveredTo-int(c.GetCharStart()):])
			coveredTo = int(c.GetCharEnd())
		}
		if sb.String() != body {
			t.Error("reconstructing body from chunk texts and offsets failed")
		}
	}
}

func TestBuildChunksTable(t *testing.T) {
	t.Parallel()

	// threeParas: paragraphs of 3-byte words, sized so the window math is
	// hand-checkable (1499 + 2 + 1499 + 2 + 1499 = 4501 bytes).
	para := strings.TrimSpace(strings.Repeat("ab ", 500)) // 1499 bytes
	threeParas := para + "\n\n" + para + "\n\n" + para

	tests := []struct {
		name         string
		docType      askerv1.DocType
		body         string
		wantChunks   int
		wantTrunc    bool
		fullCoverage bool
	}{
		{
			name:         "empty body yields zero chunks (title-only docs are legal)",
			docType:      askerv1.DocType_FILE,
			body:         "",
			wantChunks:   0,
			fullCoverage: true,
		},
		{
			name:         "empty email body yields zero chunks",
			docType:      askerv1.DocType_EMAIL,
			body:         "",
			wantChunks:   0,
			fullCoverage: true,
		},
		{
			name:         "empty chat body yields zero chunks",
			docType:      askerv1.DocType_CHAT_MESSAGE,
			body:         "",
			wantChunks:   0,
			fullCoverage: true,
		},
		{
			name:         "small body is one chunk",
			docType:      askerv1.DocType_FILE,
			body:         "hello world",
			wantChunks:   1,
			fullCoverage: true,
		},
		{
			name:         "body exactly one window is one chunk",
			docType:      askerv1.DocType_WIKI_PAGE,
			body:         strings.TrimSpace(strings.Repeat("xy ", chunkWindowBytes/3)) + "x",
			wantChunks:   1,
			fullCoverage: true,
		},
		{
			name:         "three paragraphs window into three chunks",
			docType:      askerv1.DocType_FILE,
			body:         threeParas,
			wantChunks:   3,
			fullCoverage: true,
		},
		{
			name:       "chat message is a single chunk regardless of size",
			docType:    askerv1.DocType_CHAT_MESSAGE,
			body:       strings.Repeat("chat blather ", 1000), // 13000 bytes > cap
			wantChunks: 1,
		},
		{
			name:       "calendar event is a single chunk regardless of size",
			docType:    askerv1.DocType_CALENDAR_EVENT,
			body:       strings.Repeat("agenda item; ", 1000),
			wantChunks: 1,
		},
		{
			name:         "calendar event under the cap keeps its whole body",
			docType:      askerv1.DocType_CALENDAR_EVENT,
			body:         strings.Repeat("standup notes ", 400), // 5600 bytes > window, < cap
			wantChunks:   1,
			fullCoverage: true,
		},
		{
			name:         "unspecified type falls back to paragraph windowing",
			docType:      askerv1.DocType_DOC_TYPE_UNSPECIFIED,
			body:         threeParas,
			wantChunks:   3,
			fullCoverage: true,
		},
		{
			name:         "huge body truncates at the chunk cap",
			docType:      askerv1.DocType_FILE,
			body:         strings.TrimSpace(strings.Repeat("word ", 25000)), // ~125 KB
			wantChunks:   maxChunksPerDoc,
			wantTrunc:    true,
			fullCoverage: false, // truncation drops the tail by design
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chunks, truncated := buildChunks(chunkDoc(tc.docType, tc.body))
			if len(chunks) != tc.wantChunks {
				t.Errorf("got %d chunks, want %d", len(chunks), tc.wantChunks)
			}
			if truncated != tc.wantTrunc {
				t.Errorf("truncated = %v, want %v", truncated, tc.wantTrunc)
			}
			verifyChunkInvariants(t, tc.body, chunks, tc.fullCoverage)
			for _, c := range chunks {
				if tc.docType == askerv1.DocType_CHAT_MESSAGE || tc.docType == askerv1.DocType_CALENDAR_EVENT {
					if len(c.GetText()) > singleChunkCapBytes {
						t.Errorf("single-chunk type exceeds %d-byte cap: %d", singleChunkCapBytes, len(c.GetText()))
					}
				} else if len(c.GetText()) > chunkWindowBytes {
					t.Errorf("chunk exceeds window: %d > %d bytes", len(c.GetText()), chunkWindowBytes)
				}
			}
		})
	}
}

// TestOverlapContinuityGolden pins the exact spans for a hand-computed body:
// three 1499-byte paragraphs of "ab " words. The first window cuts at the
// first paragraph break (1501), each following window starts 256 bytes
// before the previous end (1245, 2746 — both word-aligned already), and the
// middle window cuts at the second paragraph break (3002).
func TestOverlapContinuityGolden(t *testing.T) {
	t.Parallel()
	para := strings.TrimSpace(strings.Repeat("ab ", 500)) // 1499 bytes
	body := para + "\n\n" + para + "\n\n" + para          // 4501 bytes

	chunks, truncated := buildChunks(chunkDoc(askerv1.DocType_FILE, body))
	if truncated {
		t.Fatal("unexpected truncation")
	}
	want := []span{{0, 1501}, {1245, 3002}, {2746, 4501}}
	if len(chunks) != len(want) {
		t.Fatalf("got %d chunks, want %d", len(chunks), len(want))
	}
	for i, w := range want {
		if got := (span{int(chunks[i].GetCharStart()), int(chunks[i].GetCharEnd())}); got != w {
			t.Errorf("chunk %d span = %v, want %v", i, got, w)
		}
	}
	// Both interior overlaps are exactly chunkOverlapBytes here.
	for i := 1; i < len(chunks); i++ {
		overlap := int(chunks[i-1].GetCharEnd() - chunks[i].GetCharStart())
		if overlap != chunkOverlapBytes {
			t.Errorf("overlap between chunk %d and %d = %d bytes, want %d", i-1, i, overlap, chunkOverlapBytes)
		}
	}
	verifyChunkInvariants(t, body, chunks, true)
}

// TestPlainParagraphsNeverSplitMidWord verifies word-boundary cuts on
// realistic prose: every non-final chunk ends on whitespace.
func TestPlainParagraphsNeverSplitMidWord(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&sb, "Paragraph %d begins here. %s\n\n", i, strings.Repeat("lorem ipsum dolor sit amet ", 12))
	}
	body := strings.TrimSpace(sb.String())

	chunks, truncated := buildChunks(chunkDoc(askerv1.DocType_FILE, body))
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want several", len(chunks))
	}
	for i, c := range chunks[:len(chunks)-1] {
		last := body[c.GetCharEnd()-1]
		if !isSpaceByte(last) {
			t.Errorf("chunk %d ends mid-word: trailing byte %q", i, last)
		}
	}
	verifyChunkInvariants(t, body, chunks, true)
}

func TestEmailQuotedReplySections(t *testing.T) {
	t.Parallel()
	reply := "Thanks, sounds good. Let's sync tomorrow morning."
	marker := "On Mon, Jun 2, 2025 at 9:14 AM Alice <alice@example.com> wrote:"
	quoted := "> Are you free tomorrow?\n> -- Alice"
	body := reply + "\n\n" + marker + "\n" + quoted
	markerOff := strings.Index(body, marker)

	chunks, truncated := buildChunks(chunkDoc(askerv1.DocType_EMAIL, body))
	if truncated {
		t.Fatal("unexpected truncation")
	}
	// Two sections, each smaller than the window: exactly one chunk each,
	// the marker line chunked together with the text it attributes.
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if got := chunks[0].GetText(); got != reply+"\n\n" {
		t.Errorf("chunk 0 text = %q, want the fresh reply section", got)
	}
	if got := int(chunks[1].GetCharStart()); got != markerOff {
		t.Errorf("chunk 1 starts at %d, want marker offset %d", got, markerOff)
	}
	if got := chunks[1].GetText(); got != marker+"\n"+quoted {
		t.Errorf("chunk 1 text = %q, want marker plus quote run", got)
	}
	verifyChunkInvariants(t, body, chunks, true)
}

func TestEmailQuoteRunWithoutMarker(t *testing.T) {
	t.Parallel()
	body := "fresh reply text\n\n> old line one\n> old line two\nunquoted trailer"
	quoteOff := strings.Index(body, "> old")

	chunks, truncated := buildChunks(chunkDoc(askerv1.DocType_EMAIL, body))
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if got := int(chunks[1].GetCharStart()); got != quoteOff {
		t.Errorf("quote section starts at %d, want %d", got, quoteOff)
	}
	// The unquoted trailer stays inside the quote-run section: only quote
	// runs and markers start sections.
	if !strings.HasSuffix(chunks[1].GetText(), "unquoted trailer") {
		t.Errorf("trailer split out of quote section: %q", chunks[1].GetText())
	}
	verifyChunkInvariants(t, body, chunks, true)
}

// TestEmailLargeSectionWindowsWithinSection checks that an oversized section
// is windowed but windows (and their overlap) never cross the section
// boundary, and that a section smaller than the window stays one chunk.
func TestEmailLargeSectionWindowsWithinSection(t *testing.T) {
	t.Parallel()
	bigPara := strings.TrimSpace(strings.Repeat("ab ", 1000)) // 2999 bytes > window
	marker := "On Tue, Jun 3, 2025 at 10:00 AM Bob <bob@example.com> wrote:"
	quoted := "> short quoted reply"
	body := bigPara + "\n" + marker + "\n" + quoted
	markerOff := strings.Index(body, marker)

	chunks, truncated := buildChunks(chunkDoc(askerv1.DocType_EMAIL, body))
	if truncated {
		t.Fatal("unexpected truncation")
	}
	var sawSectionStart bool
	for i, c := range chunks {
		start, end := int(c.GetCharStart()), int(c.GetCharEnd())
		if start < markerOff && end > markerOff {
			t.Errorf("chunk %d [%d, %d) spans the section boundary at %d", i, start, end, markerOff)
		}
		if start == markerOff {
			sawSectionStart = true
			if c.GetText() != marker+"\n"+quoted {
				t.Errorf("section smaller than the window split up: %q", c.GetText())
			}
		}
	}
	if !sawSectionStart {
		t.Errorf("no chunk starts exactly at the section boundary %d", markerOff)
	}
	if len(chunks) != 3 { // big section -> 2 windows, quote section -> 1
		t.Errorf("got %d chunks, want 3", len(chunks))
	}
	verifyChunkInvariants(t, body, chunks, true)
}

// TestUnicodeSafety drives the hard-cut path (CJK text without whitespace)
// and the single-chunk truncation path with multi-byte runes: no cut may
// land inside a rune. Offsets are byte offsets into BodyText.
func TestUnicodeSafety(t *testing.T) {
	t.Parallel()

	t.Run("hard cuts land on rune boundaries", func(t *testing.T) {
		t.Parallel()
		body := strings.Repeat("日本語のテキスト", 200) // 4800 bytes, no whitespace
		chunks, truncated := buildChunks(chunkDoc(askerv1.DocType_FILE, body))
		if truncated {
			t.Fatal("unexpected truncation")
		}
		if len(chunks) < 2 {
			t.Fatalf("got %d chunks, want several", len(chunks))
		}
		for i := 1; i < len(chunks); i++ {
			overlap := int(chunks[i-1].GetCharEnd() - chunks[i].GetCharStart())
			if overlap <= 0 || overlap > chunkOverlapBytes {
				t.Errorf("overlap between chunks %d and %d = %d bytes, want in (0, %d]",
					i-1, i, overlap, chunkOverlapBytes)
			}
		}
		verifyChunkInvariants(t, body, chunks, true)
	})

	t.Run("single-chunk truncation is rune safe", func(t *testing.T) {
		t.Parallel()
		body := strings.Repeat("あ", 3000) // 9000 bytes of 3-byte runes
		chunks, _ := buildChunks(chunkDoc(askerv1.DocType_CHAT_MESSAGE, body))
		if len(chunks) != 1 {
			t.Fatalf("got %d chunks, want 1", len(chunks))
		}
		// 8192 is not a multiple of 3: the cap must back up to 8190.
		if got := chunks[0].GetCharEnd(); got != 8190 {
			t.Errorf("CharEnd = %d, want 8190 (largest rune boundary <= %d)", got, singleChunkCapBytes)
		}
		verifyChunkInvariants(t, body, chunks, false)
	})

	t.Run("giant single ASCII token hard-cuts at the window", func(t *testing.T) {
		t.Parallel()
		body := strings.Repeat("a", 3*chunkWindowBytes)
		chunks, truncated := buildChunks(chunkDoc(askerv1.DocType_FILE, body))
		if truncated {
			t.Fatal("unexpected truncation")
		}
		if got := chunks[0].GetCharEnd(); got != chunkWindowBytes {
			t.Errorf("first hard cut at %d, want %d", got, chunkWindowBytes)
		}
		if got := chunks[1].GetCharStart(); got != chunkWindowBytes-chunkOverlapBytes {
			t.Errorf("second chunk starts at %d, want %d", got, chunkWindowBytes-chunkOverlapBytes)
		}
		verifyChunkInvariants(t, body, chunks, true)
	})
}

func TestEmailSectionsEdgeCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want []span
	}{
		{
			name: "body starting with a quote run is one section",
			body: "> quoted from the start\n> still quoted",
			want: []span{{0, 38}},
		},
		{
			name: "blank line between marker and quotes does not split them",
			body: "reply\n\nOn Jan 1 someone wrote:\n\n> quoted",
			want: []span{{0, 7}, {7, 40}},
		},
		{
			// Blank lines are transparent: a blank line inside a quoted
			// reply does not split it into two sections.
			name: "blank line between quote runs stays one section",
			body: "> run one\n\n> run two",
			want: []span{{0, 20}},
		},
		{
			name: "fresh text after a quote run starts no section",
			body: "> quoted\nfresh after quote\n\n> quoted again",
			want: []span{{0, 28}, {28, 42}},
		},
		{
			name: "no boundaries means one section",
			body: "just plain text\nwith two lines",
			want: []span{{0, 30}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := emailSections(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("emailSections = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("section %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
