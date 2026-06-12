package main

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

func TestNormalizeTitle(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"plain", "Quarterly Report", "Quarterly Report"},
		{"surrounding whitespace", "  hello \t", "hello"},
		{"internal runs collapse", "a  \t b\n\nc", "a b c"},
		{"only whitespace", " \n\t ", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeTitle(tc.in); got != tc.want {
				t.Errorf("normalizeTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeBody(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"crlf to lf", "a\r\nb\rc", "a\nb\nc"},
		{"trailing line whitespace stripped", "a  \nb\t\nc", "a\nb\nc"},
		{"blank line runs collapse to one", "a\n\n\n\n\nb", "a\n\nb"},
		{"single blank line preserved", "para one\n\npara two", "para one\n\npara two"},
		{"quote prefixes preserved", "> quoted\n> more", "> quoted\n> more"},
		{"surrounding whitespace trimmed", "\n\n  body  \n\n", "body"},
		{"only whitespace", " \r\n\t ", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeBody(tc.in); got != tc.want {
				t.Errorf("normalizeBody(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeDocumentDerivesEtag(t *testing.T) {
	t.Parallel()
	doc := &askerv1.Document{
		Title:    "  Hello\n World ",
		BodyText: "line one  \r\nline two\r\n\r\n\r\n\r\nline three",
	}
	normalizeDocument(doc)

	if doc.GetTitle() != "Hello World" {
		t.Errorf("Title = %q, want %q", doc.GetTitle(), "Hello World")
	}
	wantBody := "line one\nline two\n\nline three"
	if doc.GetBodyText() != wantBody {
		t.Errorf("BodyText = %q, want %q", doc.GetBodyText(), wantBody)
	}
	sum := sha256.Sum256([]byte(doc.GetTitle() + doc.GetBodyText()))
	if want := hex.EncodeToString(sum[:]); doc.GetVersionEtag() != want {
		t.Errorf("VersionEtag = %q, want sha256 of normalized title+body %q", doc.GetVersionEtag(), want)
	}

	// Determinism: a replay of the same raw content derives the same etag.
	again := &askerv1.Document{Title: "  Hello\n World ", BodyText: "line one  \r\nline two\r\n\r\n\r\n\r\nline three"}
	normalizeDocument(again)
	if again.GetVersionEtag() != doc.GetVersionEtag() {
		t.Errorf("etag not deterministic: %q vs %q", again.GetVersionEtag(), doc.GetVersionEtag())
	}
}

func TestNormalizeDocumentKeepsConnectorEtag(t *testing.T) {
	t.Parallel()
	doc := &askerv1.Document{Title: "t", BodyText: "b", VersionEtag: "etag-from-source"}
	normalizeDocument(doc)
	if doc.GetVersionEtag() != "etag-from-source" {
		t.Errorf("VersionEtag = %q, want connector-provided etag preserved", doc.GetVersionEtag())
	}
}
