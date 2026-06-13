package main

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/asker/asker/connectors/sdk"
	askerv1 "github.com/asker/asker/platform/proto/gen/go/asker/v1"
)

// Deterministic synthetic corpus generation for M5 scale/load/soak testing.
// Same (seed, params) => byte-identical corpus (ids, titles, bodies, types,
// timestamps, rare tokens, isolation markers), exactly like
// tools/fake-gmail/server/seed.go. This is the GENERATION layer: it has no
// I/O and no live-stack dependency, so it is unit-testable in-memory and the
// feed layer (see feed.go) sits behind an interface.
//
// math/rand's default Source is the frozen Go 1 generator, so sequences are
// stable across Go releases. Each tenant gets its own deterministic RNG
// derived from (seed, tenant index), so generating tenant N never depends on
// having generated tenants 0..N-1 — which is what makes --resume / checkpoint
// restartable at any tenant boundary.

// genAnchor is a FIXED point in time; document created/modified timestamps are
// spread over the two years before it. A fixed anchor (not time.Now) keeps the
// corpus fully reproducible so date-filter assertions in the load suite can use
// absolute bounds. (Mirrors seed.go's seedAnchor.)
var genAnchor = time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)

const genSpread = 2 * 365 * 24 * time.Hour

// tenantIDPrefix namespaces synthetic tenants so a leakage check (and a later
// cleanup) can recognize generator-owned groups, and so they never collide
// with the OIDC-subject tenant ids real users carry. The streaming group is
// the tenant id verbatim (vespa/README.md: id:asker:doc:g=<tenant_id>:...).
const tenantIDPrefix = "synthgen-"

// connectorIDs are the synthetic connector identifiers a generated doc can be
// attributed to, chosen to match the DocType mix (email->gmail, file->gdrive,
// chat->slack, media->upload). DocID = sdk.DocID(connectorID, sourceNativeID)
// (the platform convention, connectors/sdk/docid.go).
var connectorIDByType = map[askerv1.DocType]string{
	askerv1.DocType_EMAIL:          "gmail",
	askerv1.DocType_FILE:           "gdrive",
	askerv1.DocType_CHAT_MESSAGE:   "slack",
	askerv1.DocType_CALENDAR_EVENT: "gcal",
	askerv1.DocType_WIKI_PAGE:      "confluence",
	askerv1.DocType_TICKET:         "jira",
	askerv1.DocType_IMAGE:          "upload",
	askerv1.DocType_VIDEO:          "upload",
	askerv1.DocType_AUDIO:          "upload",
}

// contacts is the small synthetic participant pool (reused from seed.go's
// style). Kept short so participant-filter load tests get meaningful overlap.
var contacts = []string{
	"ava.alvarez@example.com",
	"ben.okafor@example.com",
	"carol.nguyen@example.com",
	"dario.rossi@example.com",
	"erin.walsh@example.com",
	"farid.khan@example.com",
	"grace.liu@example.com",
	"hugo.schmidt@example.com",
}

