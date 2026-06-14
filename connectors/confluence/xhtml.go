package confluence

import (
	"html"
	"strings"
)

// blockTags are storage-format / XHTML elements whose boundary becomes a
// newline when tags are stripped, so paragraphs and list items survive as
// chunking boundaries downstream.
var blockTags = map[string]bool{
	"br": true, "p": true, "div": true, "li": true, "ul": true, "ol": true,
	"tr": true, "table": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "blockquote": true, "pre": true,
	// Confluence storage-format structural macros: keep their text, break lines.
	"ac:layout": true, "ac:layout-section": true, "ac:layout-cell": true,
	"ac:structured-macro": true, "ac:rich-text-body": true,
}

// dropTags are elements whose entire contents are non-prose and must be dropped
// (markup, scripts, styles). Confluence wraps macro parameters and CDATA-style
// content in these.
var dropTags = []string{"script", "style", "ac:parameter"}

// stripStorageXHTML converts a Confluence storage-format (XHTML) body to plain
// searchable text: it drops script/style/macro-parameter blocks, removes all
// tags (turning block boundaries into newlines), unescapes entities, and tidies
// whitespace. It is not a sanitizer or a renderer — just enough to salvage text
// for indexing, mirroring the Gmail connector's HTML fallback.
func stripStorageXHTML(s string) string {
	if s == "" {
		return ""
	}
	s = dropElements(s, dropTags)

	var b strings.Builder
	b.Grow(len(s))
	for {
		open := strings.IndexByte(s, '<')
		if open < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:open])
		closeIdx := strings.IndexByte(s[open:], '>')
		if closeIdx < 0 {
			// Unterminated tag: drop the rest.
			break
		}
		if fields := strings.Fields(s[open+1 : open+closeIdx]); len(fields) > 0 {
			if tag := strings.ToLower(strings.Trim(fields[0], "/")); blockTags[tag] {
				b.WriteByte('\n')
			}
		}
		s = s[open+closeIdx+1:]
	}

	text := html.UnescapeString(b.String())
	lines := strings.Split(text, "\n")
	out := lines[:0]
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// dropElements removes each named element and its contents from s (matched
// case-insensitively on the opening tag name). An unterminated element drops
// everything from its start tag onward.
func dropElements(s string, names []string) string {
	for _, name := range names {
		s = dropElement(s, name)
	}
	return s
}

// dropElement removes every "<name ...>...</name>" span from s.
func dropElement(s, name string) string {
	open := "<" + name
	closeTag := "</" + name + ">"
	for {
		lower := strings.ToLower(s)
		start := strings.Index(lower, open)
		if start < 0 {
			return s
		}
		// Ensure we matched a tag boundary, not a longer tag name (e.g.
		// "<ac:parameter" must not match a hypothetical "<ac:parameterx").
		after := start + len(open)
		if after < len(s) {
			if ch := s[after]; ch != ' ' && ch != '>' && ch != '/' && ch != '\t' && ch != '\n' {
				// Not a real boundary; skip this occurrence by cutting past it.
				s = s[:start] + s[after:]
				continue
			}
		}
		end := strings.Index(lower[start:], closeTag)
		if end < 0 {
			// Unterminated: drop to end.
			return s[:start]
		}
		cut := start + end + len(closeTag)
		s = s[:start] + s[cut:]
	}
}
