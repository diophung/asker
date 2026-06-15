You are Claude Code acting as a senior backend engineer, search relevance engineer, product-minded architect, and full-stack implementer.

Your task is to autonomously improve the Asker backend search engine end to end so it supports Vector Search / Semantic Search, personalized relevance ranking, and a user-facing Search Settings page.

## Product Goal

Upgrade Asker from keyword-style search into an intelligent personal search and question-answering engine.

The engine must be able to answer questions such as:

- “What’s on my calendar next week?”
- “What needs my attention this week?”
- “What emails should I respond to?”
- “What meetings are important today?”
- “What documents are relevant to this project?”
- “What did I discuss with Alex recently?”

The system should behave more like modern search systems used by Google Search, Facebook Search, and LinkedIn Search: results should not merely match words. They should be ranked by meaning, user context, recency, importance, relationship strength, stated preferences, and learned behavior.

## Scope

Implement this end to end. Do not stop at architecture notes. Write production-quality code, database migrations, backend services, API endpoints, UI pages, tests, and documentation.

Assume Asker may have access to user data sources such as calendar, email, chat, documents, tasks, cloud storage, and uploaded files. Build the system so new source connectors can be added cleanly.

## Core Capabilities

### 1. Semantic Search and Vector Search

Implement a semantic search pipeline that supports:

- Text chunking and normalization.
- Embedding generation.
- Vector storage.
- Hybrid retrieval using both keyword search and vector similarity.
- Metadata filtering by source type, date range, owner, project, participants, priority, and recency.
- Ranking by semantic similarity, freshness, source credibility, user preferences, and learned engagement.
- Question-answering over retrieved context using grounded citations or source references.
- Incremental indexing when source data changes.
- Re-indexing jobs for stale or failed embeddings.

The system should support questions about time-sensitive personal data, especially calendar and weekly planning questions.

For example, when the user asks “what’s on my calendar next week,” the engine should understand the date range, search calendar events in that range, summarize them, and highlight important conflicts, travel, prep work, and follow-ups.

When the user asks “what needs my attention this week,” the engine should combine calendar, email, tasks, documents, and recent interactions, then rank items by urgency, importance, deadlines, relationship strength, and user preference.

### 2. Personalization Engine

Build a personalization engine that uses both explicit and implicit signals.

Explicit signals come from the Search Settings page. Users must be able to specify preferences such as:

- Preferred result types: calendar, email, documents, tasks, chat, people, projects.
- Ranking style: recency-first, importance-first, deadline-first, relationship-first, project-first, balanced.
- Preferred summary style: brief, detailed, action-oriented, executive summary.
- Time horizon defaults: today, this week, next week, last 30 days.
- People or projects to prioritize.
- Sources to include or exclude.
- Topics to boost or suppress.
- Sensitivity preferences for private or personal content.
- Notification or attention thresholds.
- Work-hour and timezone preferences.
- Whether the engine should infer priorities automatically.

Implicit signals should be learned from behavior, including:

- Which results the user opens.
- Which results the user ignores.
- Which results the user saves, dismisses, marks important, or marks irrelevant.
- Which people, projects, and sources the user interacts with most.
- Search query reformulation patterns.
- Follow-up questions.
- Calendar attendance and meeting frequency.
- Email response behavior.
- Recency and frequency of interactions.

The personalization system must produce explainable ranking signals, not a black box. Each result should be able to show why it was ranked highly, for example:

“Ranked high because it is due this week, involves your manager, matches your AI platform project, and similar items were previously marked important.”

### 3. Psychological Research Foundations

Ground the search behavior and settings model in established psychological and cognitive science fundamentals. Do not make vague claims. Translate the research into product mechanisms.

Use these foundations when designing ranking, settings, and personalization:

