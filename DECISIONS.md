# DECISIONS — v3.2 Personalized, Semantic Search

This document records the stack/DB/model choices for the v3.2 personalized-search feature
(`specs/asker-v3.2-personalized-search-engine-vectordb.md`) and the rationale behind each. The
spec was written generically ("default to Qdrant", "build a Settings page", …); these decisions
adapt it to the **actual** Asker codebase, which is a mature, shipped Go monorepo (M0–M6
complete) with a strong, structural per-tenant isolation model. Where the spec's generic default
conflicts with the repo's reality, the repo wins — the spec itself mandates "inspect the repo,
choose the right tools for the existing stack."

## D1 — Vector store: **Vespa (existing)**, not Qdrant

The spec says "default to **Qdrant** self-hosted via Docker **if there's no signal**." There is
overwhelming signal: Asker already runs **Vespa in streaming mode** as its search engine —
one document group per `tenant_id`, near-zero per-tenant cost at rest, hybrid dense
(`closeness(field, embedding)` over a bge-m3 tensor) + keyword (`nativeRank`) ranking, a CLIP
text→image arm, and TEI embeddings already wired through the Kafka enrich pipeline (ADR-005,
ADR-006, ADR-013).

Introducing Qdrant would **fragment the isolation model** that the entire system is built on:
tenant isolation is *structural* (`streaming.groupname` is derived only from the verified OIDC
token, never request input; `platform/tenancy` is the compile-time chokepoint). A second vector
DB would mean a second isolation enforcement point, a second ingestion sink, and a second
operational surface — for zero functional gain, since Vespa already does cosine-similarity dense
retrieval with metadata filtering.

**Decision:** build personalization **on top of** Vespa. Cosine similarity is already configured
(`distance-metric: angular`). Metadata filter fields we rank/filter on (`type`, `created_at`,
`participants`) are already attributes. The dedicated-vector-DB requirement is satisfied by the
existing dedicated vector DB.

## D2 — Where personalization runs: **post-retrieval re-rank in the query service**

Vespa stays **user-agnostic** (its rank profiles take only global inputs). Candidate generation
(hybrid retrieval + hard filters) happens in Vespa; **per-user re-ranking happens in the query
service after retrieval**, mirroring the existing `rerankByRecency` pattern (`services/query/rerank.go`).
Rationale: streaming mode keeps no corpus statistics and builds no per-user index; a per-tenant
(not per-user) schema field cannot express per-user weights anyway; and post-retrieval re-rank is
debuggable, explainable, and where the spec's combined scoring formula naturally lives. This also
honors ADR-006 for the non-personalized path (unchanged) while layering v3 on top.

## D3 — Identity model: **tenant_id IS the user**

Asker is a *personal* search engine: ~10M tenants, one human per tenant (the dev realm has
alice/bob/carol as separate tenants; `/v1/me` asserts `tenant_id == JWT sub`). "Per-user" and
"per-tenant" therefore coincide. Personalization data (preferences, behavioral feedback, learned
weights) is keyed by `tenant_id` — which is also the sacred isolation boundary, so user-isolation
is inherited from the existing structural guarantees rather than bolted on. The JWT `sub`/`email`
claims are available for display and for seeding the profile's `self_emails`.

## D4 — Persistence: **Postgres (control-plane) for durable data, Redis for the read path**

The spec requires preferences to be **versioned, user-owned, exportable, and deletable** (data
rights / GDPR). That is durable relational data, and the repo already has exactly the right home:
the **control-plane** service owns Postgres via `golang-migrate` + `pgStore`, with tenancy-filtered
queries and a GDPR `PurgeTenant` cascade. New tables (`user_preferences`, `learned_weights`,
`feedback_events`) are added there (migration `0002_personalization`), each `REFERENCES tenants
(tenant_id) ON DELETE CASCADE` so the existing erasure cascade erases them automatically; the
delete-verification residue check is extended to cover them.

The **query hot path** must not do a synchronous Postgres round-trip per search (p95 < 800ms
budget). So the resolved profile + learned weights are **write-through cached in Redis**
(`asker:pref:<tenant>`, `asker:weights:<tenant>`) by the gateway whenever they change, and the
query service reads them from Redis (it already has a Redis client). On a Redis miss the query
service falls back to **cold-start defaults** (D7) — never an error, never empty results. The
GDPR Redis purge is extended to delete these keys.

