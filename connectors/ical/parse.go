package ical

import (
	"strings"
)

// This file is the core deliverable: a hand-written RFC 5545 (iCalendar) parser.
// No third-party ics library is vendored — we parse the text ourselves. The
// parser is deliberately small and forgiving: it extracts the VEVENT components
// and the properties this connector maps, and ignores everything else (VTODO,
// VTIMEZONE, VALARM, x-properties, ...) rather than failing a whole feed over an
// unrecognized line.
//
// # What it handles
//
//   - Line unfolding (RFC 5545 §3.1): a CRLF (or LF) followed by a single space
//     or horizontal tab is a continuation of the previous line; the linear
//     whitespace is removed and the lines are joined.
//   - Component nesting: the feed is split into VCALENDAR/VEVENT (and other)
//     BEGIN:/END: blocks; only VEVENT blocks are returned.
//   - Property parameters (RFC 5545 §3.2): a content line is
//     NAME[;PARAM=VALUE[;PARAM=VALUE...]]:VALUE — e.g.
//     DTSTART;TZID=America/Los_Angeles:20260603T093000. Parameter values may be
//     double-quoted (which lets them contain ':' and ';').
//   - Text-value escaping (RFC 5545 §3.3.11): \\ -> backslash, \n / \N ->
//     newline, \; -> ';', \, -> ',' in TEXT-typed values (SUMMARY, DESCRIPTION,
//     LOCATION, ...).
//
// # What is deferred
//
//   - Recurrence expansion (RRULE / RDATE / EXDATE): for M2 we emit only the
//     master VEVENT and surface its RRULE verbatim in metadata. Expanding a
//     recurrence rule into individual occurrences is deferred to a later
//     milestone (see README "Recurrence").

// property is one parsed iCalendar content line: a name, its parameters, and its
// (already unescaped, for TEXT values) value.
type property struct {
	name   string
	params map[string]string
	value  string
}

// param returns the named parameter (case-insensitive) and whether it was set.
func (p property) param(name string) (string, bool) {
	v, ok := p.params[strings.ToUpper(name)]
	return v, ok
}

// vevent is one parsed VEVENT component: its properties in file order, plus a
// raw copy of the unfolded block lines so a fingerprint version_etag can be
// computed when the event carries neither SEQUENCE nor LAST-MODIFIED.
type vevent struct {
	props []property
	raw   string
}

// get returns the first property with the given (case-insensitive) name.
func (e *vevent) get(name string) (property, bool) {
	up := strings.ToUpper(name)
	for _, p := range e.props {
		if p.name == up {
			return p, true
		}
	}
	return property{}, false
}

// getAll returns every property with the given (case-insensitive) name, in file
// order (ATTENDEE repeats, for example).
func (e *vevent) getAll(name string) []property {
	up := strings.ToUpper(name)
	var out []property
	for _, p := range e.props {
		if p.name == up {
			out = append(out, p)
		}
	}
	return out
}

// value returns the (unescaped) value of the first property with name, or "".
func (e *vevent) value(name string) string {
	if p, ok := e.get(name); ok {
		return p.value
	}
	return ""
}

// parseEvents unfolds the feed body and returns every top-level VEVENT it
// contains, in document order. Components other than VEVENT (VTIMEZONE, VTODO,
// VALARM, ...) are skipped. A VEVENT nested in a VALARM cannot occur in RFC 5545,
// so a simple depth counter that only collects at VEVENT depth is sufficient.
func parseEvents(body string) []*vevent {
	lines := unfold(body)

	var (
		events  []*vevent
		stack   []string // names of currently-open BEGIN: components
		cur     *vevent  // the VEVENT being collected, when inside one
		rawLine []string // raw unfolded lines of cur, for fingerprinting
	)

	for _, line := range lines {
		if line == "" {
			continue
		}
		name, _, value := splitLine(line)
		upper := strings.ToUpper(name)

		switch upper {
		case "BEGIN":
			comp := strings.ToUpper(strings.TrimSpace(value))
			stack = append(stack, comp)
			if comp == "VEVENT" && cur == nil {
				cur = &vevent{}
				rawLine = nil
			}
			continue
		case "END":
			comp := strings.ToUpper(strings.TrimSpace(value))
			if len(stack) > 0 && stack[len(stack)-1] == comp {
				stack = stack[:len(stack)-1]
			}
			if comp == "VEVENT" && cur != nil {
				cur.raw = strings.Join(rawLine, "\n")
				events = append(events, cur)
				cur = nil
				rawLine = nil
			}
			continue
		}

		// Only collect property lines while we are directly inside a VEVENT.
		if cur != nil && len(stack) > 0 && stack[len(stack)-1] == "VEVENT" {
			cur.props = append(cur.props, parseProperty(line))
			rawLine = append(rawLine, line)
		}
	}
	return events
}

