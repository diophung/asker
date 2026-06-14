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
  PhotoResult,
  SearchResult,
  SourceFilter,
  SourceName,
} from "./types";

const env = import.meta.env;
/** Backend is on unless explicitly disabled (and never under vitest). */
export const BACKEND_ENABLED =
  env.MODE !== "test" && env.VITE_USE_BACKEND !== "0";

// Same-origin path the Vite dev proxy forwards to the gateway.
const SEARCH_PATH = "/v1/search";

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
}
interface GatewayResponse {
  hits: GatewayHit[];
  total: number;
  took_ms: number;
}

/** v2 SourceFilter -> gateway DocType enum names (comma-joined). */
const SOURCE_TYPES: Record<Exclude<SourceFilter, "all" | "people">, string[]> = {
  email: ["EMAIL"],
  files: ["FILE", "WIKI_PAGE", "TICKET"],
  messages: ["CHAT_MESSAGE"],
  calendar: ["CALENDAR_EVENT"],
  photos: ["IMAGE", "VIDEO", "AUDIO"],
};

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
  // The backend has no person/entity index; the People tab has nothing to show.
  if (source === "people") {
    lastMeta = { query, approx: "0", seconds: "0.00" };
    return [];
  }
  const token = await getToken();
  const params = new URLSearchParams({
    q: query,
    limit: "20",
    offset: "0",
    mode: searchMode,
  });
  if (source !== "all") {
    params.set("types", SOURCE_TYPES[source].join(","));
  }
  const res = await fetch(`${SEARCH_PATH}?${params.toString()}`, {
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
  const base = {
    id: hit.doc_id,
    source,
    when,
    title,
    snippet,
    haystack: "",
    url: linkFor(hit),
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
