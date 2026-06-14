import { useEffect, useMemo, useRef, useState } from "react";
import { LogOut, Settings as SettingsIcon } from "lucide-react";
import { currentUser, signOut } from "./auth";
import { BACKEND_ENABLED } from "./backend";
import { Settings } from "./Settings";
import {
  getSuggestions,
  metaFor,
  resolvePanel,
  searchPersonalData,
} from "./data";
import type { Panel, SearchResult, SourceFilter } from "./types";
import { KnowledgePanel } from "./KnowledgePanel";
import { ResultItem } from "./ResultItem";
import { SearchBox } from "./SearchBox";
import { SignIn } from "./SignIn";
import { SourceTabs } from "./SourceTabs";
import { ErrorState, LoadingSkeleton, MetaLine, NoResults } from "./states";

type Phase = "idle" | "loading" | "done" | "error";

const TYPE_TO_FILTER: Record<SearchResult["type"], SourceFilter> = {
  email: "email",
  file: "files",
  message: "messages",
  calendar: "calendar",
  photo: "photos",
  person: "people",
};

const DEAD_END_SUGGESTIONS = ["Sarah Chen", "Q3 planning", "budget", "design"];

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
  const [box, setBox] = useState("");
  const [query, setQuery] = useState(""); // submitted query; "" = home
  const [source, setSource] = useState<SourceFilter>("all");
  const [phase, setPhase] = useState<Phase>("idle");
  const [results, setResults] = useState<SearchResult[]>([]);
  const [counts, setCounts] = useState<Partial<Record<SourceFilter, number>>>({});
  const [panel, setPanel] = useState<Panel | null>(null);
  // In backend mode the gateway needs a token — gate on a dev sign-in. In mock
  // mode there is no backend, so no sign-in is required.
  const [authed, setAuthed] = useState(!BACKEND_ENABLED);
  const [view, setView] = useState<"search" | "settings">("search");

  const reqId = useRef(0);
  const jumpToTop = useRef(false);
  const resultsRef = useRef<HTMLDivElement>(null);

  const isHome = query === "";
  const suggestions = useMemo(() => getSuggestions(box), [box]);
  const meta = useMemo(() => metaFor(query, results.length), [query, results.length]);

  async function run(q: string, src: SourceFilter, recount: boolean) {
    const id = ++reqId.current;
    setPhase("loading");
    try {
      // Switching a source RE-RUNS the search (not a client-side hide). The
      // unfiltered pass feeds the per-tab counts + the entity panel.
      const all = recount ? await searchPersonalData(q, "all") : null;
      const hits =
        src === "all" && all ? all : await searchPersonalData(q, src);
      if (id !== reqId.current) {
        return; // a newer request superseded this one
      }
      if (all) {
        const c: Partial<Record<SourceFilter, number>> = { all: all.length };
        for (const r of all) {
          const f = TYPE_TO_FILTER[r.type];
          c[f] = (c[f] ?? 0) + 1;
        }
        setCounts(c);
        setPanel(resolvePanel(q));
      }
      setResults(hits);
      setPhase("done");
    } catch {
      if (id === reqId.current) {
        setPhase("error");
      }
    }
  }

  function submit(q: string, opts?: { jump?: boolean }) {
    setBox(q);
    setQuery(q);
    setSource("all");
    jumpToTop.current = opts?.jump ?? false;
    void run(q, "all", true);
  }

  function changeSource(next: SourceFilter) {
    if (next === source) {
      return;
    }
    setSource(next);
    void run(query, next, false);
  }

  // "Open top match": land the user on the single best result.
  useEffect(() => {
    if (phase === "done" && jumpToTop.current) {
      jumpToTop.current = false;
      const first = resultsRef.current?.querySelector<HTMLElement>("article a");
      first?.scrollIntoView({ block: "center", behavior: "smooth" });
      first?.focus();
    }
  }, [phase, results]);

  function signOutAll() {
    signOut();
    setQuery("");
    setBox("");
    setSource("all");
    setPhase("idle");
    setView("search");
    setAuthed(false);
  }

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
        onClick={() => {
          setQuery("");
          setBox("");
          setSource("all");
          setPhase("idle");
        }}
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
          <SourceTabs active={source} onChange={changeSource} counts={counts} />
        </div>
      </header>

      <div className="mx-auto max-w-[1100px] px-4 py-5">
        <div className="flex flex-col gap-10 lg:flex-row lg:gap-12">
          <section className="min-w-0 max-w-[600px] flex-1" aria-live="polite">
            {phase === "error" ? (
              <ErrorState onRetry={() => run(query, source, true)} />
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
                    <ResultItem key={r.id} result={r} query={query} />
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