var titleWords = []string{
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

// docTypeName maps a DocType to its proto enum name, the exact string the
// index-writer feeds into Vespa's `type` field (writer.go: doc.GetType().String()).
func docTypeName(t askerv1.DocType) string { return t.String() }

// GenDoc is one generated document together with the derived facts the load
// suite needs to assert on it WITHOUT re-deriving them (rare token, tenant id,
// streaming group). It is the unit the feed layer consumes.
type GenDoc struct {
	Doc *askerv1.Document
	// RareToken is the qzx-style marker embedded in this doc's body, or "" if
	// this doc did not draw a rare token (see rareTokenRate). Unique per doc
	// across the whole corpus, so a query for it must hit exactly one doc in
	// exactly one tenant.
	RareToken string
	// IsolationMarker is the per-tenant marker present in EVERY doc of the
	// tenant (and only that tenant). A leakage check searches one tenant's
	// marker as another tenant and must get zero hits.
	IsolationMarker string
}

// TenantID returns the streaming-group / tenant id this doc is scoped to.
func (g GenDoc) TenantID() string { return g.Doc.GetTenantId() }

// TenantPlan is the per-tenant generation plan: which tenant, how many docs,
// and the deterministic per-tenant seed. The corpus is the ordered list of
// tenant plans; --resume skips tenants already past the checkpoint.
type TenantPlan struct {
	Index           int    // 0-based tenant index within the run
	TenantID        string // streaming group = tenant id
	DocCount        int    // docs to generate for this tenant
	IsolationMarker string // per-tenant isolation marker token
}

// Spec is the validated, fully-resolved generation specification (flags/env
// already parsed and checked). It drives both --dry-run planning and real
// generation; identical Spec => identical corpus.
type Spec struct {
	Tenants       int
	Seed          int64
	DocTypeMix    []TypeWeight // normalized weights over DocTypes
	RareTokenRate float64      // [0,1]: fraction of docs carrying a rare token
	// Distribution of docs per tenant. Most tenants are small; a few are
	// large (realistic skew). MinDocs/MaxDocs bound the per-tenant count and
	// SkewLargeFraction of tenants are drawn from the upper half of the range.
	MinDocs           int
	MaxDocs           int
	SkewLargeFraction float64
}

// TypeWeight is one entry in the doc-type mix.
type TypeWeight struct {
	Type   askerv1.DocType
	Weight float64
}

// TenantID renders the streaming group / tenant id for tenant index i. Stable
// and zero-padded so lexical and numeric order agree (load-suite friendly).
func TenantID(i int) string { return fmt.Sprintf("%s%08d", tenantIDPrefix, i) }

// IsolationMarker is the per-tenant marker embedded in every doc of tenant i.
// Distinct from the rare-token namespace (qzx...) so a leakage assertion can
// search one tenant's marker as another tenant and expect exactly zero hits.
func IsolationMarker(seed int64, i int) string {
	return fmt.Sprintf("isomark%x%08d", uint64(seed)&0xffff, i)
}

// RareToken returns the unique targeting token for the global rare-token index
// g (0-based across the whole corpus). Same formula as seed.go's RareToken so
// the style is shared, but the value is the GLOBAL ordinal among docs that drew
// a rare token, guaranteeing corpus-wide uniqueness across tenants.
func RareToken(g int) string { return fmt.Sprintf("qzx%08d", g) }

// PlanTenants builds the ordered per-tenant plan for spec. Deterministic: the
// per-tenant doc counts and isolation markers depend only on (seed, params).
func PlanTenants(spec Spec) []TenantPlan {
	plans := make([]TenantPlan, spec.Tenants)
	for i := 0; i < spec.Tenants; i++ {
		plans[i] = TenantPlan{
			Index:           i,
			TenantID:        TenantID(i),
			DocCount:        docCountFor(spec, i),
			IsolationMarker: IsolationMarker(spec.Seed, i),
		}
	}
	return plans
}

// docCountFor returns the deterministic doc count for tenant i under the
// configured skew. A per-tenant RNG (derived from seed+index, NOT the global
// sequence) decides small-vs-large and the exact count, so the count is stable
// regardless of which tenants are generated and in what order (resume-safe).
func docCountFor(spec Spec, i int) int {
	lo, hi := spec.MinDocs, spec.MaxDocs
	if hi <= lo {
		return lo
	}
	rng := tenantRNG(spec.Seed, i, "count")
	span := hi - lo + 1
	mid := lo + span/2
	if rng.Float64() < spec.SkewLargeFraction {
		// Large tenant: draw from the upper half [mid, hi].
		if hi <= mid {
			return hi
		}
		return mid + rng.Intn(hi-mid+1)
	}
	// Small tenant: draw from the lower half [lo, mid].
	return lo + rng.Intn(mid-lo+1)
}

// tenantRNG returns a deterministic RNG seeded from (seed, tenant index, salt).
// Different salts give independent streams for independent decisions (count vs
// content) so adding a decision never shifts an unrelated stream.
func tenantRNG(seed int64, tenantIdx int, salt string) *rand.Rand {
	h := fnv.New64a()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(seed))
	_, _ = h.Write(buf[:])
	binary.LittleEndian.PutUint64(buf[:], uint64(tenantIdx))
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte(salt))
	return rand.New(rand.NewSource(int64(h.Sum64()))) //nolint:gosec // deterministic test data, not crypto
}

