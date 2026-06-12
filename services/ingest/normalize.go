package main

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

var (
	wsRunRe         = regexp.MustCompile(`\s+`)
	trailingWSRe    = regexp.MustCompile(`[ \t]+\n`)
	blankLinesRunRe = regexp.MustCompile(`\n{3,}`)
)

// normalizeDocument trims absurd whitespace from the title and body and
// ensures VersionEtag is set, deriving it as the hex SHA-256 of title+body
// when the connector left it empty. It runs BEFORE chunking, so chunk byte
// offsets refer to the normalized BodyText that travels downstream, and it is
// deterministic, so replays of the same raw record derive the same etag and
// the Redis dedupe key holds.
func normalizeDocument(doc *askerv1.Document) {
	doc.Title = normalizeTitle(doc.GetTitle())
	doc.BodyText = normalizeBody(doc.GetBodyText())
	if doc.GetVersionEtag() == "" {
		sum := sha256.Sum256([]byte(doc.GetTitle() + doc.GetBodyText()))
		doc.VersionEtag = hex.EncodeToString(sum[:])
	}
}

// normalizeTitle collapses all whitespace runs (including newlines) to single
// spaces and trims the ends.
func normalizeTitle(title string) string {
	return strings.TrimSpace(wsRunRe.ReplaceAllString(title, " "))
}

// normalizeBody normalizes line endings to \n, strips trailing whitespace
// from each line, collapses runs of 3+ newlines to one blank line, and trims
// leading/trailing whitespace. Paragraph structure (single blank lines) and
// '>' quote prefixes are preserved for the structure-aware chunker.
func normalizeBody(body string) string {
	if body == "" {
		return ""
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	body = trailingWSRe.ReplaceAllString(body, "\n")
	body = blankLinesRunRe.ReplaceAllString(body, "\n\n")
	return strings.TrimSpace(body)
}
