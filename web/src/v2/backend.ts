// Real-backend implementation of the search seam. The UI calls
// `searchPersonalData` (data.ts); when the backend is enabled this maps the
// gateway's /v1/search hits onto the v2 result model. Everything backend-shaped
// stays in this file — the components never see a gateway hit.
//
// AUTH (DEV ONLY): the v2 dev server runs on a port the Keycloak realm does not
// allow as a redirect URI, so the production OIDC redirect flow (src/auth.ts)
// can't be used here. Instead we get a token via the resource-owner password
// grant with dev credentials, through the Vite dev proxy (so the browser stays
// same-origin and no CORS / redirect-URI config is touched). In production the
// seam swaps back to the real OIDC token from src/auth.ts. NEVER ship this.

import { getToken } from "./auth";
import type {
  CalendarResult,
  EmailResult,
  FileResult,
  MessageResult,
  PersonResult,
  PhotoResult,
  SearchResult,
  SourceFilter,
  SourceName,
} from "./types";

const env = import.meta.env;
/** Backend is on unless explicitly disabled (and never under vitest). */
export const BACKEND_ENABLED =
  env.MODE !== "test" && env.VITE_USE_BACKEND !== "0";

// Same-origin path the proxy forwards to the gateway.
const SEARCH_PATH = "/v1/search";

/**
 * The endpoint for a source tab. "all" is the unfiltered /v1/search; every
 * other tab is served by its OWN endpoint (/v1/search/<source>) — the source is
 * the path, not a query param, so each tab is genuinely a distinct endpoint the
 * full page load navigates to.
 */
function endpointFor(source: SourceFilter): string {
  return source === "all" ? SEARCH_PATH : `${SEARCH_PATH}/${source}`;
}

// --- Search preference (in-memory; the Settings page sets it). --------------

export type SearchMode = "hybrid" | "keyword" | "vector";
let searchMode: SearchMode = "hybrid";
export function getSearchMode(): SearchMode {
  return searchMode;
}
export function setSearchMode(m: SearchMode): void {
  searchMode = m;
}

// --- Account + data controls (Settings page). -------------------------------

export interface Me {
  email: string;
  tenantId: string;
}

export async function getMe(): Promise<Me> {
  const token = await getToken();
  const res = await fetch("/v1/me", {
    headers: { Authorization: `Bearer ${token}` },
  });
  if (!res.ok) {
    throw new Error(`HTTP ${res.status}`);
  }
  const j = (await res.json()) as { email: string; tenant_id: string };
  return { email: j.email, tenantId: j.tenant_id };
}

/** GDPR per-tenant erasure (DELETE /v1/me/data) — DESTRUCTIVE, caller-confirmed. */
export async function deleteMyData(): Promise<Record<string, unknown>> {
  const token = await getToken();
  const res = await fetch("/v1/me/data", {
    method: "DELETE",
    headers: { Authorization: `Bearer ${token}` },
  });
  if (!res.ok) {
    throw new Error(`Delete failed (HTTP ${res.status})`);
  }
  return (await res.json()) as Record<string, unknown>;
}

// --- Personalization (v3.2): the user's resolved preference profile, the
// transparency controls, and the behavioral-feedback signal. The wire shape is
// snake_case (mirrors platform/personalization/profile.go); the gateway clamps
// every field, so the UI can send partial-ish profiles and trust the response.

/** A DocType enum name used to key SourceWeights (1.0 when unset). */
export type DocType =
  | "EMAIL"
  | "CHAT_MESSAGE"
  | "FILE"
  | "CALENDAR_EVENT"
  | "WIKI_PAGE"
  | "TICKET"
  | "IMAGE"
  | "VIDEO"
  | "AUDIO";

export interface WorkingHours {
  start_hour: number;
  end_hour: number;
}

export interface MuteList {
  people: string[];
  topics: string[];
  sources: string[];
}

/** The combined-relevance-score coefficients (partly tunable, partly learned). */
export interface ScoreWeights {
  semantic: number;
  preference: number;
  behavioral: number;
  attention: number;
  fatigue: number;
}

/**
 * The single resolved UserPreferenceProfile the ranker reads — every field maps
 * to a named ranking input. Mirrors platform/personalization/profile.go.
 */
