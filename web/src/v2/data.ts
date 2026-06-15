// THE ONLY SEAM. Everything network/data lives here so the UI never reaches into
// mock data directly — swap `searchPersonalData` for the real Asker /v1/search
// (it already returns provenance-rich, per-source hits) and the UI is unchanged.

import { BACKEND_ENABLED, backendMeta, searchBackend } from "./backend";
import type {
  Panel,
  PersonResult,
  SearchResult,
  SourceFilter,
  Suggestion,
} from "./types";

/** Distributive Omit so type-specific fields survive on the union. */
type NoHay<T> = T extends unknown ? Omit<T, "haystack"> : never;

const ARTIFICIAL_DELAY_MS = 150;

function withHaystack(r: NoHay<SearchResult>): SearchResult {
  const extra: string[] = [];
  switch (r.type) {
    case "email":
      extra.push(r.sender);
      break;
    case "file":
      extra.push(r.owner, r.folder, r.fileKind);
      break;
    case "message":
      extra.push(r.sender, r.channel);
      break;
    case "calendar":
      extra.push(...r.attendees, r.location ?? "");
      break;
    case "photo":
      extra.push(r.place ?? "");
      break;
    case "person":
      extra.push(r.name, r.role, r.org, r.email);
      break;
  }
  const haystack = [r.source, r.who, r.when, r.title, r.snippet, ...extra]
    .join(" ")
    .toLowerCase();
  return { ...r, haystack } as SearchResult;
}