> Rejected alternative: storing preferences in Redis only (like recent-searches). Simpler and
> needs no proto change, but Redis is cache-grade durability here; losing user-owned preference
> data on a flush is a data-rights regression. Postgres-of-record + Redis-cache is the right split.

## D5 — Embeddings: **existing TEI bge-m3, kept behind the `embedder` interface**

The repo already abstracts the embedder (`embedder`/`clipEmbedder` interfaces, model + dim as
deploy config per ADR-005). We reuse it unchanged — no reason to add OpenAI/Cohere when a
production embedding model is already served and isolation-clean. The model name/dim already
travel with each vector via the deploy contract.

## D6 — Query understanding: **rules + embeddings, no new LLM dependency**

Temporal scope and intent are parsed by a lightweight, deterministic, fully-unit-tested layer in
the query service (`temporal.go`, `intent.go`), not an LLM. The stack has **no LLM** (TEI serves
embeddings only); adding one would be a heavy new dependency for a classification task that rules +
the existing embedder handle well, and determinism makes the acceptance tests reliable. Temporal
phrases ("next week", "this week", "today", …) resolve to a concrete `[from,to]` range in the
user's timezone (from the profile; default UTC) and apply as a **hard filter** — never left to the
embedding (see D11 for *which* date field). Intent (`schedule_lookup` / `needs_attention` /
`find_item` / `freeform`) selects sources and the ranking profile. The interface is left open for
an LLM classifier swap.

## D7 — Personalization model: **transparent online logistic learning-to-rank**

