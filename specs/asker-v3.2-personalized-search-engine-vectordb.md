# Claude Code Task: Build a Semantic, Personalized Search Engine for Asker

## Role & Mandate

You are a senior backend + ML engineer. Implement this feature **end to end, autonomously**: inspect the repo, choose the right tools for the existing stack, write the code, write the tests, run them, fix failures, wire up migrations, document, and open a PR — without pausing to ask me for confirmation unless you hit a genuine blocker (missing credentials, a destructive irreversible action, or an architectural fork with no reasonable default). When you must choose, pick the most sensible default, record the decision in `DECISIONS.md`, and keep going.

**First action:** detect the language, framework, package manager, existing DB, and current search implementation by reading the repo. Do not assume. Match the existing code style, dependency-injection patterns, error handling, and test conventions.

---

## The Big Picture (read this before coding)

Today Asker's search is almost certainly **lexical** — it matches keywords. That fails the moment a user phrases intent instead of keywords. We are turning Asker's search from a *keyword matcher* into an *intent-and-attention engine*.

Two things change:

1. **Understanding (Semantic/Vector Search):** the engine must understand *meaning*, not just words. "What's on my calendar next week" and "what needs my attention this week" contain almost no overlapping keywords with the underlying records, yet both must return correct, ranked results. Think of it as the difference between a librarian who only matches the exact title you say vs. one who understands what you're actually looking for and walks you to the right shelf.

2. **Relevance (Personalization):** the same query from two different users should return *differently ranked* results, because relevance is personal. This is what makes Google, Facebook, and LinkedIn search feel like they "know" you. The engine combines **what the user explicitly tells us** (the Settings page) with **what we observe them doing** (implicit behavior) to learn a per-user model of relevance.

The two must work together: vector search proposes *candidates that mean the right thing*; the personalization layer *re-ranks them into the order this specific user cares about.*

---

## Part 1 — Semantic / Vector Search Engine

### Goal
The engine must correctly answer natural-language, intent-based queries over the user's connected data (calendar events, emails, tasks, documents, messages — whatever Asker already indexes). Two canonical queries **must** work and should be used as acceptance tests:

- **"What's on my calendar next week?"** → returns the user's events scoped to next week, time-ordered, with the most important ones surfaced first.
- **"What needs my attention this week?"** → returns items requiring action this week (unanswered messages, approaching deadlines, unread important email, RSVP-pending invites, overdue tasks), ranked by urgency × importance × personal relevance.

### Architecture (implement all stages)

1. **Ingestion & chunking.** Build/extend a pipeline that pulls records from Asker's existing data sources and normalizes them into a common document schema: `{id, source_type, title, body, timestamp(s), participants, status, metadata}`. Chunk long bodies sensibly (respect sentence/paragraph boundaries; keep a token-aware splitter).

2. **Embeddings.** Use a production embedding model (e.g., OpenAI `text-embedding-3-large`, Cohere, or a self-hosted `bge`/`e5` model — pick based on what credentials/infra the repo already has; default to a hosted API if none). Abstract the embedder behind an interface so the model is swappable. Store the model name + dimension with each vector for forward compatibility.