// --- The corpus: 22 items across every result type, clustered around a small
// cast (Sarah Chen, Marcus Lee, Priya Patel, Tom Alvarez) and two projects
// (Q3 Planning, Budget) so queries return coherent, blended results. -----------
const CORPUS: SearchResult[] = (
  [
    {
      id: "e1",
      type: "email",
      source: "Gmail",
      who: "from Sarah Chen",
      when: "3 days ago",
      title: "Q3 Planning — agenda for Thursday",
      snippet:
        "Here's the draft agenda for our Q3 planning sync. Please add anything I've missed before Thursday — I want to lock scope and owners in the room.",
      sender: "Sarah Chen",
      replyCount: 4,
      hasAttachment: true,
    },
    {
      id: "e2",
      type: "email",
      source: "Gmail",
      who: "from Marcus Lee",
      when: "yesterday",
      title: "Re: Budget numbers for Q3",
      snippet:
        "Updated the engineering line items — we're about 8% under the planning estimate. Tom should sign off before the budget review.",
      sender: "Marcus Lee",
      replyCount: 2,
    },
    {
      id: "e3",
      type: "email",
      source: "Gmail",
      who: "from Priya Patel",
      when: "last week",
      title: "Design review notes + next steps",
      snippet:
        "Notes from the design review are attached. Two open questions on the planning dashboard layout — flagged for Sarah.",
      sender: "Priya Patel",
      hasAttachment: true,
    },
    {
      id: "e4",
      type: "email",
      source: "Gmail",
      who: "from Tom Alvarez",
      when: "2 weeks ago",
      title: "Budget approved ✔",
      snippet:
        "Finance signed off on the Q3 budget as proposed. Marcus — you're clear to start hiring for the two open roles.",
      sender: "Tom Alvarez",
    },
    {
      id: "f1",
      type: "file",
      source: "Drive",
      who: "/Projects/Q3",
      when: "edited Tuesday",
      title: "Q3 Planning Doc",
      snippet:
        "Goals, scope, owners, and the rough timeline for the quarter. Section 3 has the risk register we discussed in planning.",
      fileKind: "doc",
      owner: "Sarah Chen",
      folder: "My Drive / Projects / Q3",
    },
    {
      id: "f2",
      type: "file",
      source: "Drive",
      who: "/Finance",
      when: "edited 4 days ago",
      title: "Q3 Budget.xlsx",
      snippet:
        "Line-item budget by team with the planning vs. actual columns. Engineering is tracking under; marketing is over by ~3%.",
      fileKind: "sheet",
      owner: "Tom Alvarez",
      folder: "My Drive / Finance",
    },
    {
      id: "f3",
      type: "file",
      source: "Drive",
      who: "/Design",
      when: "edited last week",
      title: "Design System v2",
      snippet:
        "Component spec for the v2 overhaul — tokens, spacing, and the search result patterns. Owner: Priya.",
      fileKind: "doc",
      owner: "Priya Patel",
      folder: "My Drive / Design",
    },
    {
      id: "f4",
      type: "file",
      source: "Drive",
      who: "/Projects/Q3",
      when: "edited 3 weeks ago",
      title: "Q3 All-Hands.pptx",
      snippet:
        "Deck for the all-hands: planning highlights, budget summary, and the roadmap slide Sarah will present.",
      fileKind: "slides",
      owner: "Sarah Chen",
      folder: "My Drive / Projects / Q3",
    },
    {
      id: "f5",
      type: "file",
      source: "Drive",
      who: "/Legal",
      when: "edited a month ago",
      title: "Vendor Contract — Acme.pdf",
      snippet:
        "Signed master services agreement with Acme. Renewal date and the budget cap are on page 4.",
      fileKind: "pdf",
      owner: "Tom Alvarez",
      folder: "My Drive / Legal",
    },
    {
      id: "m1",
      type: "message",
      source: "Slack",
      who: "in #q3-planning",
      when: "2 hours ago",
      title: "Sarah Chen: let's lock the agenda by EOD",
      snippet:
        "let's lock the agenda by EOD so everyone can prep — I'll pin the planning doc to the channel.",
      sender: "Sarah Chen",
      channel: "#q3-planning",
      reactions: 5,
    },
    {
      id: "m2",
      type: "message",
      source: "Slack",
      who: "from Marcus Lee",
      when: "this morning",
      title: "Marcus Lee: can you send the budget sheet?",
      snippet:
        "can you send the budget sheet? want to cross-check the engineering numbers before the review.",
      sender: "Marcus Lee",
      channel: "Marcus Lee",
      reactions: 1,
    },
    {
      id: "m3",
      type: "message",
      source: "Slack",
      who: "in #design",
      when: "yesterday",
      title: "Priya Patel: shipped the planning dashboard mocks",
      snippet:
        "shipped the planning dashboard mocks — feedback welcome. Tried the result-list layout from the design system.",
      sender: "Priya Patel",
      channel: "#design",
      reactions: 8,
    },
    {
      id: "c1",
      type: "calendar",
      source: "Calendar",
      who: "with Sarah, Marcus, Priya",
      when: "Thu, Aug 14",
      title: "Q3 Planning Sync",
      snippet:
        "Lock scope and owners for the quarter. Agenda in the planning doc; Sarah to run.",
      start: "Thu, Aug 14 · 10:00 AM",
      month: "AUG",
      day: "14",
      attendees: ["Sarah Chen", "Marcus Lee", "Priya Patel"],
      location: "Conf Rm B · Google Meet",
    },
    {
      id: "c2",
      type: "calendar",
      source: "Calendar",
      who: "with Tom Alvarez",
      when: "Sat, Aug 16",
      title: "Budget Review",
      snippet:
        "Walk through the Q3 budget vs. plan with finance. Bring the engineering deltas.",
      start: "Sat, Aug 16 · 2:00 PM",
      month: "AUG",
      day: "16",
      attendees: ["Tom Alvarez", "Marcus Lee"],
      location: "Google Meet",
    },
    {
      id: "c3",
      type: "calendar",
      source: "Calendar",
      who: "with Sarah Chen",
      when: "Tue, Aug 12",
      title: "1:1 with Sarah",
      snippet: "Weekly 1:1 — planning blockers, hiring, and the all-hands deck.",
      start: "Tue, Aug 12 · 9:30 AM",
      month: "AUG",
      day: "12",
      attendees: ["Sarah Chen"],
    },
    {
      id: "p1",
      type: "photo",
      source: "Photos",
      who: "Conf Rm B",
      when: "3 days ago",
      title: "Whiteboard — Q3 roadmap",
      snippet: "Photo of the whiteboard from planning: swimlanes and the rough timeline.",
      place: "San Francisco",
      thumb: ["#1a73e8", "#34a853"],
    },
    {
      id: "p2",
      type: "photo",
      source: "Photos",
      who: "Offsite",
      when: "last month",
      title: "Team offsite — group shot",
      snippet: "The whole team at the Q3 kickoff offsite.",
      place: "Half Moon Bay",
      thumb: ["#fbbc04", "#ea4335"],
    },
    {
      id: "p3",
      type: "photo",
      source: "Photos",
      who: "Screenshot",
      when: "5 days ago",
      title: "Budget chart screenshot",
      snippet: "Screenshot of the budget burn-down chart for the planning deck.",
      place: undefined,
      thumb: ["#9334e6", "#1a73e8"],
    },
    {
      id: "per1",
      type: "person",
      source: "Contacts",
      who: "Product Manager · Acme",
      when: "last contacted yesterday",
      title: "Sarah Chen",
      snippet:
        "Product Manager leading Q3 planning. You've exchanged 38 emails and share 6 documents.",
      name: "Sarah Chen",
      role: "Product Manager",
      org: "Acme",
      lastContacted: "Yesterday",
      sharedDocs: ["Q3 Planning Doc", "Q3 All-Hands.pptx", "Roadmap 2026"],
      upcomingMeeting: "Q3 Planning Sync · Thu, Aug 14 · 10:00 AM",
      email: "sarah.chen@acme.com",
    },
    {
      id: "per2",
      type: "person",
      source: "Contacts",
      who: "Engineering Lead · Acme",
      when: "last contacted this morning",
      title: "Marcus Lee",
      snippet:
        "Engineering lead owning the Q3 budget for eng. You share 4 documents and 2 channels.",
      name: "Marcus Lee",
      role: "Engineering Lead",
      org: "Acme",
      lastContacted: "This morning",
      sharedDocs: ["Q3 Budget.xlsx", "Architecture Notes"],
      upcomingMeeting: "Budget Review · Sat, Aug 16 · 2:00 PM",
      email: "marcus.lee@acme.com",
    },
    {
      id: "per3",
      type: "person",
      source: "Contacts",
      who: "Design Lead · Acme",
      when: "last contacted last week",
      title: "Priya Patel",
      snippet:
        "Design lead on the v2 overhaul and the planning dashboard. You share 3 documents.",
      name: "Priya Patel",
      role: "Design Lead",
      org: "Acme",
      lastContacted: "Last week",
      sharedDocs: ["Design System v2", "Planning Dashboard Mocks"],
      email: "priya.patel@acme.com",
    },
  ] satisfies NoHay<SearchResult>[]
).map(withHaystack);

