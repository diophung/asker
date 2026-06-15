package main

import (
	"fmt"
	"math"
	"net/http"
	"net/mail"
	"sort"
	"strings"
	"time"

	queryv1 "github.com/asker/asker/platform/proto/gen/go/asker/query/v1"
	documentv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

const (
	// peopleScanLimit is how many candidate document hits the People tab
	// aggregates contacts from. People are derived, so we want a generous
	// window of the most relevant human-bearing documents.
	peopleScanLimit = 100
	// peopleMaxResults caps the returned people.
	peopleMaxResults = 20
	// peopleHalfLifeDays controls the recency boost in the people ranking.
	peopleHalfLifeDays = 30.0
)

// personJSON is one derived contact in the People-tab response.
type personJSON struct {
	Name          string `json:"name"`
	Email         string `json:"email"`
	Count         int    `json:"count"`
	LastContacted string `json:"last_contacted"` // RFC3339, or "" when unknown
	Summary       string `json:"summary"`        // e.g. "12 emails · 3 messages"
}

// peopleResponseJSON is the People-tab wire shape (distinct from the document
// search shape: people are an aggregation, not document hits).
type peopleResponseJSON struct {
	People []personJSON `json:"people"`
	Total  int64        `json:"total"`
	TookMs int64        `json:"took_ms"`
}

// handlePeopleSearch serves GET /v1/search/people. There is no person index, so
// people are DERIVED: it runs a tenant-scoped search over the human-bearing
// document types (email/chat/calendar) and aggregates the senders, recipients,
// and attendees of the matching documents into ranked contacts. The tenant
// rides the request context exactly as for /v1/search.
func (d *deps) handlePeopleSearch(w http.ResponseWriter, r *http.Request) {
	req, err := parseSearchRequest(r.URL.Query(), d.maxQueryChars)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Aggregate people from the human-bearing types over a generous candidate
	// window; limit/offset here size the document scan, not the people page.
	req.DocTypes = []documentv1.DocType{
		documentv1.DocType_EMAIL,
		documentv1.DocType_CHAT_MESSAGE,
		documentv1.DocType_CALENDAR_EVENT,
	}
	req.Limit = peopleScanLimit
	req.Offset = 0

	start := time.Now()
	resp, err := d.query.Search(r.Context(), req)
	if err != nil {
		d.upstreamError(w, r, "QueryService.Search", err)
		return
	}
	d.recordRecent(r.Context(), req.GetQuery())

	people := aggregatePeople(resp.GetHits(), req.GetQuery(), time.Now())
	if people == nil {
		people = []personJSON{}
	}
	writeJSON(w, http.StatusOK, peopleResponseJSON{
		People: people,
		Total:  int64(len(people)),
		TookMs: time.Since(start).Milliseconds(),
	})
}

// personRef is one parsed mention of a person (a name and/or email).
type personRef struct {
	name  string
	email string
}

// personAgg accumulates one contact across many document hits.
type personAgg struct {
	name  string
	email string
	count int
	last  time.Time
	byCat map[string]int // "email" | "message" | "event" -> count
}

// aggregatePeople turns document hits into ranked contacts. People whose name or
// email matches the query come first (a name search like "sarah"); if none
// match (a topic search like "q3 planning"), it falls back to the people who
// appear most in the relevant results (relatedness). Ranking blends frequency
// (diminishing) with recency of last contact. `now` is injected for determinism.
func aggregatePeople(hits []*queryv1.Hit, query string, now time.Time) []personJSON {
	terms := strings.Fields(strings.ToLower(query))
	aggs := map[string]*personAgg{}

	for _, h := range hits {
		md := h.GetMetadata()
		cat := personCategory(h.GetType())
		when := hitGatewayTime(h)
		for _, ref := range peopleFromHit(md, h.GetType()) {
			key := ref.email
			if key == "" {
				key = strings.ToLower(ref.name)
			}
			if key == "" {
				continue
			}
			a := aggs[key]
			if a == nil {
				a = &personAgg{email: ref.email, byCat: map[string]int{}}
				aggs[key] = a
			}
			a.name = betterName(a.name, ref.name)
			a.count++
			if cat != "" {
				a.byCat[cat]++
			}
			if when.After(a.last) {
				a.last = when
			}
		}
	}

	matched := make([]*personAgg, 0, len(aggs))
	other := make([]*personAgg, 0, len(aggs))
	for _, a := range aggs {
		if personMatches(a, terms) {
			matched = append(matched, a)
		} else {
			other = append(other, a)
		}
	}
	pick := matched
	if len(pick) == 0 {
		pick = other // topic query: surface related people instead of nothing
	}

	sort.SliceStable(pick, func(i, j int) bool {
		si, sj := personScore(pick[i], now), personScore(pick[j], now)
		if si != sj {
			return si > sj
		}
		// Fully deterministic tiebreak: distinct people can share a score AND a
		// display name (e.g. john@team1.com vs john@team2.com both render
		// "john"); without a final discriminator their order — and thus which
		// survives the peopleMaxResults cutoff — would leak map-iteration order.
		// email is the unique dedup key when present; name-only aggs differ by name.
		if ni, nj := displayPersonName(pick[i]), displayPersonName(pick[j]); ni != nj {
			return ni < nj
		}
		return pick[i].email < pick[j].email
	})
	if len(pick) > peopleMaxResults {
		pick = pick[:peopleMaxResults]
	}

	out := make([]personJSON, 0, len(pick))
	for _, a := range pick {
		out = append(out, a.toJSON())
	}
	return out
}

// peopleFromHit extracts the people mentioned by a document's metadata, by type:
// email from/to/cc, chat sender, calendar organizer + attendees (the
// response_status:<addr> keys the connectors record, plus an attendees list).
func peopleFromHit(md map[string]string, dtype documentv1.DocType) []personRef {
	if md == nil {
		return nil
	}
	var refs []personRef
	add := func(s string) {
		if s != "" {
			refs = append(refs, parsePeople(s)...)
		}
	}
	switch dtype {
	case documentv1.DocType_EMAIL:
		add(md["from"])
		add(md["to"])
		add(md["cc"])
	case documentv1.DocType_CHAT_MESSAGE:
		if md["from"] != "" {
			add(md["from"])
		} else {
			add(md["sender"])
		}
	case documentv1.DocType_CALENDAR_EVENT:
		add(md["organizer"])
		add(md["attendees"])
		for k := range md {
			if strings.HasPrefix(k, "response_status:") {
				add(strings.TrimPrefix(k, "response_status:"))
			}
		}
	}
	return refs
}

// parsePeople parses an address-list-shaped string ("Sarah Chen
// <sarah@acme.com>, bob@x.com") into person refs. A value that is not a valid
// RFC-5322 address list (e.g. a bare Slack display name) becomes a single
// name-only ref.
func parsePeople(s string) []personRef {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// Fast path: a well-formed address list.
	if addrs, err := mail.ParseAddressList(s); err == nil && len(addrs) > 0 {
		return refsFromAddrs(addrs)
	}
	// mail.ParseAddressList rejects the WHOLE list if ANY single entry is
	// malformed — and an unquoted-comma display name ("Lastname, Firstname
	// <addr>") is a common real-world header that trips it. When the value looks
	// like a list (has a comma), salvage the entries that DO parse so one bad
	// recipient never discards the rest, nor collapses the raw header into one
	// junk contact. Stray fragments with no parseable address are dropped (their
	// real owner is recovered from the sibling fragment that carries the email).
	if strings.Contains(s, ",") {
		var out []personRef
		for _, frag := range strings.Split(s, ",") {
			if frag = strings.TrimSpace(frag); frag == "" {
				continue
			}
			if a, err := mail.ParseAddress(frag); err == nil {
				out = append(out, refFromAddr(a))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	// A comma-free bare value (e.g. a chat display name), or a list where nothing
	// parsed: treat the whole string as a single name-only contact.
	return []personRef{{name: s}}
}

func refsFromAddrs(addrs []*mail.Address) []personRef {
	out := make([]personRef, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, refFromAddr(a))
	}
	return out
}

func refFromAddr(a *mail.Address) personRef {
	email := strings.ToLower(strings.TrimSpace(a.Address))
	name := strings.TrimSpace(a.Name)
	if name == "" {
		name = displayNameFromEmail(email)
	}
	return personRef{name: name, email: email}
}

func displayNameFromEmail(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

// betterName keeps the more useful display name: a non-empty one over empty, and
// a multi-word full name over a single token.
func betterName(stored, candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return stored
	}
	if stored == "" {
		return candidate
	}
	if strings.Contains(candidate, " ") && !strings.Contains(stored, " ") {
		return candidate
	}
	return stored
}

func personMatches(a *personAgg, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	hay := strings.ToLower(a.name + " " + a.email)
	for _, t := range terms {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// personScore blends frequency (log, diminishing returns) with a recency boost
// (exponential decay of days since last contact) — "most recent, most relevant".
func personScore(a *personAgg, now time.Time) float64 {
	rec := 0.0
	if !a.last.IsZero() {
		ageDays := now.Sub(a.last).Hours() / 24
		if ageDays < 0 {
			ageDays = 0
		}
		rec = math.Exp2(-ageDays / peopleHalfLifeDays)
	}
	return math.Log1p(float64(a.count)) + rec
}

func (a *personAgg) toJSON() personJSON {
	last := ""
	if !a.last.IsZero() {
		last = a.last.UTC().Format(time.RFC3339)
	}
	return personJSON{
		Name:          displayPersonName(a),
		Email:         a.email,
		Count:         a.count,
		LastContacted: last,
		Summary:       a.summary(),
	}
}

func displayPersonName(a *personAgg) string {
	if a.name != "" {
		return a.name
	}
	if a.email != "" {
		return a.email
	}
	return "Unknown"
}

func (a *personAgg) summary() string {
	parts := make([]string, 0, 3)
	if n := a.byCat["email"]; n > 0 {
		parts = append(parts, plural(n, "email", "emails"))
	}
	if n := a.byCat["message"]; n > 0 {
		parts = append(parts, plural(n, "message", "messages"))
	}
	if n := a.byCat["event"]; n > 0 {
		parts = append(parts, plural(n, "event", "events"))
	}
	return strings.Join(parts, " · ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func personCategory(t documentv1.DocType) string {
	switch t {
	case documentv1.DocType_EMAIL:
		return "email"
	case documentv1.DocType_CHAT_MESSAGE:
		return "message"
	case documentv1.DocType_CALENDAR_EVENT:
		return "event"
	default:
		return ""
	}
}

// hitGatewayTime is the hit's effective date (modified, else created, else zero).
func hitGatewayTime(h *queryv1.Hit) time.Time {
	if ts := h.GetModified(); ts != nil {
		return ts.AsTime()
	}
	if ts := h.GetCreated(); ts != nil {
		return ts.AsTime()
	}
	return time.Time{}
}
