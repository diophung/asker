// URL routing for the full-page-load source tabs.
//
// The source tabs are REAL navigations: clicking a tab triggers a full document
// load of that tab's URL, and each tab is served by its own backend endpoint
// (/v1/search/<source>). So every tab is a distinct, shareable, bookmarkable URL
// that the app reconstructs its state from on load (parseLocation). The one
// motion that stays a soft in-page transition is home -> results — the spec's
// hero glide, which it explicitly forbids hard-swapping — which SearchApp drives
// with history.pushState. Tab clicks are plain <a href> links (SourceTabs).

import type { SourceFilter } from "./types";

const SOURCES: readonly SourceFilter[] = [
  "all",
  "email",
  "files",
  "messages",
  "calendar",
  "photos",
  "people",
];

function isSource(s: string): s is SourceFilter {
  return (SOURCES as readonly string[]).includes(s);
}

export interface Route {
  /** The submitted query; "" means the home (empty) state. */
  query: string;
  source: SourceFilter;
}

/** The path for home (the empty, centered state). */
export const homeUrl = "/";

/**
 * Build the URL for a (source, query) pair. "all" is the unfiltered /search
 * route; every other source is its own /search/<source> path. The query rides
 * in ?q so the page is shareable and reconstructable.
 */
export function searchUrl(source: SourceFilter, query: string): string {
  const qs = query ? `?q=${encodeURIComponent(query)}` : "";
  return source === "all" ? `/search${qs}` : `/search/${source}${qs}`;
}

/**
 * Parse a location into a Route. `/search/<source>` selects the source (default
 * "all"); `?q=` carries the query. An empty query is the home state.
 */
export function parseLocation(
  loc: { pathname: string; search: string } = window.location,
): Route {
  const query = (new URLSearchParams(loc.search).get("q") ?? "").trim();
  let source: SourceFilter = "all";
  const m = /^\/search\/([^/?]+)/.exec(loc.pathname);
  if (m && isSource(m[1])) {
    source = m[1];
  }
  return { query, source };
}