const SOURCE_OF: Record<Exclude<SourceFilter, "all">, SearchResult["type"]> = {
  email: "email",
  files: "file",
  messages: "message",
  calendar: "calendar",
  photos: "photo",
  people: "person",
};

function matches(r: SearchResult, terms: string[]): boolean {
  return terms.every((t) => r.haystack.includes(t));
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * THE STUB. Resolves the per-source hits for a query after a short artificial
 * delay (so loading states are visible). Throwing here surfaces the error state.
 */
export async function searchPersonalData(
  query: string,
  source: SourceFilter,
): Promise<SearchResult[]> {
  if (BACKEND_ENABLED) {
    return searchBackend(query, source);
  }
  await delay(ARTIFICIAL_DELAY_MS);
  const terms = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (terms.length === 0) {
    return [];
  }
  let hits = CORPUS.filter((r) => matches(r, terms));
  if (source !== "all") {
    hits = hits.filter((r) => r.type === SOURCE_OF[source]);
  }
  return hits;
}

// --- Autocomplete: typed rows (recent / people / docs), the first signal the
// engine knows YOUR world. Derived from the corpus, never reaching into it from
// the UI. --------------------------------------------------------------------

const RECENT_SEARCHES = [
  "q3 planning",
  "budget review",
  "sarah chen",
  "design system",
];

const PEOPLE = CORPUS.filter((r): r is PersonResult => r.type === "person");
const DOCS = CORPUS.filter((r) => r.type === "file");

export function getSuggestions(query: string, recents?: string[]): Suggestion[] {
  const q = query.trim().toLowerCase();
  const out: Suggestion[] = [];

  // In backend mode the caller passes the tenant's real recent searches; the
  // mock corpus falls back to a canned list. Filter by the typed prefix (and
  // drop an exact match of what's already typed), like Google.
  const source = recents ?? RECENT_SEARCHES;
  const matching = q
    ? source.filter((r) => r.toLowerCase().includes(q) && r.toLowerCase() !== q)
    : source;
  for (const text of matching.slice(0, 5)) {
    out.push({ kind: "recent", text });
  }

  // People/document suggestions come from the mock corpus only — the real
  // backend has no autocomplete/entity endpoint, so backend mode shows recents.
  if (q && !BACKEND_ENABLED) {
    for (const p of PEOPLE.filter((p) => p.haystack.includes(q)).slice(0, 2)) {
      out.push({ kind: "person", text: p.name, sub: `${p.role} · ${p.org}` });
    }
    for (const d of DOCS.filter((d) => d.haystack.includes(q)).slice(0, 2)) {
      out.push({ kind: "doc", text: d.title, sub: d.folder });
    }
  }
  return out.slice(0, 7);
}

// --- Meta line: a believable "About N results … (0.NN seconds)". Deterministic
// per query so it doesn't flicker between renders. ----------------------------

function hash(s: string): number {
  let h = 0;
  for (let i = 0; i < s.length; i++) {
    h = (h * 31 + s.charCodeAt(i)) | 0;
  }
  return Math.abs(h);
}

export function metaFor(
  query: string,
  shown: number,
): { approx: string; seconds: string } {
  if (BACKEND_ENABLED) {
    const real = backendMeta(query);
    if (real) {
      return real;
    }
  }
  const h = hash(query);
  const approx = (200 + (h % 1900) + shown * 7).toLocaleString();
  const seconds = (0.08 + (h % 24) / 100).toFixed(2);
  return { approx, seconds };
}

// --- Knowledge panel: resolve a person or project from the query, built from
// the user's OWN graph. -------------------------------------------------------

const PROJECTS: Record<string, Extract<Panel, { kind: "project" }>> = {
  q3: {
    kind: "project",
    name: "Q3 Planning",
    summary: "Quarterly planning across product, eng, and design — scope locked Aug 14.",
    people: ["Sarah Chen", "Marcus Lee", "Priya Patel"],
    recentActivity: [
      "Sarah edited Q3 Planning Doc · Tuesday",
      "Marcus updated Q3 Budget.xlsx · 4 days ago",
      "Priya shipped planning dashboard mocks · yesterday",
    ],
    files: ["Q3 Planning Doc", "Q3 Budget.xlsx", "Q3 All-Hands.pptx"],
  },
  budget: {
    kind: "project",
    name: "Q3 Budget",
    summary: "Line-item budget vs. plan for the quarter — approved by finance.",
    people: ["Tom Alvarez", "Marcus Lee", "Sarah Chen"],
    recentActivity: [
      "Tom approved the Q3 budget · 2 weeks ago",
      "Marcus updated engineering line items · yesterday",
    ],
    files: ["Q3 Budget.xlsx", "Vendor Contract — Acme.pdf"],
  },
};

export function resolvePanel(query: string): Panel | null {
  // The knowledge panel is built from the mock graph; the real backend has no
  // person/project entity index, so no panel in backend mode.
  if (BACKEND_ENABLED) {
    return null;
  }
  const q = query.trim().toLowerCase();
  if (!q) {
    return null;
  }
  const person = PEOPLE.find(
    (p) => q.includes(p.name.toLowerCase()) || p.name.toLowerCase().includes(q),
  );
  if (person && q.length >= 3) {
    return {
      kind: "person",
      name: person.name,
      role: person.role,
      org: person.org,
      lastContacted: person.lastContacted,
      sharedDocs: person.sharedDocs,
      upcomingMeeting: person.upcomingMeeting,
      email: person.email,
    };
  }
  if (q.includes("q3") || q.includes("planning")) {
    return PROJECTS.q3;
  }
  if (q.includes("budget")) {
    return PROJECTS.budget;
  }
  return null;
}
