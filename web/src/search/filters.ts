// UI filter state and its mapping onto the pinned /v1/search parameters.
import {
  DEFAULT_LIMIT,
  type DocTypeName,
  type SearchMode,
  type SearchRequest,
} from "../api";

export interface Filters {
  types: DocTypeName[];
  /** Date input value, "YYYY-MM-DD" or "". */
  from: string;
  /** Date input value, "YYYY-MM-DD" or "". */
  to: string;
  participant: string;
  mode: SearchMode;
}

export const defaultFilters: Filters = {
  types: [],
  from: "",
  to: "",
  participant: "",
  mode: "hybrid",
};

/** Sidebar checkbox options, in display order. Values are DocType enum names. */
export const DOC_TYPE_OPTIONS: ReadonlyArray<{
  value: DocTypeName;
  label: string;
}> = [
  { value: "EMAIL", label: "Email" },
  { value: "CHAT_MESSAGE", label: "Chat" },
  { value: "FILE", label: "File" },
  { value: "CALENDAR_EVENT", label: "Calendar" },
  { value: "WIKI_PAGE", label: "Wiki" },
  { value: "TICKET", label: "Ticket" },
  { value: "IMAGE", label: "Image" },
  { value: "VIDEO", label: "Video" },
  { value: "AUDIO", label: "Audio" },
];

const labelByType = new Map(DOC_TYPE_OPTIONS.map((o) => [o.value, o.label]));

/** Human label for a DocType enum name; falls back to the raw value. */
export function typeLabel(t: string): string {
  return labelByType.get(t as DocTypeName) ?? t;
}

/**
 * Convert submitted query + filter state + paging offset into a SearchRequest.
 * Date inputs are widened to full-day RFC3339 bounds in UTC: from at 00:00:00Z,
 * to at 23:59:59Z. Blank fields are omitted.
 */
export function toSearchRequest(
  q: string,
  f: Filters,
  offset: number,
  limit: number = DEFAULT_LIMIT,
): SearchRequest {
  const req: SearchRequest = {
    q,
    limit,
    offset,
    mode: f.mode,
  };
  if (f.types.length > 0) {
    req.types = [...f.types];
  }
  if (f.from !== "") {
    req.from = `${f.from}T00:00:00Z`;
  }
  if (f.to !== "") {
    req.to = `${f.to}T23:59:59Z`;
  }
  const participant = f.participant.trim();
  if (participant !== "") {
    req.participant = participant;
  }
  return req;
}