export interface Profile {
  version: number;
  source_weights: Partial<Record<DocType, number>>;
  important_people: string[];
  topics: string[];
  mute: MuteList;
  self_emails: string[];
  timezone: string;
  working_hours: WorkingHours;
  /** Signal-detection criterion in [0,1]: 0 = show everything, 1 = critical few. */
  attention_sensitivity: number;
  /** [0,1]: 0 = importance, 1 = recency. */
  recency_vs_importance: number;
  /** [0,1]: 0 = familiar (exploit), 1 = novel (explore). */
  novelty_vs_familiarity: number;
  learning_paused: boolean;
  weights: ScoreWeights;
}

/** Cold-start defaults mirroring personalization.DefaultProfile (Go). Used in
 * mock mode (no backend) so the Settings page still renders sensibly. */
export function defaultProfile(): Profile {
  return {
    version: 0,
    source_weights: {},
    important_people: [],
    topics: [],
    mute: { people: [], topics: [], sources: [] },
    self_emails: [],
    timezone: "",
    working_hours: { start_hour: 9, end_hour: 17 },
    attention_sensitivity: 0.5,
    recency_vs_importance: 0.5,
    novelty_vs_familiarity: 0.15,
    learning_paused: false,
    weights: {
      semantic: 1.0,
      preference: 0.6,
      behavioral: 0.5,
      attention: 0.8,
      fatigue: 0.3,
    },
  };
}

/** ProfileWire is the profile as it arrives on the wire: Go serializes empty
 * slices/maps as `null`, so every array/object field can be null even though the
 * Profile type models them as non-null. normalizeProfile bridges the two. */
interface ProfileWire {
  version?: number | null;
  source_weights?: Partial<Record<DocType, number>> | null;
  important_people?: string[] | null;
  topics?: string[] | null;
  mute?: {
    people?: string[] | null;
    topics?: string[] | null;
    sources?: DocType[] | null;
  } | null;
  self_emails?: string[] | null;
  timezone?: string | null;
  working_hours?: WorkingHours | null;
  attention_sensitivity?: number | null;
  recency_vs_importance?: number | null;
  novelty_vs_familiarity?: number | null;
  learning_paused?: boolean | null;
  weights?: ScoreWeights | null;
}

/** normalizeProfile coerces a wire profile (with possibly-null arrays/objects)
 * into a fully-populated Profile, falling back to cold-start defaults for any
 * missing field. Without this the Settings page calls .map()/.includes() on a
 * null array (the cold-start response) and renders a blank screen. */
export function normalizeProfile(raw: ProfileWire | null | undefined): Profile {
  const d = defaultProfile();
  const r = raw ?? {};
  const mute = r.mute ?? {};
  return {
    version: r.version ?? 0,
    source_weights: r.source_weights ?? {},
    important_people: r.important_people ?? [],
    topics: r.topics ?? [],
    mute: {
      people: mute.people ?? [],
      topics: mute.topics ?? [],
      sources: mute.sources ?? [],
    },
    self_emails: r.self_emails ?? [],
    timezone: r.timezone ?? "",
    working_hours: r.working_hours ?? d.working_hours,
    attention_sensitivity: r.attention_sensitivity ?? d.attention_sensitivity,
    recency_vs_importance: r.recency_vs_importance ?? d.recency_vs_importance,
    novelty_vs_familiarity: r.novelty_vs_familiarity ?? d.novelty_vs_familiarity,
    learning_paused: r.learning_paused ?? false,
    weights: r.weights ?? d.weights,
  };
}

export interface PreferencesResponse {
  profile: Profile;
  sampleCount: number;
}

// In-memory mock store so the UI round-trips edits when there's no backend.
let mockProfile: Profile | null = null;
let mockSampleCount = 0;

/** GET /v1/preferences — the resolved profile + how many interactions were
 * learned from. In mock mode returns (and persists) sensible defaults. */
export async function getPreferences(): Promise<PreferencesResponse> {
  if (!BACKEND_ENABLED) {
    mockProfile ??= defaultProfile();
    return { profile: mockProfile, sampleCount: mockSampleCount };
  }
  const token = await getToken();
  const res = await fetch("/v1/preferences", {
    headers: { Authorization: `Bearer ${token}` },
  });
  if (!res.ok) {
    throw new Error(`Couldn't load preferences (HTTP ${res.status})`);
  }
  const j = (await res.json()) as { profile?: ProfileWire | null; sample_count?: number };
  // Go serializes empty arrays as null; normalize so the Settings UI never
  // dereferences a null array (the cold-start blank-screen bug).
  return { profile: normalizeProfile(j.profile), sampleCount: j.sample_count ?? 0 };
}

