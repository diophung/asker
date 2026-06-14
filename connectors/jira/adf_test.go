package jira

import (
	"encoding/json"
	"testing"
)

// parseADF unmarshals an ADF JSON literal into an *adfNode for tests.
func parseADF(t *testing.T, raw string) *adfNode {
	t.Helper()
	var n adfNode
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatalf("unmarshal ADF: %v", err)
	}
	return &n
}

func TestADFText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		adf  string
		want string
	}{
		{
			name: "nil node",
			adf:  "null",
			want: "",
		},
		{
			name: "plain string description",
			adf:  `"just a plain string"`,
			want: "just a plain string",
		},
		{
			name: "single paragraph",
			adf:  `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Hello world"}]}]}`,
			want: "Hello world",
		},
		{
			name: "paragraph with marks keeps only text",
			adf:  `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"bold","marks":[{"type":"strong"}]},{"type":"text","text":" and normal"}]}]}`,
			want: "bold and normal",
		},
		{
			name: "two paragraphs separated by newline",
			adf:  `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"first"}]},{"type":"paragraph","content":[{"type":"text","text":"second"}]}]}`,
			want: "first\nsecond",
		},
		{
			name: "heading and bullet list",
			adf:  `{"type":"doc","content":[{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Steps"}]},{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"one"}]}]},{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"two"}]}]}]}]}`,
			want: "Steps\none\ntwo",
		},
		{
			name: "hard break inside paragraph",
			adf:  `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"line one"},{"type":"hardBreak"},{"type":"text","text":"line two"}]}]}`,
			want: "line one\nline two",
		},
		{
			name: "mention renders its display text",
			adf:  `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"cc "},{"type":"mention","attrs":{"id":"acc-1","text":"@Ben Carter"}}]}]}`,
			want: "cc @Ben Carter",
		},
		{
			name: "emoji renders short name when no text",
			adf:  `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"nice "},{"type":"emoji","attrs":{"shortName":":thumbsup:"}}]}]}`,
			want: "nice :thumbsup:",
		},
		{
			name: "inline card renders its url",
			adf:  `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"see "},{"type":"inlineCard","attrs":{"url":"https://example.com/x"}}]}]}`,
			want: "see https://example.com/x",
		},
		{
			name: "code block text preserved",
			adf:  `{"type":"doc","content":[{"type":"codeBlock","attrs":{"language":"go"},"content":[{"type":"text","text":"fmt.Println(1)"}]}]}`,
			want: "fmt.Println(1)",
		},
		{
			name: "empty paragraph produces no blank line",
			adf:  `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"a"}]},{"type":"paragraph"},{"type":"paragraph","content":[{"type":"text","text":"b"}]}]}`,
			want: "a\nb",
		},
		{
			name: "nested blockquote",
			adf:  `{"type":"doc","content":[{"type":"blockquote","content":[{"type":"paragraph","content":[{"type":"text","text":"quoted"}]}]}]}`,
			want: "quoted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := adfText(parseADF(t, tt.adf))
			if got != tt.want {
				t.Errorf("adfText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestADFTextNilPointer(t *testing.T) {
	t.Parallel()
	if got := adfText(nil); got != "" {
		t.Errorf("adfText(nil) = %q, want empty", got)
	}
}
