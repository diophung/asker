# Asker — Search UI Prompt ("Google for your own data")

Build a search interface that *feels* like Google, but searches a single person's
private corpus (email, files, messages, calendar, photos, contacts) instead of the
public web. The goal is instant recognition — anyone who has used Google should feel
at home in under a second — while the result surface is adapted for **heterogeneous,
private, provenance-rich data**.

Follow this spec exactly where it is specific. Where it is silent, default to
"what would Google do," then ask "what changes because this is *my* data, not the web."

---

## Stack & constraints

- **React + TypeScript + Tailwind CSS.** Single self-contained component/app.
- **Icons:** `lucide-react`.
- **No backend.** All results come from a stubbed async function:
```ts
  async function searchPersonalData(query: string, source: SourceFilter): Promise<SearchResult[]>
```
  Back it with a realistic mock dataset (15–25 items spanning every result type below)
  and a ~150ms artificial delay so loading states are visible. **This stub is the only
  seam** — it must be trivially swappable for the real Asker API later, so keep all
  network/data concerns inside it and never reach into mock data elsewhere.
- State with `useState`/`useReducer` only. **No `localStorage`/`sessionStorage`.**
- Fully responsive (down to ~375px) and keyboard-navigable. Respect `prefers-reduced-motion`.

---

## Two states, one transition

This is the heart of the Google feel. Get the state machine right before styling.

**1. Home (empty query).** Centered, almost-empty white page. Wordmark logo, the search
box directly beneath it, nothing else competing for attention. Search box is autofocused.

**2. Results (active query).** Logo shrinks to the top-left, the search box moves up into
a slim header row, results stream in below. A thin meta line sits above the results.

**The transition matters.** Submitting a query should *move* the box from center to header
(animate position, ~200ms, eased), not hard-cut between two pages. This single motion is
what makes it read as Google rather than a generic search page.

---

## The search box (the hero — spend your polish here)

- Rounded pill, ~46px tall, soft 1px border that lifts into a subtle shadow on focus.
- Left: magnifying-glass icon. Right: a small mic/voice affordance (decorative is fine)
  and, when text is present, an `×` to clear.
- Placeholder in plain, active voice: **"Search your email, files, and messages"**
  (not "Enter query").
- **Autocomplete dropdown** on focus/typing — this is where personal data diverges from
  Google's "popular queries." Mix three typed row kinds, each with a leading icon:
  - **Recent searches** (clock icon)
  - **People** from the corpus (avatar) — e.g. "Sarah Chen"
  - **Documents/threads** (file icon) — e.g. "Q3 Planning Doc"

  Arrow-key navigable, Enter selects. This is the first signal that the engine knows
  *your* world, not the web's.

- On Home, you may keep two buttons under the box ("Search" + a second). Google's
  "I'm Feeling Lucky" maps poorly to private data — repurpose it as **"Open top match"**
  (jump straight to the single best result) or drop it. State which you chose and why.

---

## Result types (the central design problem)

Google's results are homogeneous — every one is a web page, so one "blue link" template
covers everything. **Asker's results are heterogeneous.** A result might be an email, a
PDF, a Slack message, a calendar event, or a photo. The job is to **fork the template by
source type while preserving Google's scannable, text-forward "ten blue links" rhythm** —
do *not* collapse into a card grid or dashboard.