// unfold splits the body into logical lines, applying RFC 5545 §3.1 line
// unfolding: a line that begins with a single space or tab is a continuation of
// the preceding line and the leading whitespace character is removed. Both CRLF
// and bare LF line endings are accepted; a trailing CR is trimmed.
func unfold(body string) []string {
	rawLines := strings.Split(body, "\n")
	var out []string
	for _, raw := range rawLines {
		raw = strings.TrimSuffix(raw, "\r")
		if raw == "" {
			out = append(out, "")
			continue
		}
		if (raw[0] == ' ' || raw[0] == '\t') && len(out) > 0 {
			// Continuation: append (minus the one leading whitespace char) to
			// the previous logical line.
			out[len(out)-1] += raw[1:]
			continue
		}
		out = append(out, raw)
	}
	return out
}

// splitLine splits a content line into its name part (everything before the
// first unquoted ':'), the parameter segment (everything between the name's
// first ';' and the value ':'), and the raw value (after the ':'). The split
// honors double-quoted parameter values, which may contain ':' and ';'.
func splitLine(line string) (name, paramSeg, value string) {
	colon := unquotedIndex(line, ':')
	if colon < 0 {
		// No value separator (e.g. a malformed line): treat the whole thing as
		// a valueless name.
		head := line
		if semi := strings.IndexByte(head, ';'); semi >= 0 {
			return head[:semi], head[semi+1:], ""
		}
		return head, "", ""
	}
	head := line[:colon]
	value = line[colon+1:]
	if semi := strings.IndexByte(head, ';'); semi >= 0 {
		return head[:semi], head[semi+1:], value
	}
	return head, "", value
}

// parseProperty parses one content line into a property with an unescaped TEXT
// value. The property name is upper-cased for case-insensitive lookup.
func parseProperty(line string) property {
	name, paramSeg, value := splitLine(line)
	return property{
		name:   strings.ToUpper(strings.TrimSpace(name)),
		params: parseParams(paramSeg),
		value:  unescapeText(value),
	}
}

// parseParams parses the ";"-separated parameter segment of a content line into
// a name->value map (parameter names upper-cased). A double-quoted value is
// unquoted. Multi-valued parameters (comma-separated) keep their raw value; this
// connector does not need to split them.
func parseParams(seg string) map[string]string {
	params := map[string]string{}
	if seg == "" {
		return params
	}
	for _, part := range splitUnquoted(seg, ';') {
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			// A bare flag parameter (no '='): record it with an empty value.
			params[strings.ToUpper(strings.TrimSpace(part))] = ""
			continue
		}
		key := strings.ToUpper(strings.TrimSpace(part[:eq]))
		val := unquoteParam(part[eq+1:])
		params[key] = val
	}
	return params
}

// unquoteParam strips one layer of surrounding double quotes from a parameter
// value, leaving an unquoted value unchanged.
func unquoteParam(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

// unescapeText reverses RFC 5545 §3.3.11 TEXT escaping: \\ -> '\', \n and \N ->
// newline, \; -> ';', \, -> ','. An unknown escape keeps the escaped character
// verbatim (dropping the backslash), which is the lenient behavior most feeds
// expect.
func unescapeText(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i == len(s)-1 {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case 'n', 'N':
			b.WriteByte('\n')
		case '\\', ';', ',':
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// unquotedIndex returns the index of the first occurrence of ch in s that is not
// inside a double-quoted run, or -1.
func unquotedIndex(s string, ch byte) int {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case ch:
			if !inQuote {
				return i
			}
		}
	}
	return -1
}

// splitUnquoted splits s on every unquoted occurrence of sep, leaving separators
// inside double-quoted runs intact.
func splitUnquoted(s string, sep byte) []string {
	var (
		out     []string
		start   int
		inQuote bool
	)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case sep:
			if !inQuote {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}