3. **Vector store — use a dedicated vector database.** Provision and integrate a purpose-built vector DB (Qdrant, Weaviate, Pinecone, or Milvus — choose based on existing infra/cloud; default to **Qdrant** self-hosted via Docker if there's no signal). Create a collection with proper payload indexes for the metadata fields you'll filter on (user_id, source_type, timestamps, status). Configure cosine similarity. Add the service to `docker-compose`/infra-as-code so it runs locally and in CI.

4. **Hybrid retrieval.** Pure vectors miss exact matches (names, IDs, dates). Implement **hybrid search**: combine dense vector similarity with sparse/keyword retrieval (BM25 or the DB's native keyword filter), then fuse with **Reciprocal Rank Fusion**. This is the standard that makes results robust.

5. **Query understanding layer.** Natural-language queries carry *structured intent* the vector search alone can't honor — especially **time scope** and **intent type**. Before retrieval, parse the query to extract:
   - **Temporal scope** ("next week", "this week", "today") → resolve to a concrete date range relative to the user's timezone and current date, and apply it as a **hard metadata filter** (do not rely on the embedding to capture dates — it won't reliably).
   - **Intent class** (e.g., `schedule_lookup`, `needs_attention`, `find_item`) → selects which sources, filters, and ranking profile to use. Implement this as a lightweight classifier (LLM function-call or a rules+embedding hybrid).
   - **Entities** (people, projects) → optional filters/boosts.

   Concretely: "what's on my calendar next week" → intent `schedule_lookup`, source `calendar`, date filter = next Mon–Sun. "what needs my attention this week" → intent `needs_attention`, multi-source, date filter = this week, ranking profile = urgency-weighted.

6. **"Needs attention" scoring.** This intent is not a similarity problem; it's a salience problem. Compute an attention score per candidate from features such as: deadline proximity, unread/unanswered status, sender/organizer importance, whether the user is directly addressed vs. CC'd, RSVP-pending, task overdue, and thread staleness. (See the psychology section — Zeigarnik, loss aversion, and signal-detection thresholds directly inform these features.)

7. **Ranking pipeline.** Produce final results as: **candidate generation (hybrid retrieval + hard filters) → feature extraction → personalized re-ranking (Part 3) → diversification → top-K**.

### API
Expose a clean endpoint (match existing API conventions), e.g. `POST /search` accepting `{query, user_id, limit, filters?}` and returning ranked results with: the item, a relevance score, and a short **explanation of *why* it ranked where it did** (e.g., "deadline in 2 days · you usually open these"). Explanations matter for trust and for debugging the ranker.

---

## Part 2 — Settings Page (Preferences → Personalization Inputs)

Build a **Settings page** (backend models + API; build the UI too if Asker has a frontend in this repo, otherwise expose a fully-specified REST contract + OpenAPI so the frontend team can wire it). Persist preferences per user and feed every field into the ranking algorithm — **no setting may be cosmetic.** Each control below maps to a named feature weight or filter the ranker consumes.

Settings to implement (group them clearly in the UI):

- **Priority sources** — let users rank/weight which data sources matter most (Calendar, Email, Tasks, Docs, Messages). → source-weight vector.
- **Important people** — add people whose items should be boosted. → participant-importance boost.
- **Priority topics/projects/keywords** — interests to boost. → topical-affinity boost.
- **Mute list** — people/topics/sources to down-rank or hide. → negative weights / filters.
- **Working hours & timezone** — define "this week"/"next week" and what counts as urgent. → temporal grounding.
- **Attention sensitivity** — a slider from "only the critical few" to "show me everything." → the decision threshold of the needs-attention classifier (signal-detection trade-off).
- **Recency vs. importance** — a slider trading freshness against significance. → time-decay weight.
- **Novelty vs. familiarity** — how much to surface new/unseen vs. things like what they usually engage with. → exploration rate.
- **Personalization controls (transparency & autonomy):** a toggle to pause learning, a "reset what you've learned about me" action, and a per-result "show fewer/more like this" feedback affordance. These are required, not optional (autonomy + control are core to trust — and to GDPR-style data rights).

The Settings model must be versioned and the ranker must read a single resolved `UserPreferenceProfile` object so behavior is testable and explainable.

---

## Part 3 — Personalization Engine (grounded in psychological research)

Build a per-user relevance model that fuses **explicit preferences (Settings)** with **implicit behavioral signals**, mirroring how Google/Facebook/LinkedIn rank. Learn from interactions: clicks, dwell time, opens, replies, dismissals, "show fewer like this." Store these as feedback events; update the user's feature weights online (start with a transparent **learning-to-rank** approach — e.g., logistic/linear ranker or LambdaMART over engineered features — before anything fancier, because it's debuggable and the explanations fall out naturally).

### Final relevance score (combine, don't pick one)
```
relevance = w_sem · semantic_similarity
          + w_pref · explicit_preference_match      // from Settings
          + w_behav · learned_behavioral_affinity   // from implicit signals
          + w_attn · attention/urgency_score         // for needs-attention intents
          - w_fatigue · repetition_penalty           // avoid showing the same thing
```
The `w_*` weights are themselves partly user-tunable (via Settings) and partly learned.