// GenerateTenant generates all docs for one tenant plan. rareBase is the global
// rare-token ordinal the first rare-token doc of this tenant should use; the
// returned count is how many rare tokens this tenant consumed (so the caller
// advances rareBase across tenants and keeps tokens globally unique). The doc
// stream depends only on (spec.Seed, plan.Index) plus rareBase, so a resumed
// run that recomputes rareBase from the checkpoint reproduces the same docs.
//
// The rare-token decision rides on its OWN per-tenant RNG stream ("rare"),
// independent of the content stream, so RareTokensInTenant can count rare
// tokens cheaply (one float draw per doc) without replaying body generation —
// which is what keeps a 100K-tenant --dry-run fast.
func GenerateTenant(spec Spec, plan TenantPlan, rareBase int) (docs []GenDoc, rareUsed int) {
	contentRNG := tenantRNG(spec.Seed, plan.Index, "content")
	rareRNG := tenantRNG(spec.Seed, plan.Index, "rare")
	docs = make([]GenDoc, 0, plan.DocCount)
	g := rareBase
	for d := 0; d < plan.DocCount; d++ {
		dt := pickType(spec.DocTypeMix, contentRNG)
		var rare string
		if rareRNG.Float64() < spec.RareTokenRate {
			rare = RareToken(g)
			g++
		}
		docs = append(docs, buildDoc(spec, plan, contentRNG, d, dt, rare))
	}
	return docs, g - rareBase
}

// RareTokensInTenant counts how many docs in tenant plan draw a rare token,
// WITHOUT building the documents. Used to advance the global rare-token base
// across tenants when resuming and to size the dry-run rare-token index. Because
// the rare-token decision is on its own RNG stream, this is exact AND cheap
// (no content/body replay) — only plan.DocCount float draws.
func RareTokensInTenant(spec Spec, plan TenantPlan) int {
	rareRNG := tenantRNG(spec.Seed, plan.Index, "rare")
	n := 0
	for d := 0; d < plan.DocCount; d++ {
		if rareRNG.Float64() < spec.RareTokenRate {
			n++
		}
	}
	return n
}

// pickType chooses a DocType from the normalized mix.
func pickType(mix []TypeWeight, rng *rand.Rand) askerv1.DocType {
	if len(mix) == 0 {
		return askerv1.DocType_FILE
	}
	r := rng.Float64()
	var acc float64
	for _, tw := range mix {
		acc += tw.Weight
		if r < acc {
			return tw.Type
		}
	}
	return mix[len(mix)-1].Type
}