- Attention is limited, so the engine should reduce cognitive load and avoid overwhelming the user.
- Recency and salience influence what users notice first.
- Goal-directed behavior means results should adapt to stated user intent and active goals.
- Relevance is contextual, not only textual.
- People give stronger attention to items involving close collaborators, deadlines, unresolved obligations, and high-consequence outcomes.
- Choice overload should be reduced by grouping, ranking, and summarizing results.
- Recognition is easier than recall, so settings should use clear options, examples, and presets instead of asking users to manually define everything.
- Users need control and transparency to trust personalization.
- Feedback loops should be lightweight, such as thumbs up/down, “more like this,” “less like this,” “always prioritize,” and “hide source.”

Convert these principles into concrete implementation decisions.

For example:

- Use result grouping to reduce choice overload.
- Use presets like “Executive Mode,” “Focus Mode,” “Recency Mode,” and “Deadline Mode.”
- Show “why this result” explanations.
- Use attention scoring for weekly planning.
- Do not let implicit learning override explicit user preferences.
- Allow users to reset or export personalization data.
- Keep privacy controls visible and easy to understand.

### 4. Search Settings Page

Implement a complete Search Settings page.

The page must include:

- Ranking preference controls.
- Source selection controls.
- Time horizon defaults.
- People/project priority controls.
- Summary style controls.
- Privacy and data usage controls.
- Personalization reset.
- Search behavior presets.
- Explanation of how personalization works.
- Ability to save, update, and retrieve preferences.
- Validation and sensible defaults.
- Clean, modern UI.

The Settings page should feel like a serious productivity product, not a developer configuration dump. The user should feel in control.

### 5. Backend APIs

Create or update APIs for:

- Search query execution.
- Semantic answer generation.
- Search result feedback.
- User search preferences.
- Ranking explanation.
- Indexing status.
- Re-indexing trigger.
- Source filtering.
- Personalization profile retrieval.
- Personalization reset.

Use clear request and response contracts.

Example API concepts:

- `POST /api/search`
- `POST /api/search/answer`
- `POST /api/search/feedback`
- `GET /api/search/preferences`
- `PUT /api/search/preferences`
- `GET /api/search/explanation/:resultId`
- `GET /api/search/index/status`
- `POST /api/search/index/rebuild`
- `POST /api/search/personalization/reset`

Adapt route names to the existing Asker codebase conventions.

### 6. Data Model

Design and implement the required database schema.

The data model should include:

- Searchable documents or records.
- Chunks.
- Embeddings.
- Source metadata.
- User preferences.
- User feedback events.
- Ranking signals.
- Search sessions.
- Query logs.
- Personalization profiles.
- Indexing jobs.
- Search explanations.

Use migrations where appropriate.

Keep privacy in mind. Do not store raw sensitive content unnecessarily if metadata or derived signals are sufficient.

### 7. Ranking Algorithm

Implement a ranking pipeline that combines:

- Semantic similarity score.
- Keyword match score.
- Recency score.
- Deadline score.
- Importance score.
- Relationship strength score.
- User preference boost.
- Source priority boost.
- Feedback-derived boost.
- Calendar/event urgency score.
- Email response-needed score.
- Task due-date score.

The ranking pipeline must be modular and testable.

Use a weighted scoring model with configurable weights. Explicit user preferences should have higher authority than inferred behavior.

Return ranking explanations with each result.

### 8. Question Answering Behavior

For answer-style queries, implement a grounded response flow:

1. Parse query intent.
2. Resolve time range, such as “next week” or “this week.”
3. Retrieve relevant records using hybrid search.
4. Rank and group results.
5. Generate an answer using only retrieved context.
6. Include source references.
7. Include action items when appropriate.
8. Show uncertainty when data is incomplete.

For “what needs my attention this week,” the answer should produce grouped sections such as:

- Urgent deadlines.
- Meetings needing preparation.
- Emails likely needing reply.
- Important follow-ups.
- Project risks.
- Low-confidence items that may need review.

### 9. Privacy, Safety, and Trust

Implement privacy-aware behavior.

Requirements:

