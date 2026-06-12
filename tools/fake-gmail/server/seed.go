package server

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// Deterministic synthetic mail generation for dev/CI (ADR-008). Given the
// same (seed, count) the generator produces byte-identical ids, subjects,
// bodies, participants and dates, so e2e suites can assert on exact content.
// math/rand's Source is the frozen Go 1 generator, so sequences are stable
// across Go releases.

// seedAnchor is a FIXED point in time; internalDates are spread over the two
// years before it. A fixed anchor (instead of time.Now) keeps the corpus
// fully reproducible so date-filter tests can use absolute bounds.
var seedAnchor = time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)

const seedSpread = 2 * 365 * 24 * time.Hour

// seedContacts is the small synthetic contact set messages are exchanged
// with. Kept short so participant-filter tests get meaningful overlap.
var seedContacts = []string{
	"ava.alvarez@example.com",
	"ben.okafor@example.com",
	"carol.nguyen@example.com",
	"dario.rossi@example.com",
	"erin.walsh@example.com",
	"farid.khan@example.com",
	"grace.liu@example.com",
	"hugo.schmidt@example.com",
}

var subjectWords = []string{
	"quarterly", "roadmap", "review", "budget", "launch", "draft", "notes",
	"sync", "planning", "update", "metrics", "retro", "offsite", "invoice",
	"contract", "design", "proposal", "schedule", "migration", "incident",
	"release", "onboarding", "hiring", "survey", "renewal", "summary",
	"forecast", "kickoff", "deadline", "feedback", "agenda", "milestone",
}

var bodyWords = []string{
	"the", "we", "should", "discuss", "before", "next", "week", "team",
	"project", "needs", "a", "decision", "on", "this", "by", "friday",
	"please", "review", "attached", "document", "and", "share", "your",
	"thoughts", "meeting", "moved", "to", "thursday", "afternoon", "because",
	"of", "conflict", "with", "customer", "call", "numbers", "look", "good",
	"but", "margin", "is", "tighter", "than", "expected", "let", "me",
	"know", "if", "you", "can", "join", "early", "draft", "ready", "for",
	"comments", "deadline", "remains", "unchanged", "vendor", "confirmed",
	"delivery", "date", "infrastructure", "cost", "went", "down", "after",
	"migration", "thanks", "everyone", "great", "work", "quarter", "will",
	"follow", "up", "separately", "details", "are", "in", "shared", "folder",
}

// RareToken returns the unique targeting token embedded in the body of the
// seeded message at index i. Exported (within the package surface used by
// tests/e2e via the admin API contract) so search tests can derive the token
// for a specific message: same formula, no server round-trip needed.
func RareToken(i int) string {
	return fmt.Sprintf("qzx%05d", i)
}

// generateSeedMessages builds count message inputs for owner. Message index
// i carries the distinct rare token RareToken(i) in its body so search tests
// can target exactly one message. Ids embed the index, guaranteeing batch
// uniqueness while staying deterministic.
func generateSeedMessages(owner string, seed int64, count int) []messageInput {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test data, not crypto
	inputs := make([]messageInput, 0, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%010x%06x", rng.Uint64()&0xffffffffff, uint(i)&0xffffff)
		contact := seedContacts[rng.Intn(len(seedContacts))]
		inbound := rng.Intn(2) == 0
		from, to := contact, owner
		labels := []string{"INBOX"}
		if !inbound {
			from, to = owner, contact
			labels = []string{"SENT"}
		}

		subject := pickWords(rng, subjectWords, 3+rng.Intn(4))
		body := buildBody(rng, i)
		date := seedAnchor.Add(-time.Duration(rng.Int63n(int64(seedSpread))))

		inputs = append(inputs, messageInput{
			id:           id,
			threadID:     id, // one message per thread: threading is out of scope for the fake
			internalDate: date.Truncate(time.Second),
			from:         from,
			to:           to,
			subject:      subject,
			body:         body,
			labelIDs:     labels,
		})
	}
	return inputs
}

func pickWords(rng *rand.Rand, pool []string, n int) string {
	words := make([]string, n)
	for i := range words {
		words[i] = pool[rng.Intn(len(pool))]
	}
	return strings.Join(words, " ")
}

func buildBody(rng *rand.Rand, index int) string {
	var b strings.Builder
	sentences := 3 + rng.Intn(5)
	for s := 0; s < sentences; s++ {
		sentence := pickWords(rng, bodyWords, 8+rng.Intn(7))
		b.WriteString(strings.ToUpper(sentence[:1]) + sentence[1:])
		b.WriteString(". ")
	}
	// The rare token: distinct per message index, absent from the word
	// pools, so a search for it must hit exactly this message.
	fmt.Fprintf(&b, "Tracking reference %s.", RareToken(index))
	return b.String()
}