// buildDoc constructs one canonical Document plus its derived facts. All draws
// come from the per-tenant content RNG; the rare-token decision was made by the
// caller on a separate stream and passed in as rare.
func buildDoc(_ Spec, plan TenantPlan, rng *rand.Rand, docIdx int, dt askerv1.DocType, rare string) GenDoc {
	connectorID := connectorIDByType[dt]
	if connectorID == "" {
		connectorID = "upload"
	}
	// native id embeds tenant+index => unique; DocID is the platform hash.
	nativeID := fmt.Sprintf("%s-%s-%06d", connectorID, plan.TenantID, docIdx)
	docID := sdk.DocID(connectorID, nativeID)

	titleLen := 3 + rng.Intn(4)
	title := pickWords(rng, titleWords, titleLen)

	body := buildBody(rng, plan.IsolationMarker, rare)

	// Timestamps spread over the two years before the fixed anchor.
	createdOffset := time.Duration(rng.Int63n(int64(genSpread)))
	created := genAnchor.Add(-createdOffset).Truncate(time.Second)
	// Modified at or after created, within the remaining window.
	var modified time.Time
	if rng.Intn(3) == 0 && createdOffset > 0 {
		modified = created.Add(time.Duration(rng.Int63n(int64(createdOffset)))).Truncate(time.Second)
	} else {
		modified = created
	}

	contact := contacts[rng.Intn(len(contacts))]
	inbound := rng.Intn(2) == 0

	doc := &askerv1.Document{
		TenantId:       plan.TenantID,
		DocId:          docID,
		ConnectorId:    connectorID,
		SourceNativeId: nativeID,
		Type:           dt,
		Title:          title,
		BodyText:       body,
		Metadata: map[string]string{
			"synthgen":     "1",
			"isolation":    plan.IsolationMarker,
			"doc_type":     docTypeName(dt),
			"tenant_index": fmt.Sprintf("%d", plan.Index),
		},
		Ts: &askerv1.Timestamps{
			Created:  tsProto(created),
			Modified: tsProto(modified),
		},
		VersionEtag: fmt.Sprintf("v-%s", docID[:16]),
	}
	doc.Participants = participantsFor(dt, contact, inbound)

	return GenDoc{Doc: doc, RareToken: rare, IsolationMarker: plan.IsolationMarker}
}

// tsProto renders a time as a protobuf Timestamp.
func tsProto(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

func participantsFor(dt askerv1.DocType, contact string, inbound bool) []*askerv1.Participant {
	switch dt {
	case askerv1.DocType_IMAGE, askerv1.DocType_VIDEO, askerv1.DocType_AUDIO, askerv1.DocType_FILE:
		// Media/file uploads have no participants in the M1 model.
		return nil
	default:
		fromRole, toRole := "from", "to"
		owner := "owner@synthgen.example"
		if inbound {
			return []*askerv1.Participant{
				{Email: contact, Role: fromRole},
				{Email: owner, Role: toRole},
			}
		}
		return []*askerv1.Participant{
			{Email: owner, Role: fromRole},
			{Email: contact, Role: toRole},
		}
	}
}

func pickWords(rng *rand.Rand, pool []string, n int) string {
	words := make([]string, n)
	for i := range words {
		words[i] = pool[rng.Intn(len(pool))]
	}
	return strings.Join(words, " ")
}

// buildBody builds a varied-length body (3..10 sentences of 8..16 words),
// mirroring seed.go's buildBody style, then appends the per-tenant isolation
// marker (in EVERY doc) and, if drawn, the unique rare token.
func buildBody(rng *rand.Rand, isolationMarker, rare string) string {
	var b strings.Builder
	sentences := 3 + rng.Intn(8) // vary length: 3..10 sentences
	for s := 0; s < sentences; s++ {
		sentence := pickWords(rng, bodyWords, 8+rng.Intn(9))
		b.WriteString(strings.ToUpper(sentence[:1]) + sentence[1:])
		b.WriteString(". ")
	}
	// Per-tenant isolation marker in EVERY doc: a leakage check searches it as
	// another tenant and must get zero hits.
	fmt.Fprintf(&b, "Tenant isolation marker %s. ", isolationMarker)
	if rare != "" {
		fmt.Fprintf(&b, "Tracking reference %s.", rare)
	}
	return b.String()
}

// NormalizeMix sorts and normalizes a doc-type mix so weights sum to 1. A
// sorted, normalized mix is the canonical form pickType walks; sorting by enum
// value keeps the cumulative thresholds deterministic regardless of input
// order.
func NormalizeMix(mix []TypeWeight) []TypeWeight {
	out := make([]TypeWeight, len(mix))
	copy(out, mix)
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	var sum float64
	for _, tw := range out {
		sum += tw.Weight
	}
	if sum <= 0 {
		return out
	}
	for i := range out {
		out[i].Weight /= sum
	}
	return out
}