- Users can disable personalization.
- Users can exclude sources.
- Users can delete learned preferences.
- Users can inspect why results were ranked.
- Explicit preferences override inferred preferences.
- Sensitive data should not be used for personalization unless allowed.
- Avoid storing unnecessary raw private content.
- Log enough for debugging without leaking sensitive data.
- Add access checks around all personal data retrieval.
- Do not mix data between users or tenants.

### 10. UI Requirements

Implement the UI needed to support this feature.

Include:

- Search box supporting natural language questions.
- Results page with grouped results.
- Answer panel for semantic answers.
- Result cards with source, date, relevance explanation, and actions.
- Feedback controls.
- Search Settings page.
- Indexing status display if appropriate.
- Empty states and error states.
- Loading states.
- Accessibility basics.

The user experience should reduce cognitive load. Avoid dumping too many results. Prefer grouped, explained, ranked results.

### 11. Testing

Add comprehensive tests.

Include:

- Unit tests for query parsing.
- Unit tests for time range resolution.
- Unit tests for ranking score calculation.
- Unit tests for preference overrides.
- Unit tests for feedback ingestion.
- Unit tests for personalization profile updates.
- Integration tests for search API.
- Integration tests for preferences API.
- Integration tests for indexing pipeline.
- UI tests for Search Settings page.
- Regression tests for multi-user isolation.

Use mock embeddings where needed so tests are deterministic.

### 12. Observability

Add observability for:

- Search latency.
- Embedding generation failures.
- Index freshness.
- Retrieval hit rate.
- Answer generation failure rate.
- Feedback rate.
- Click-through rate.
- Personalization impact.
- Query types.
- Top failed queries.
- Empty result rate.

Add logs, metrics, and traces following the existing Asker conventions.

### 13. Implementation Strategy

Work autonomously. Inspect the existing codebase first and adapt to its architecture, framework, database, conventions, and style.

Do not ask for confirmation unless blocked by missing credentials or impossible external dependencies.

Proceed in this order:

1. Understand the current Asker backend, frontend, database, auth model, and search implementation.
2. Identify the existing extension points for search, settings, and data connectors.
3. Implement the database schema and migrations.
4. Implement embedding and indexing services.
5. Implement hybrid retrieval.
6. Implement personalization preferences and feedback storage.
7. Implement ranking and explanation services.
8. Implement natural-language query parsing and time range resolution.
9. Implement search and answer APIs.
10. Implement the Search Settings page.
11. Implement the search results and answer UI updates.
12. Add tests.
13. Add observability.
14. Update documentation.
15. Run lint, typecheck, tests, and build.
16. Fix failures until the implementation is clean.

### 14. Acceptance Criteria

The work is complete only when:

- A user can ask “what’s on my calendar next week” and receive a grounded answer from calendar data.
- A user can ask “what needs my attention this week” and receive ranked, grouped, explainable results.
- Search uses semantic/vector retrieval, not only keyword matching.
- Search results are personalized using explicit settings and learned feedback.
- The Search Settings page works end to end.
- Preferences persist and affect ranking.
- Feedback affects future ranking.
- Results include “why this result” explanations.
- Users can reset personalization.
- Privacy controls are implemented.
- Multi-user data isolation is enforced.
- Tests pass.
- Documentation explains architecture, APIs, settings, ranking, and operational behavior.

### 15. Engineering Quality Bar

Build this as a production feature, not a demo.

Code must be modular, readable, tested, secure, and maintainable.

Prefer simple, explainable ranking over magical complexity. A transparent scoring model is better than an impressive black box that nobody trusts.

Use feature flags where appropriate.

When external services are unavailable, provide graceful degradation. For example, fall back to keyword search when embeddings are temporarily unavailable, but clearly mark degraded mode.

At the end, provide a concise implementation summary including:

- Files changed.
- Major design decisions.
- Database migrations added.
- APIs added or changed.
- UI pages added or changed.
- How personalization works.
- How psychological principles were translated into product behavior.
- How to run tests.
- Known limitations.
- Recommended next improvements.