Per the spec ("start with a transparent learning-to-rank approach … because it's debuggable and
the explanations fall out naturally"), behavioral affinity is a **pointwise logistic ranker** over
engineered binary features (`type:EMAIL`, `src:gmail`, `from:alice@x`, `topic:budget`), updated
**online** by SGD on each feedback event. Final relevance is the spec's combined score:

```
relevance = w_sem·semantic + w_pref·preference + w_behav·behavioral + w_attn·attention − w_fatigue·repetition
```

The `w_*` are partly user-tunable (Settings sliders map to them) and partly learned. All scoring,
the LTR update, the profile model, and defaults live in a new pure-logic shared package
**`platform/personalization`** (no I/O; ≥75% coverage per the platform floor), imported by the
query service (apply) and the control-plane (update-on-feedback). **Cold start:** a new user with
no behavior gets `DefaultProfile()` (sensible non-cosmetic defaults — choice architecture) →
explicit Settings → population priors → online adaptation. Never empty, never random.

## D8 — Hybrid retrieval + RRF

The existing hybrid is a single-pass Vespa linear blend (ADR-006 deferred RRF to M5). The v3 spec
requires **Reciprocal Rank Fusion**. We implement `rrfFuse` (pure, unit-tested) and a dual-arm
retrieval (keyword-ranked + vector-ranked lists) fused by RRF, **gated by config**
(`QUERY_HYBRID_RRF`) and used by the **personalized path only**. The non-personalized path
(existing tests, `newServer` with no profile loader) is byte-for-byte unchanged, so ADR-006 holds
where it still applies. Personal corpora are tiny (exact streaming scan), so the second Vespa call
is within budget. This supersedes ADR-006's deferral for the personalized path.

## D9 — Explanations & transparency surfaced via proto, not the metadata map

`query.proto` `Hit` gains `explanation` (string) and `features` (`map<string,double>`, debug)
fields; `SearchRequest` gains `debug`. Proto regeneration was validated to be clean and
reproducible in this environment (`make tools && make proto` → zero drift), so first-class fields
are preferred over overloading the `metadata` map. The gateway surfaces `explanation` in its
pinned REST hit shape; the web renders a "why this ranked" affordance and per-result
"show more/fewer like this" feedback. Pause-learning, reset-what-you've-learned, and export are
mandatory controls (Settings + `/v1/preferences/*`).

## D10 — Acceptance tests: real pipeline via the in-process harness

The spec's "end-to-end acceptance tests" are implemented as **Go tests through the real
query-service gRPC pipeline** (the existing `bufconn` + tenancy-interceptor + `vespaStub`
harness), seeding the Vespa stub with fixture calendar/email/task documents and injecting a
profile via a fake loader — exercising genuine query-understanding → retrieval → attention →
personalized re-rank → response. Gateway preference/feedback APIs are tested through the real
handler chain with fakes. This is the idiomatic "e2e" in this codebase (the bash live-stack e2e
under `tools/e2e` needs a 2.3 GB model + a calendar-seeding extension to fake-gmail; that
extension is recorded as a follow-up so the existing suites stay green). The two canonical queries,
personalization-differs, and user-isolation are all asserted at this level.

## D11 — Temporal scope filters the EVENT'S OCCURRENCE TIME, via a new `event_start` attribute

A design review caught a decisive bug in the naive plan: `created_at` (the only date attribute in
the Vespa schema) is the document's *creation* time. For a calendar event that is when the event
was *authored*, not when it *occurs* (`connectors/gcal/event.go` maps `ts.Created` from the event's
`created`/`updated`; the occurrence time lives only in `metadata["start"]`, which is summary-only
and not filterable). Filtering "next week" on `created_at` would return events *created* next week —
wrong for the #1 canonical query.

**Decision:** add `event_start` (and `event_end`) as Vespa `long` attributes, populated by the
index-writer from `metadata["start"]`/`["end"]`. `schedule_lookup` ("what's on my calendar next
week") filters on **`event_start`** (occurrence time, scalable/pushed-down to Vespa) and orders
results by it (time-ordered, per spec). "Received this week" semantics for email/messages still use
`created_at`. The query service additionally hard-filters calendar candidates by occurrence time
post-retrieval (parsed from `metadata["start"]`), which makes the logic self-contained and testable
and guards against any indexing gap. Pre-existing documents acquire `event_start` on their next
reindex; until then the post-retrieval occurrence filter still applies.

## D12 — Attention signals: grounded in real data, forward-compatible for the rest

The same review flagged that several attention features the spec lists are not in the indexed data.
The attention scorer is built to compute each from a documented set of (optional) `Hit.metadata`
keys, degrading gracefully when a key is absent. What is **grounded today**:

- **RSVP-pending** (Zeigarnik, loss aversion): `metadata["response_status:<self-email>"] ==
  "needsAction"` — emitted by the calendar connector (`connectors/gcal/event.go`); `self_emails`
  comes from the profile (seeded from the verified JWT email).
- **Approaching events** (loss aversion): `event_start` proximity within the window (D11).
- **Unread / important email** (Zeigarnik, Authority): the Gmail connector is extended to emit
  `metadata["unread"]`/`["important"]` from the message `labelIds` (UNREAD/IMPORTANT) — a real,
  small connector enhancement.
- **Directly-addressed vs CC'd** (salience): `self_emails` ∈ `metadata["to"]` outranks CC-only.
- **Sender/organizer importance** (Social Proof/Authority): `metadata["from"]` ∈
  `important_people`, plus learned per-sender affinity.
- **Recency / thread staleness** (Recency, Zeigarnik): from `created_at`/`modified_at`.

**Consumed when present, but not yet emitted by first-party connectors (documented follow-ups):**
generic task `due`/`deadline` + `status` for overdue scoring (no dedicated task connector exists;
Jira TICKETs carry `status`). The scorer already handles these keys, so enriching a connector to
emit them is purely additive. The "attention sensitivity" slider is the Signal-Detection criterion
applied to the resulting score (only-the-critical-few ⇄ show-everything).

## Psychological principles encoded (cited in code where applied)

Signal Detection Theory (attention-sensitivity = decision criterion), Zeigarnik (open loops:
unread/unanswered/RSVP-pending/overdue boosted), Loss Aversion / Prospect Theory (approaching
deadlines & expiring invites weighted), Recency / Mere-Exposure (time-decay + engagement
frequency), Social Proof / Authority (important-people & learned-sender boosts), Serial Position
(highest-value item first, critical items never mid-list), Cognitive Load / Hick's & Miller's
(tight high-confidence top-K), Choice Architecture (good defaults before any setting is touched),
Exploration vs. Exploitation (novelty slider → MMR diversification), Peak-End / Trust (per-result
explanations + reset/pause/export controls).