/** PUT /v1/preferences — validate + persist; returns the bumped version. */
export async function savePreferences(profile: Profile): Promise<number> {
  if (!BACKEND_ENABLED) {
    mockProfile = { ...profile, version: profile.version + 1 };
    return mockProfile.version;
  }
  const token = await getToken();
  const res = await fetch("/v1/preferences", {
    method: "PUT",
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(profile),
  });
  if (!res.ok) {
    throw new Error(`Couldn't save preferences (HTTP ${res.status})`);
  }
  const j = (await res.json()) as { version: number };
  return j.version;
}

/** One behavioral interaction sent to POST /v1/feedback. */
export interface FeedbackEvent {
  doc_id: string;
  doc_type: string;
  connector_id: string;
  senders: string[];
  topics: string[];
  action:
    | "open"
    | "click"
    | "reply"
    | "show_more"
    | "dismiss"
    | "show_fewer";
  dwell_ms?: number;
  query: string;
}

export interface FeedbackResponse {
  sampleCount: number;
  learningPaused: boolean;
}

/** POST /v1/feedback — record a behavioral event. Best-effort: never throws, so
 * a failed signal can't break the result list. */
export async function sendFeedback(ev: FeedbackEvent): Promise<FeedbackResponse> {
  if (!BACKEND_ENABLED) {
    mockSampleCount += 1;
    return { sampleCount: mockSampleCount, learningPaused: false };
  }
  try {
    const token = await getToken();
    const res = await fetch("/v1/feedback", {
      method: "POST",
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ dwell_ms: 0, ...ev }),
    });
    if (!res.ok) {
      return { sampleCount: 0, learningPaused: false };
    }
    const j = (await res.json()) as {
      sample_count: number;
      learning_paused: boolean;
    };
    return {
      sampleCount: j.sample_count ?? 0,
      learningPaused: j.learning_paused ?? false,
    };
  } catch {
    return { sampleCount: 0, learningPaused: false };
  }
}

/** POST /v1/preferences/reset — erase what we've learned; returns the count of
 * feedback rows deleted. */
export async function resetLearning(): Promise<number> {
  if (!BACKEND_ENABLED) {
    const deleted = mockSampleCount;
    mockSampleCount = 0;
    return deleted;
  }
  const token = await getToken();
  const res = await fetch("/v1/preferences/reset", {
    method: "POST",
    headers: { Authorization: `Bearer ${token}` },
  });
  if (!res.ok) {
    throw new Error(`Couldn't reset learning (HTTP ${res.status})`);
  }
  const j = (await res.json()) as { feedback_deleted: number };
  return j.feedback_deleted ?? 0;
}

/** GET /v1/preferences/export — the full data-rights export (profile + learned
 * model + sample count). */
export async function exportPersonalization(): Promise<Record<string, unknown>> {
  if (!BACKEND_ENABLED) {
    mockProfile ??= defaultProfile();
    return {
      profile: mockProfile,
      learned_model: {},
      sample_count: mockSampleCount,
    };
  }
  const token = await getToken();
  const res = await fetch("/v1/preferences/export", {
    headers: { Authorization: `Bearer ${token}` },
  });
  if (!res.ok) {
    throw new Error(`Couldn't export your data (HTTP ${res.status})`);
  }
  return (await res.json()) as Record<string, unknown>;
}

// --- Indexing status (Settings page). ---------------------------------------
// Live view of the async ingest pipeline: how many documents are searchable in
// Vespa (`indexed`) vs. how many connectors have pulled from sources
// (`emitted`), the in-flight `backlog` between them, and per-connector sync
// phase. A per-connector "Re-index" resets a connector to re-pull + re-index
// from scratch (POST /v1/connectors/{id}/reindex).

/** One connector's slice of the indexing status. */
export interface IndexConnectorStatus {
  id: string;
  connector_id: string;
  display_name: string;
  /** "PENDING" | "FULL_SYNC" | "INCREMENTAL" | "FAILED" | "SYNC_PHASE_UNSPECIFIED" */
  phase: string;
  docs_emitted: number;
  /** RFC3339, or "" when never synced. */
  last_sync_completed: string;
  last_error: string;
}

