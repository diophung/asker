package jira

import (
	"encoding/json"
	"strings"
)

// Atlassian Document Format (ADF) -> plain text.
//
// ADF is the JSON rich-text format Jira Cloud returns for issue descriptions
// and comments. The document is a tree:
//
//	{"type":"doc","version":1,"content":[ ...block nodes... ]}
//
// Each node has a "type", an optional "content" array of child nodes, an
// optional "text" string (only on leaf "text" nodes), and optional "attrs".
// Block nodes (paragraph, heading, bulletList, listItem, blockquote,
// codeBlock, panel, table rows/cells, ...) group children; inline/leaf nodes
// (text, hardBreak, mention, emoji, ...) carry the actual characters.
//
// The walk below concatenates the text of every leaf in document order,
// inserting a newline between block-level nodes so paragraphs and list items
// stay on separate lines (which downstream chunking relies on). It is
// deliberately lossy: marks (bold/links/color), media, and layout produce no
// markup — only the human-readable text and link/mention/emoji labels survive.
//
// A description field may also be a plain string (older Jira, or the
// connector being pointed at a wiki-renderer-disabled site); adfText handles
// that too via adfNode.UnmarshalJSON.

// adfNode is one ADF node. It unmarshals from either an object (the normal
// case) or a bare JSON string (a plain-text description), so callers do not
// need to know which a field holds.
type adfNode struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Content []*adfNode      `json:"content"`
	Attrs   json.RawMessage `json:"attrs"`

	// plain holds a description delivered as a bare JSON string rather than an
	// ADF object.
	plain string
}

// UnmarshalJSON accepts either an ADF object or a bare string. A JSON null
// leaves the node empty.
func (n *adfNode) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" || trimmed == "" {
		return nil
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		n.plain = s
		return nil
	}
	// Avoid infinite recursion by unmarshalling into a type without the custom
	// method.
	type alias adfNode
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*n = adfNode(a)
	return nil
}

// blockTypes are ADF node types whose rendered text ends a line, so adjacent
// blocks (paragraphs, headings, list items, table cells, ...) do not run
// together.
var blockTypes = map[string]bool{
	"paragraph":   true,
	"heading":     true,
	"blockquote":  true,
	"bulletList":  true,
	"orderedList": true,
	"listItem":    true,
	"codeBlock":   true,
	"panel":       true,
	"rule":        true,
	"tableRow":    true,
	"tableCell":   true,
	"tableHeader": true,
	"mediaGroup":  true,
	"mediaSingle": true,
}

// adfText renders an ADF node tree (or a plain-string description) to plain
// text. A nil node yields "".
func adfText(n *adfNode) string {
	if n == nil {
		return ""
	}
	if n.plain != "" {
		return strings.TrimSpace(n.plain)
	}
	var b strings.Builder
	walkADF(n, &b)
	return normalizeText(b.String())
}

// walkADF appends the text of n and its descendants to b in document order,
// emitting a newline after each block-level node.
func walkADF(n *adfNode, b *strings.Builder) {
	if n == nil {
		return
	}
	switch n.Type {
	case "text":
		b.WriteString(n.Text)
	case "hardBreak":
		b.WriteByte('\n')
	case "mention", "emoji", "inlineCard":
		b.WriteString(attrLabel(n.Attrs))
	}

	for _, child := range n.Content {
		walkADF(child, b)
	}

	if blockTypes[n.Type] {
		b.WriteByte('\n')
	}
}

// attrText is the subset of node attrs that carry human-readable text for
// inline nodes without a "text" leaf (mention.text, emoji.text/shortName,
// inlineCard.url).
type attrText struct {
	Text      string `json:"text"`
	ShortName string `json:"shortName"`
	URL       string `json:"url"`
}

// attrLabel extracts a display label from an inline node's attrs: the mention
// text, the emoji text/short name, or an inline-card URL.
func attrLabel(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var a attrText
	if err := json.Unmarshal(raw, &a); err != nil {
		return ""
	}
	switch {
	case a.Text != "":
		return a.Text
	case a.ShortName != "":
		return a.ShortName
	case a.URL != "":
		return a.URL
	default:
		return ""
	}
}

// normalizeText trims each line, drops blank lines produced by empty block
// nodes, and joins with single newlines so the body is compact and stable.
func normalizeText(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		line = strings.TrimLeft(line, " \t")
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