### Psychological principles to encode (cite these in code comments where applied)

- **Salience & Signal Detection Theory** — "needs attention" is a detection problem with a tunable threshold; the attention-sensitivity slider is the criterion that trades false alarms vs. misses. Surface the genuinely urgent (Von Restorff / distinctiveness) and don't bury it.
- **Zeigarnik Effect** — unfinished/open loops (unanswered messages, incomplete tasks) command disproportionate attention; boost open/pending items for the needs-attention intent.
- **Loss Aversion / Prospect Theory** — people weigh impending losses (missed deadline, expiring RSVP) more than equivalent gains; weight approaching deadlines and time-sensitive items accordingly.
- **Recency & Mere-Exposure / Familiarity** — recent and frequently-engaged items feel more relevant; apply time-decay and frequency-of-engagement features (this is the core of Facebook/LinkedIn feed relevance).
- **Social Proof & Authority** — items from important/authoritative people (manager, frequent collaborators) rank higher; learn the user's social graph weights from interaction.
- **Serial Position Effect** — order matters: the top and bottom of a list get the most attention, so put the highest-value item first and never let critical items land mid-list.
- **Cognitive Load (Hick's & Miller's Law)** — more options = slower, worse decisions; default to a tight, high-confidence top-K and let users expand, rather than dumping everything.
- **Choice Architecture / Nudge & Defaults** — defaults dominate behavior, so ship sensible default weights that work before the user touches Settings, and treat Settings as adjustments to good defaults.
- **Exploration vs. Exploitation** — pure personalization creates filter bubbles and staleness; reserve a tunable fraction of slots for novel/diverse results (the novelty slider) using a bandit-style or MMR diversification step.
- **Peak–End & Trust/Transparency** — explanations ("why am I seeing this") and user control over the model materially increase trust and perceived relevance; this is why the per-result explanations and the reset/pause controls are mandatory.

### Cold start
New users have no behavior. Fall back to: explicit Settings → population-level priors → rapid online adaptation as signals arrive. Never return an empty or random ranking for a new user.

---

## Non-Functional Requirements

- **Privacy & data rights:** personalization data is user-owned. Implement reset/export/delete. Never leak one user's data into another's results — enforce `user_id` isolation at the vector-DB filter level *and* in tests.
- **Performance:** p95 search latency budget < 800ms; cache embeddings; batch where possible; index the metadata filter fields.
- **Observability:** log query → intent → candidates → final ranking with feature contributions, behind a debug flag, so any ranking is reproducible and explainable.
- **Configuration:** all model names, weights, thresholds, and the vector-DB connection in config/env, not hardcoded.

---

## Deliverables & Definition of Done

Implement, then **verify** (this is part of the task, not optional):

1. Ingestion + embedding + dedicated vector DB integration, with infra wired into docker-compose/CI.
2. Query-understanding layer (temporal + intent + entity parsing) with hard date filtering.
3. Hybrid retrieval + RRF + the full ranking pipeline including the attention scorer.
4. Settings: data model, API, OpenAPI spec, and UI if a frontend exists — every field consumed by the ranker.
5. Personalization engine: implicit-signal capture, learning-to-rank model, the combined scoring function, cold-start handling, and the psychological features above.
6. **Tests that must pass:**
   - Unit tests for temporal parsing, intent classification, attention scoring, and the ranker.
   - **End-to-end acceptance tests** that seed a fixture user with calendar/email/task data and assert that *"what's on my calendar next week"* and *"what needs my attention this week"* return correct, correctly-ordered results.
   - A personalization test proving the **same query ranks differently** for two users with different Settings/behavior.
   - A user-isolation test proving no cross-user leakage.
7. `DECISIONS.md` (stack/DB/model choices + rationale), updated README/setup docs, and a `.env.example`.
8. Run the full test suite and linters; fix everything until green. Then open a PR with a clear description, the acceptance-test evidence, and a summary of the psychological principles encoded.

**Work autonomously through all of the above. Use sensible defaults, document them, and only stop for a true blocker.**