export interface IndexStatus {
  /** Documents currently searchable in Vespa. -1 means "unknown" (the query
   * service was unreachable) — render as "—", never the literal -1. */
  indexed: number;
  /** Documents pulled from sources by connectors. */
  emitted: number;
  /** max(0, emitted - indexed): still flowing through the pipeline. */
  backlog: number;
  /** A backfill/re-index is in progress. */
  syncing: boolean;
  connectors: IndexConnectorStatus[];
}

/** GET /v1/index/status — the live indexing snapshot. In mock mode returns a
 * sensible fake so the Settings page renders without a backend. */
export async function getIndexStatus(): Promise<IndexStatus> {
  if (!BACKEND_ENABLED) {
    return {
      indexed: 1200,
      emitted: 1200,
      backlog: 0,
      syncing: false,
      connectors: [
        {
          id: "mock-gmail",
          connector_id: "gmail",
          display_name: "Gmail",
          phase: "INCREMENTAL",
          docs_emitted: 1200,
          last_sync_completed: new Date(Date.now() - 5 * 60_000).toISOString(),
          last_error: "",
        },
      ],
    };
  }
  const token = await getToken();
  const res = await fetch("/v1/index/status", {
    headers: { Authorization: `Bearer ${token}` },
  });
  if (!res.ok) {
    throw new Error(`Couldn't load indexing status (HTTP ${res.status})`);
  }
  const j = (await res.json()) as Partial<IndexStatus> & {
    connectors?: Partial<IndexConnectorStatus>[] | null;
  };
  return {
    indexed: typeof j.indexed === "number" ? j.indexed : -1,
    emitted: j.emitted ?? 0,
    backlog: j.backlog ?? 0,
    syncing: j.syncing ?? false,
    connectors: (j.connectors ?? []).map((c) => ({
      id: c?.id ?? "",
      connector_id: c?.connector_id ?? "",
      display_name: c?.display_name ?? "",
      phase: c?.phase ?? "",
      docs_emitted: c?.docs_emitted ?? 0,
      last_sync_completed: c?.last_sync_completed ?? "",
      last_error: c?.last_error ?? "",
    })),
  };
}

/** POST /v1/connectors/{id}/reindex — reset a connector so it re-pulls and
 * re-indexes from scratch. Best-effort: throws on a non-2xx so the caller can
 * surface the failure, but in mock mode resolves immediately. */
export async function reindexConnector(id: string): Promise<void> {
  if (!BACKEND_ENABLED) {
    return;
  }
  const token = await getToken();
  const res = await fetch(
    `/v1/connectors/${encodeURIComponent(id)}/reindex`,
    {
      method: "POST",
      headers: { Authorization: `Bearer ${token}` },
    },
  );
  if (!res.ok) {
    throw new Error(`Couldn't start re-indexing (HTTP ${res.status})`);
  }
}

// --- The gateway /v1/search wire shape (mirrors web/src/api.ts Hit). ---------

interface GatewayHit {
  doc_id: string;
  connector_id: string;
  type: string;
  title: string;
  snippet: string;
  score: number;
  created: string;
  modified: string;
  metadata: Record<string, string>;
  source_url: string;
  start_ms: number;
  end_ms: number;
  modality: string;
  thumbnail_key: string;
  // v3 personalization: "why this ranked" + optional per-feature contributions.
  explanation: string;
  features?: Record<string, number>;
}
interface GatewayResponse {
  hits: GatewayHit[];
  total: number;
  took_ms: number;
}

// The /v1/search/people wire shape (derived contacts — not document hits).
interface GatewayPerson {
  name: string;
  email: string;
  count: number;
  last_contacted: string;
  summary: string;
}
interface PeopleResponse {
  people: GatewayPerson[];
  total: number;
  took_ms: number;
}

let lastMeta: { query: string; approx: string; seconds: string } | null = null;

export function backendMeta(
  query: string,
): { approx: string; seconds: string } | null {
  return lastMeta && lastMeta.query === query
    ? { approx: lastMeta.approx, seconds: lastMeta.seconds }
    : null;
}

