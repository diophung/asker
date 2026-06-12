package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// Chunking policy (spec §2.4, pinned M1 contract):
//
//   - ~512-token windows approximated as 2048 bytes, with a 64-token
//     (256-byte) overlap between consecutive windows.
//   - Structure-aware: EMAIL bodies are first split into sections at
//     quoted-reply boundaries ("On ... wrote:" marker lines, and the first
//     line of each run of '>'-quoted lines); windows never span sections and
//     a section smaller than one window stays a single chunk. CHAT_MESSAGE
//     and CALENDAR_EVENT are always a single chunk (capped at 8192 bytes).
//     Everything else is windowed preferring paragraph boundaries, then line
//     boundaries, then word boundaries; a chunk is cut mid-word only when
//     the window contains no whitespace at all.
//   - Chunk.CharStart / Chunk.CharEnd are BYTE offsets into Document.BodyText
//     ([start, end) slicing the body exactly to the chunk text). The proto
//     comment says "char offsets"; this service interprets and produces them
//     as byte offsets — Go strings are indexed by byte, and byte offsets are
//     unambiguous for downstream consumers in any language, whereas "char"
//     (rune? UTF-16 unit?) is not. Cuts always land on UTF-8 rune
//     boundaries, so every chunk is valid UTF-8.
//   - At most 64 chunks per document; the remainder of the body is dropped
//     from retrieval (the caller logs the truncation).
const (
	// chunkWindowBytes approximates ~512 tokens at ~4 bytes/token.
	chunkWindowBytes = 2048
	// chunkOverlapBytes approximates the 64-token overlap.
	chunkOverlapBytes = 256
	// maxChunksPerDoc caps retrieval units per document.
	maxChunksPerDoc = 64
	// singleChunkCapBytes bounds the one chunk of CHAT_MESSAGE /
	// CALENDAR_EVENT documents, which are never windowed.
	singleChunkCapBytes = 8192
)

// span is a half-open [start, end) byte range into the document body.
type span struct {
	start, end int
}

// quoteMarkerRe matches attribution lines such as
// "On Mon, Jun 2, 2025 at 9:14 AM Alice <alice@example.com> wrote:".
var quoteMarkerRe = regexp.MustCompile(`^On .+ wrote:\s*$`)

// buildChunks cuts doc.BodyText into retrieval chunks per the policy above.
// It returns the chunks (nil for an empty body — title-only documents are
// legal and still flow through the pipeline) and whether the 64-chunk cap
// truncated the output.
func buildChunks(doc *askerv1.Document) ([]*askerv1.Chunk, bool) {
	body := doc.GetBodyText()
	if body == "" {
		return nil, false
	}

	var spans []span
	switch doc.GetType() {
	case askerv1.DocType_CHAT_MESSAGE, askerv1.DocType_CALENDAR_EVENT:
		spans = []span{{0, truncateRuneSafe(body, singleChunkCapBytes)}}
	case askerv1.DocType_EMAIL:
		for _, sec := range emailSections(body) {
			spans = appendWindowSpans(spans, body, sec)
		}
	default:
		spans = appendWindowSpans(spans, body, span{0, len(body)})
	}

	truncated := false
	if len(spans) > maxChunksPerDoc {
		spans = spans[:maxChunksPerDoc]
		truncated = true
	}

	chunks := make([]*askerv1.Chunk, len(spans))
	for i, sp := range spans {
		chunks[i] = &askerv1.Chunk{
			ChunkId:   fmt.Sprintf("%s#%d", doc.GetDocId(), i),
			Text:      body[sp.start:sp.end],
			CharStart: int64(sp.start), // byte offset into BodyText, inclusive
			CharEnd:   int64(sp.end),   // byte offset into BodyText, exclusive
		}
	}
	return chunks, truncated
}

// appendWindowSpans windows one section of body into overlapping spans and
// appends them to spans. A section that fits in one window stays one chunk;
// overlap never reaches across a section boundary.
func appendWindowSpans(spans []span, body string, sec span) []span {
	start := sec.start
	for start < sec.end {
		end := cutPoint(body, start, sec.end)
		spans = append(spans, span{start, end})
		if end >= sec.end {
			break
		}
		start = nextWindowStart(body, start, end)
	}
	return spans
}