Every result, regardless of type, has:
- A **provenance line** (Google's green URL equivalent) — small, above the title:
  `{source} · {who} · {when}` → e.g. `Gmail · from Sarah Chen · 3 days ago`,
  `Drive · /Projects/Q3 · edited Tuesday`. Source name carries a small brand-colored dot.
- A **title** in Google's link-blue (`#1a0dab`), ~20px, the primary tap target.
- A **snippet** in dark gray (`#4d5156`), ~14px, ~1.58 line-height, **query terms bolded**.
- ~28px vertical gap to the next result. Left-aligned, single column, ~600px max width.

Type-specific affordances layered on top:

| Type | Extra signals |
|---|---|
| **Email** | sender avatar, thread/reply count, attachment paperclip |
| **File/Doc** | file-type icon (PDF/Doc/Sheet), owner, folder breadcrumb |
| **Message** | sender + channel/DM name, reaction count |
| **Calendar event** | date/time block, attendee avatars, location |
| **Photo** | thumbnail on the left, date + place captured |
| **Person** | render in the side panel below, not the main column |

---

## Source filter tabs (Google's "All / Images / News")

A horizontal tab row under the header, same pattern as Google, different semantics:

`All · Email · Files · Messages · Calendar · Photos · People`

Active tab underlined in Google-blue; show a result count per tab where natural.
Switching filters re-runs `searchPersonalData(query, source)` — it does not just hide rows.

---

## Knowledge panel (Google's entity card, repurposed)

When the query resolves to a **person or project**, show a right-rail panel mirroring
Google's knowledge panel — but built from *the user's own* data:

- Person: avatar, role/org, **last contacted**, **shared documents**, **upcoming meeting**,
  quick contact actions.
- Project: a one-line summary, key people, recent activity, related files.

This is the clearest "this is *my* graph, not the web's" moment. Make it feel earned.

---

## The privacy signature (your one bold move)

Everything above keeps Google's restraint. Spend your boldness in exactly one place:
**make the privateness of the corpus tangible.** Where Google signals the *authority* of
public pages, Asker signals *isolation* — "this is yours, and only yours."

- The meta line above results reads:
  **`About 1,240 results from your data (0.18 seconds) · 🔒 only you can see these`**
- Each provenance line is source-attributed and timestamped, so every result visibly
  comes from a known place the user controls.
- A subtle, quiet lock motif near the search scope — not loud, not a banner.

Keep this disciplined: one trust layer, woven in. Do not turn it into a security dashboard.

---

## Empty, loading, and error states (copy is design material)

Write these in the interface's own voice — active, plain, never apologetic.

- **Home / empty:** the page itself is the empty state. If you want a line under the box:
  **"Everything you've ever saved, in one search."**
- **Loading:** thin Google-style progress shimmer or skeleton rows that match the result
  layout — not a centered spinner.
- **No results:** **"Nothing matched *{query}* in your data. Try a name, a date, or fewer words."**
  Offer the corpus-aware suggestions, not a dead end.
- **Error:** **"Couldn't reach your data just now. Retry."** — say what happened and the fix.

---

## Visual tokens (Google-faithful)

- **Background:** white / near-white. Generous whitespace. No borders on result rows.
- **Type:** clean neutral sans (`Inter`, `Roboto`, or `system-ui`). Title 20px, snippet
  14px/1.58, provenance 13px. Let the hierarchy carry the page.
- **Result palette:** title `#1a0dab` (blue), provenance `#006621`/gray (green), snippet
  `#4d5156` (gray). This trio is what the eye reads as "Google results."
- **Source brand colors** appear *only* as small badge dots typing each result
  (Gmail red, Drive blue/green/yellow, Slack aubergine, Calendar blue). Nowhere else.
- Soft shadows, generous radius on the search box; flat and quiet everywhere else.

---

## Anti-patterns — do not

- Turn results into a card grid, masonry, or dashboard. Keep the single scannable column.
- Add a left sidebar nav, widgets, charts, or KPI tiles.
- Clutter the home page with anything beyond logo + box.
- Use source brand colors as backgrounds or large fills.
- Replace the home→results *motion* with a hard page swap.

---

## Definition of done

A first-time user lands on a near-empty white page with a familiar box, types a name,
watches the box glide up as typed results stream in — emails, files, and messages, each
with clear provenance — switches to the Files tab, sees a person's knowledge panel on the
right, and never once doubts that everything on screen is private to them. It looks like
Google; it behaves like it knows them.