export async function searchBackend(
  query: string,
  source: SourceFilter,
): Promise<SearchResult[]> {
  // People are derived (no person index): a separate endpoint + wire shape.
  if (source === "people") {
    return searchPeople(query);
  }
  const token = await getToken();
  const params = new URLSearchParams({
    q: query,
    limit: "20",
    offset: "0",
    mode: searchMode,
  });
  const res = await fetch(`${endpointFor(source)}?${params.toString()}`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  if (!res.ok) {
    throw new Error(`search failed (HTTP ${res.status})`);
  }
  const data = (await res.json()) as GatewayResponse;
  lastMeta = {
    query,
    approx: data.total.toLocaleString(),
    seconds: (data.took_ms / 1000).toFixed(2),
  };
  return data.hits.map(mapHit);
}

/** The People tab: derived contacts from /v1/search/people. */
async function searchPeople(query: string): Promise<SearchResult[]> {
  const token = await getToken();
  const params = new URLSearchParams({ q: query });
  const res = await fetch(`${SEARCH_PATH}/people?${params.toString()}`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  if (!res.ok) {
    throw new Error(`people search failed (HTTP ${res.status})`);
  }
  const data = (await res.json()) as PeopleResponse;
  lastMeta = {
    query,
    approx: data.total.toLocaleString(),
    seconds: (data.took_ms / 1000).toFixed(2),
  };
  return data.people.map(mapPerson);
}

function mapPerson(p: GatewayPerson): PersonResult {
  const when = p.last_contacted ? relativeTime(p.last_contacted) : "";
  return {
    id: `person:${p.email || p.name}`,
    type: "person",
    source: "Contacts",
    who: p.summary || "Contact",
    when,
    title: p.name,
    snippet: p.summary,
    haystack: "",
    name: p.name,
    role: "",
    org: "",
    lastContacted: when,
    sharedDocs: [],
    email: p.email,
  };
}

// --- Recent searches (backend-stored, per tenant). --------------------------

/** The caller's recent queries, newest first; [] on any failure (non-critical). */
export async function getRecentSearches(): Promise<string[]> {
  try {
    const token = await getToken();
    const res = await fetch("/v1/searches/recent", {
      headers: { Authorization: `Bearer ${token}` },
    });
    if (!res.ok) {
      return [];
    }
    const j = (await res.json()) as { searches?: string[] };
    return Array.isArray(j.searches) ? j.searches : [];
  } catch {
    return [];
  }
}

/** Remove one recent search (best effort). */
export async function removeRecentSearch(q: string): Promise<void> {
  try {
    const token = await getToken();
    await fetch(`/v1/searches?q=${encodeURIComponent(q)}`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${token}` },
    });
  } catch {
    // best effort — the optimistic UI update already removed it locally.
  }
}

// --- Mapping: gateway hit -> v2 result. -------------------------------------

const CONNECTOR_SOURCE: Record<string, SourceName> = {
  gmail: "Gmail",
  "outlook-mail": "Gmail",
  gdrive: "Drive",
  s3: "Drive",
  jira: "Drive",
  confluence: "Drive",
  slack: "Slack",
  msteams: "Slack",
  "whatsapp-export": "Slack",
  "imessage-agent": "Slack",
  gcal: "Calendar",
  "outlook-cal": "Calendar",
  ical: "Calendar",
  upload: "Photos",
  contacts: "Contacts",
};

function sourceOf(connectorId: string): SourceName {
  return CONNECTOR_SOURCE[connectorId] ?? "Drive";
}

/** "Paraform <team@paraform.com>" -> "Paraform"; bare email -> the email. */
function displayName(addr: string): string {
  const m = /^\s*"?([^"<]*?)"?\s*<[^>]*>\s*$/.exec(addr);
  const name = m?.[1]?.trim();
  return name && name.length > 0 ? name : addr.trim();
}

function relativeTime(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) {
    return "";
  }
  const days = Math.floor((Date.now() - t) / 86_400_000);
  if (days <= 0) return "today";
  if (days === 1) return "yesterday";
  if (days < 7) return `${days} days ago`;
  if (days < 30) return `${Math.floor(days / 7)} weeks ago`;
  return new Date(t).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
  });
}

const MONTHS = ["JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"];

/** Strip the gateway snippet's <hi>/<sep> markup + zero-width junk to plain
 * text; the v2 Highlight component re-bolds the query terms. */
function sanitizeSnippet(s: string): string {
  return s
    .replace(/<\/?hi>/gi, "")
    .replace(/<sep\s*\/?>/gi, " ")
    .replace(/[\u200b-\u200f\u2028\u2029\ufeff]/g, "") // zero-width / bidi / BOM
    .replace(/\u00a0/g, " ") // nbsp
    .replace(/\s+/g, " ")
    .trim();
}

/** Gmail web deep link from the message id when the connector left it empty. */
function linkFor(hit: GatewayHit): string {
  if (hit.source_url) return hit.source_url;
  const md = hit.metadata;
  if (md.html_link) return md.html_link;
  if (md.web_link) return md.web_link;
  if (md.web_view_link) return md.web_view_link;
  if (md.permalink) return md.permalink;
  if (hit.connector_id === "gmail" && md.message_id) {
    return `https://mail.google.com/mail/u/0/#all/${md.message_id}`;
  }
  return "";
}

function mapHit(hit: GatewayHit): SearchResult {
  const md = hit.metadata ?? {};
  const source = sourceOf(hit.connector_id);
  const when = relativeTime(hit.modified || hit.created);
  const snippet = sanitizeSnippet(hit.snippet) || "(no preview)";
  const title = hit.title || "(untitled)";
  // Participant addresses for personalization feedback (the ranker keys
  // important-people / sender-fatigue on these); raw addresses, not display
  // names, so the backend can match them against the profile.
  const senders = [md.from, md.sender, md.author, md.owner]
    .filter((v): v is string => !!v)
    .map((v) => v.trim());
  const base = {
    id: hit.doc_id,
    source,
    when,
    title,
    snippet,
    haystack: "",
    url: linkFor(hit),
    connectorId: hit.connector_id,
    docType: hit.type,
    senders,
    topics: [] as string[],
    explanation: hit.explanation ?? "",
  };

  switch (hit.type) {
    case "CALENDAR_EVENT": {
      const start = md.start ? new Date(md.start) : null;
      const valid = start && !Number.isNaN(start.getTime());
      const attendees = Object.keys(md)
        .filter((k) => k.startsWith("response_status:"))
        .map((k) => displayName(k.slice("response_status:".length)));
      const r: CalendarResult = {
        ...base,
        type: "calendar",
        who: attendees.length ? `with ${attendees[0]}` : (md.location ?? "Event"),
        start: valid
          ? start.toLocaleString(undefined, {
              weekday: "short",
              month: "short",
              day: "numeric",
              hour: "numeric",
              minute: "2-digit",
            })
          : "",
        month: valid ? MONTHS[start.getMonth()] : "",
        day: valid ? String(start.getDate()) : "",
        attendees,
        location: md.location,
      };
      return r;
    }
    case "CHAT_MESSAGE": {
      const sender = displayName(md.from ?? md.sender ?? hit.connector_id);
      const r: MessageResult = {
        ...base,
        type: "message",
        who: md.channel ? `in ${md.channel}` : `from ${sender}`,
        sender,
        channel: md.channel ?? md.team ?? sender,
      };
      return r;
    }
    case "IMAGE":
    case "VIDEO":
    case "AUDIO": {
      const r: PhotoResult = {
        ...base,
        type: "photo",
        who: hit.connector_id,
        place: md.place,
        thumb: ["#1a73e8", "#9334e6"],
      };
      return r;
    }
    case "FILE":
    case "WIKI_PAGE":
    case "TICKET": {
      const r: FileResult = {
        ...base,
        type: "file",
        who: md.folder ?? md.web_url ?? "File",
        fileKind: fileKindOf(md.mime_type ?? md.content_type ?? "", title),
        owner: displayName(md.owner ?? md.author ?? ""),
        folder: md.folder ?? linkFor(hit),
      };
      return r;
    }
    default: {
      // EMAIL (and anything unknown) -> the email template.
      const from = md.from ?? "";
      const sender = displayName(from) || "Unknown sender";
      const r: EmailResult = {
        ...base,
        type: "email",
        who: from ? `from ${sender}` : (hit.connector_id || "message"),
        sender,
        hasAttachment: md.has_attachment === "true",
      };
      return r;
    }
  }
}

function fileKindOf(mime: string, title: string): FileResult["fileKind"] {
  const s = `${mime} ${title}`.toLowerCase();
  if (s.includes("pdf")) return "pdf";
  if (s.includes("sheet") || s.includes("excel") || s.endsWith(".xlsx")) return "sheet";
  if (s.includes("presentation") || s.includes("slides") || s.endsWith(".pptx")) return "slides";
  if (s.includes("image") || s.includes("png") || s.includes("jpg")) return "image";
  if (s.includes("doc") || s.includes("word")) return "doc";
  return "other";
}
