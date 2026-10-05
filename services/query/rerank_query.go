package main

import (
	"context"
	"strings"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
)

// Conservative typo recovery affects only the cross-encoder's soft query text.
// It never changes retrieval, embeddings, typed hard predicates, or user input.
// A correction needs one unambiguous edit and support in at least two distinct
// authorized model-head titles; no external/global corpus vocabulary is used.
func correctRerankQuery(ctx context.Context, query string, head []*queryv1.Hit) string {
	const maxVocabulary = 2048
	const maxCorrections = 2
	const maxAnchoredCorrections = 3
	const maxQueryTokens = 40
	if ctx.Err() != nil || strings.ContainsAny(query, "\"':") {
		return query
	}
	words := strings.Fields(query)
	if len(words) > maxQueryTokens {
		return query
	}
	frequency := make(map[string]int)
	seenDocs := make(map[string]bool)
	for _, hit := range head {
		if ctx.Err() != nil {
			return query
		}
		id := hit.GetDocId()
		if id == "" || seenDocs[id] {
			continue
		}
		seenDocs[id] = true
		seenWords := make(map[string]bool)
		for _, word := range strings.FieldsFunc(strings.ToLower(truncateRunes(hit.GetTitle(), 256)), func(r rune) bool { return r < 'a' || r > 'z' }) {
			if !softCorrectionWord(word) || seenWords[word] {
				continue
			}
			seenWords[word] = true
			if _, exists := frequency[word]; exists || len(frequency) < maxVocabulary {
				frequency[word]++
			}
		}
	}
	// A third independent correction is allowed only when an unchanged content
	// term anchors the query in at least two authorized titles. Capitalized
	// names can provide that context but are never themselves rewritten.
	anchored := false
	for _, token := range words {
		word := strings.ToLower(strings.Trim(token, ".,?!()[]{};"))
		if softCorrectionWord(word) && frequency[word] >= 2 && !intentStopwords[word] && !correctionStopwords[word] {
			anchored = true
			break
		}
	}
	limit := maxCorrections
	if anchored {
		limit = maxAnchoredCorrections
	}
	corrections := 0
	for i, token := range words {
		if ctx.Err() != nil {
			return query
		}
		word := strings.Trim(token, ".,?!()[]{};")
		if !softCorrectionWord(word) || frequency[word] > 0 || intentStopwords[word] || correctionStopwords[word] {
			continue
		}
		candidate := ""
		ambiguous := false
		for known, count := range frequency {
			if count < 2 || !oneEditApart(word, known) {
				continue
			}
			if candidate != "" {
				ambiguous = true
				break
			}
			candidate = known
		}
		if candidate == "" || ambiguous {
			continue
		}
		words[i] = strings.Replace(token, word, candidate, 1)
		corrections++
		if corrections == limit {
			break
		}
	}
	if corrections == 0 {
		return query
	}
	return strings.Join(words, " ")
}

var correctionStopwords = map[string]bool{
	"after": true, "before": true, "because": true, "where": true, "which": true,
	"while": true, "would": true, "could": true, "these": true, "those": true,
	"their": true, "there": true, "other": true, "under": true, "until": true,
}

// Only lowercase ASCII content words qualify. Capitalized proper names,
// numbers, identifiers, addresses, URLs and punctuation-bearing literals do not.
func softCorrectionWord(word string) bool {
	if len(word) < 5 || len(word) > 24 {
		return false
	}
	for _, ch := range word {
		if ch < 'a' || ch > 'z' {
			return false
		}
	}
	return true
}

// oneEditApart recognizes one insertion, deletion, substitution or adjacent
// transposition, in linear time and without a dynamic-programming allocation.
func oneEditApart(a, b string) bool {
	if a == b || len(a)-len(b) > 1 || len(b)-len(a) > 1 {
		return false
	}
	if len(a) == len(b) {
		first := -1
		for i := range len(a) {
			if a[i] == b[i] {
				continue
			}
			if first < 0 {
				first = i
				continue
			}
			return i == first+1 && a[first] == b[i] && a[i] == b[first] && a[i+1:] == b[i+1:]
		}
		return first >= 0
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	i, j := 0, 0
	skipped := false
	for i < len(a) {
		if a[i] == b[j] {
			i++
			j++
			continue
		}
		if skipped {
			return false
		}
		skipped = true
		j++
	}
	return true
}
