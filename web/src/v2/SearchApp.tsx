import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { LogOut, Settings as SettingsIcon } from "lucide-react";
import { currentUser, isSignedIn, signOut } from "./auth";
import {
  BACKEND_ENABLED,
  getRecentSearches,
  removeRecentSearch,
  sendFeedback,
} from "./backend";
import { Settings } from "./Settings";
import {
  getSuggestions,
  metaFor,
  resolvePanel,
  searchPersonalData,
} from "./data";
import type { Panel, SearchResult, SourceFilter } from "./types";
import { KnowledgePanel } from "./KnowledgePanel";
import { ResultItem, type FeedbackAction } from "./ResultItem";
import { SearchBox } from "./SearchBox";
import { SignIn } from "./SignIn";
import { SourceTabs } from "./SourceTabs";
import { ErrorState, LoadingSkeleton, MetaLine, NoResults } from "./states";
import { homeUrl, parseLocation, searchUrl } from "./router";

type Phase = "idle" | "loading" | "done" | "error";

const DEAD_END_SUGGESTIONS = ["Sarah Chen", "Q3 planning", "budget", "design"];
const MAX_RECENTS = 20;

/** Google-homage wordmark; restrained everywhere else, per the spec. */
function Logo({ compact }: { compact?: boolean }) {
  const letters: [string, string][] = [
    ["A", "#4285f4"],
    ["s", "#ea4335"],
    ["k", "#fbbc04"],
    ["e", "#4285f4"],
    ["r", "#34a853"],
  ];
  return (
    <span
      className={[
        "font-semibold tracking-tight select-none transition-all duration-200",
        compact ? "text-[26px]" : "text-[64px] leading-none",
      ].join(" ")}
      aria-label="Asker"
    >
      {letters.map(([ch, color], i) => (
        <span key={i} style={{ color }} aria-hidden="true">
          {ch}
        </span>
      ))}
    </span>
  );
}