// cutPoint picks the end of the window starting at start within the section
// ending at secEnd. Preference order: take the whole remainder when it fits;
// otherwise cut after the last paragraph break in the window, else after the
// last line break, else after the last word break, else hard-cut at the
// window limit (single giant token; mid-word is unavoidable). Soft cuts must
// leave a chunk longer than the overlap so the next window always advances.
func cutPoint(body string, start, secEnd int) int {
	if secEnd-start <= chunkWindowBytes {
		return secEnd
	}
	limit := start + chunkWindowBytes
	for limit > start && !utf8.RuneStart(body[limit]) {
		limit-- // never split inside a rune
	}
	window := body[start:limit]

	if i := strings.LastIndex(window, "\n\n"); i >= 0 && i+2 > chunkOverlapBytes {
		return start + i + 2
	}
	if i := strings.LastIndexByte(window, '\n'); i >= 0 && i+1 > chunkOverlapBytes {
		return start + i + 1
	}
	if i := strings.LastIndexAny(window, " \t"); i >= 0 && i+1 > chunkOverlapBytes {
		return start + i + 1
	}
	return limit
}

// nextWindowStart returns where the window after [prevStart, prevEnd) begins:
// chunkOverlapBytes before the previous end, advanced to a rune boundary and
// then to the next word start when one exists inside the overlap region
// (never past prevEnd, so no byte of the body is skipped — everything between
// the returned offset and prevEnd is already covered by the previous chunk).
// Texts without whitespace in the overlap (e.g. CJK) keep the rune-aligned
// position so the overlap is preserved.
func nextWindowStart(body string, prevStart, prevEnd int) int {
	next := prevEnd - chunkOverlapBytes
	if next <= prevStart {
		next = prevStart + 1
	}
	for next < prevEnd && !utf8.RuneStart(body[next]) {
		next++
	}
	if ws := wordStartWithin(body, next, prevEnd); ws >= 0 {
		return ws
	}
	return next
}

// wordStartWithin returns the first word-start offset in [from, limit), or -1
// when from is already at a word start or no word starts in the range.
func wordStartWithin(body string, from, limit int) int {
	if from <= 0 || isSpaceByte(body[from-1]) || isSpaceByte(body[from]) {
		return -1 // already at a word start (or at whitespace, a clean cut)
	}
	i := from
	for i < limit && !isSpaceByte(body[i]) {
		i++ // skip the tail of the word the previous chunk already covers
	}
	for i < limit && isSpaceByte(body[i]) {
		i++ // skip the whitespace run
	}
	if i >= limit {
		return -1
	}
	return i
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// emailSections splits an email body into sections at quoted-reply
// boundaries. A new section starts at an "On ... wrote:" marker line and at
// the first line of a run of '>'-quoted lines; a quote run directly following
// a marker (blank lines in between are transparent) stays in the marker's
// section, so an attribution line and the text it attributes chunk together.
func emailSections(body string) []span {
	bounds := []int{0}
	prevSticky := false // previous non-blank line was quoted or a marker
	offset := 0
	for offset < len(body) {
		lineEnd := len(body)
		next := len(body)
		if nl := strings.IndexByte(body[offset:], '\n'); nl >= 0 {
			lineEnd = offset + nl
			next = lineEnd + 1
		}
		line := body[offset:lineEnd]

		if strings.TrimSpace(line) != "" {
			quoted := strings.HasPrefix(strings.TrimLeft(line, " \t"), ">")
			marker := quoteMarkerRe.MatchString(line)
			if offset > 0 && (marker || (quoted && !prevSticky)) {
				bounds = append(bounds, offset)
			}
			prevSticky = quoted || marker
		}
		offset = next
	}

	spans := make([]span, 0, len(bounds))
	for i, b := range bounds {
		end := len(body)
		if i+1 < len(bounds) {
			end = bounds[i+1]
		}
		if end > b {
			spans = append(spans, span{b, end})
		}
	}
	return spans
}

// truncateRuneSafe returns the largest end <= cap such that body[:end] is
// whole runes, or len(body) when it already fits.
func truncateRuneSafe(body string, capBytes int) int {
	if len(body) <= capBytes {
		return len(body)
	}
	end := capBytes
	for end > 0 && !utf8.RuneStart(body[end]) {
		end--
	}
	return end
}