export function SearchApp() {
  // The whole results state is reconstructable from the URL: a full page load on
  // a tab (or a bookmark/share) lands here and we rebuild from it.
  const [initialRoute] = useState(parseLocation);
  const [box, setBox] = useState(initialRoute.query);
  const [query, setQuery] = useState(initialRoute.query); // submitted; "" = home
  const [source, setSource] = useState<SourceFilter>(initialRoute.source);
  const [phase, setPhase] = useState<Phase>(
    initialRoute.query === "" ? "idle" : "loading",
  );
  const [results, setResults] = useState<SearchResult[]>([]);
  const [panel, setPanel] = useState<Panel | null>(null);
  const [recents, setRecents] = useState<string[]>([]);
  // Backend mode needs a token — gate on a dev sign-in (restored from
  // sessionStorage across the tab reloads). Mock mode has no backend, no gate.
  const [authed, setAuthed] = useState(!BACKEND_ENABLED || isSignedIn());
  const [view, setView] = useState<"search" | "settings">("search");

  const reqId = useRef(0);
  const jumpToTop = useRef(false);
  const resultsRef = useRef<HTMLDivElement>(null);
  const initialFetched = useRef(false);

  const isHome = query === "";
  const suggestions = useMemo(
    () => getSuggestions(box, BACKEND_ENABLED ? recents : undefined),
    [box, recents],
  );
  const meta = useMemo(() => metaFor(query, results.length), [query, results.length]);

  // A single source's results (the active tab). Each tab is its own endpoint.
  const run = useCallback(async (q: string, src: SourceFilter) => {
    const id = ++reqId.current;
    setPhase("loading");
    try {
      const hits = await searchPersonalData(q, src);
      if (id !== reqId.current) {
        return; // a newer request superseded this one
      }
      setResults(hits);
      setPanel(resolvePanel(q));
      setPhase("done");
    } catch {
      if (id === reqId.current) {
        setPhase("error");
      }
    }
  }, []);

  const refreshRecents = useCallback(async () => {
    setRecents(await getRecentSearches());
  }, []);

  // Submit from the box / a suggestion: a SOFT transition (the hero glide, which
  // the spec forbids hard-swapping). Tab switches, by contrast, are full loads.
  const submit = useCallback(
    (q: string, opts?: { jump?: boolean }) => {
      const trimmed = q.trim();
      if (trimmed === "") {
        return;
      }
      setBox(trimmed);
      setQuery(trimmed);
      setSource("all");
      jumpToTop.current = opts?.jump ?? false;
      window.history.pushState({}, "", searchUrl("all", trimmed));
      void run(trimmed, "all");
      // Optimistically surface the just-searched query (the backend records it
      // server-side on the search request itself).
      if (BACKEND_ENABLED) {
        setRecents((prev) =>
          [trimmed, ...prev.filter((r) => r.toLowerCase() !== trimmed.toLowerCase())].slice(
            0,
            MAX_RECENTS,
          ),
        );
      }
    },
    [run],
  );

  const goHome = useCallback(() => {
    setBox("");
    setQuery("");
    setSource("all");
    setResults([]);
    setPanel(null);
    setPhase("idle");
    window.history.pushState({}, "", homeUrl);
  }, []);

  const handleRemoveRecent = useCallback((text: string) => {
    setRecents((prev) => prev.filter((r) => r !== text));
    void removeRecentSearch(text);
  }, []);

  // "More/Fewer like this" on a result -> a behavioral signal the ranker learns
  // from. Best-effort (sendFeedback never throws); carries the result's identity
  // + the query so the backend can attribute the signal.
  const handleFeedback = useCallback(
    (result: SearchResult, action: FeedbackAction) => {
      void sendFeedback({
        doc_id: result.id,
        doc_type: result.docType ?? "",
        connector_id: result.connectorId ?? "",
        senders: result.senders ?? [],
        topics: result.topics ?? [],
        action,
        query,
      });
    },
    [query],
  );

  // Initial fetch: once authed (in backend mode) and on mount, run the URL's
  // query and load recents. Runs once (the ref guards re-entry after sign-in).
  useEffect(() => {
    if (BACKEND_ENABLED && !authed) {
      return; // wait for sign-in
    }
    if (initialFetched.current) {
      return;
    }
    initialFetched.current = true;
    if (initialRoute.query !== "") {
      void run(initialRoute.query, initialRoute.source);
    }
    if (BACKEND_ENABLED) {
      void refreshRecents();
    }
  }, [authed, run, refreshRecents, initialRoute]);

  // Back/forward across the soft (pushState) home<->results transitions.
  useEffect(() => {
    function onPop() {
      const r = parseLocation();
      setBox(r.query);
      setQuery(r.query);
      setSource(r.source);
      if (r.query !== "") {
        void run(r.query, r.source);
      } else {
        setResults([]);
        setPanel(null);
        setPhase("idle");
      }
    }
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, [run]);

  // "Open top match": land the user on the single best result.
  useEffect(() => {
    if (phase === "done" && jumpToTop.current) {
      jumpToTop.current = false;
      const first = resultsRef.current?.querySelector<HTMLElement>("article a");
      first?.scrollIntoView({ block: "center", behavior: "smooth" });
      first?.focus();
    }
  }, [phase, results]);

  const signOutAll = useCallback(() => {
    signOut();
    setQuery("");
    setBox("");
    setSource("all");
    setResults([]);
    setPanel(null);
    setRecents([]);
    setPhase("idle");
    setView("search");
    setAuthed(false);
    window.history.pushState({}, "", homeUrl);
  }, []);

  // Auth gate (after all hooks). Backend mode requires a signed-in dev session.
  if (BACKEND_ENABLED && !authed) {
    return <SignIn onSignedIn={() => setAuthed(true)} />;
  }

  if (BACKEND_ENABLED && view === "settings") {
    return <Settings onBack={() => setView("search")} onSignOut={signOutAll} />;
  }

  const accountChip =
    BACKEND_ENABLED && authed ? (
      <div className="fixed right-3 top-3 z-30 flex items-center gap-2 rounded-full border border-gline bg-white/90 px-2.5 py-1.5 text-[12.5px] shadow-sm backdrop-blur">
        <span className="hidden text-gmuted sm:inline">{currentUser()}</span>
        <button
          type="button"
          onClick={() => setView("settings")}
          className="inline-flex items-center gap-1 text-gblue hover:underline"
          title="Settings"
        >
          <SettingsIcon className="size-3.5" /> Settings
        </button>
        <span aria-hidden="true" className="text-gline">
          |
        </span>
        <button
          type="button"
          onClick={signOutAll}
          className="inline-flex items-center gap-1 text-gblue hover:underline"
          title="Sign out"
        >
          <LogOut className="size-3.5" /> Sign out
        </button>
      </div>
    ) : null;

  const searchHeader = (
    <div
      className={[
        "mx-auto flex w-full px-4 transition-all duration-200 ease-out",
        isHome
          ? "max-w-[584px] flex-col items-center"
          : "max-w-[1100px] flex-row items-center gap-5",
      ].join(" ")}
    >
      <button
        type="button"
        onClick={goHome}
        className={isHome ? "mb-7" : "shrink-0"}
        aria-label="Asker home"
      >
        <Logo compact={!isHome} />
      </button>
      <div className={isHome ? "w-full" : "w-full max-w-[640px]"}>
        <SearchBox
          value={box}
          onChange={setBox}
          onSubmit={(q) => submit(q)}
          suggestions={suggestions}
          onRemoveRecent={BACKEND_ENABLED ? handleRemoveRecent : undefined}
          autoFocus
          variant={isHome ? "home" : "header"}
        />
      </div>
    </div>
  );

  if (isHome) {
    return (
      <main className="flex min-h-screen flex-col">
        {accountChip}
        <div className="flex flex-1 flex-col items-center justify-center pb-[18vh]">
          {searchHeader}
          <p className="mt-6 px-4 text-center text-[14px] text-gmuted">
            Everything you&rsquo;ve ever saved, in one search.
          </p>
          <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
            <button
              type="button"
              onClick={() => submit(box)}
              className="rounded-md bg-gbg-soft px-4 py-2 text-[14px] text-gink hover:shadow-sm disabled:opacity-50"
              disabled={box.trim() === ""}
            >
              Search
            </button>
            <button
              type="button"
              onClick={() => submit(box, { jump: true })}
              className="rounded-md bg-gbg-soft px-4 py-2 text-[14px] text-gink hover:shadow-sm disabled:opacity-50"
              disabled={box.trim() === ""}
              title="Jump straight to the single best result"
            >
              Open top match
            </button>
          </div>
        </div>
      </main>
    );
  }

  return (
    <main className="min-h-screen">
      {accountChip}
      <header className="sticky top-0 z-10 bg-white pt-3">
        <div className="relative h-0.5 overflow-hidden">
          {phase === "loading" && (
            <div className="v2-progress absolute inset-y-0 left-0 w-full bg-gblue" />
          )}
        </div>
        {searchHeader}
        <div className="mx-auto mt-2 max-w-[1100px] px-4">
          <SourceTabs active={source} query={query} />
        </div>
      </header>

      <div className="mx-auto max-w-[1100px] px-4 py-5">
        <div className="flex flex-col gap-10 lg:flex-row lg:gap-12">
          <section className="min-w-0 max-w-[600px] flex-1" aria-live="polite">
            {phase === "error" ? (
              <ErrorState onRetry={() => run(query, source)} />
            ) : phase === "loading" ? (
              <LoadingSkeleton />
            ) : results.length === 0 ? (
              <NoResults
                query={query}
                suggestions={DEAD_END_SUGGESTIONS}
                onSuggest={(q) => submit(q)}
              />
            ) : (
              <>
                <MetaLine approx={meta.approx} seconds={meta.seconds} />
                <div ref={resultsRef} className="mt-5 space-y-7">
                  {results.map((r) => (
                    <ResultItem
                      key={r.id}
                      result={r}
                      query={query}
                      onFeedback={BACKEND_ENABLED ? handleFeedback : undefined}
                    />
                  ))}
                </div>
              </>
            )}
          </section>

          {panel && phase !== "loading" && (
            <div className="w-full lg:w-[336px] lg:shrink-0">
              <KnowledgePanel panel={panel} />
            </div>
          )}
        </div>
      </div>
    </main>
  );
}
