import { useEffect, useState } from "react";
import {
  DEFAULT_LIMIT,
  isAbortError,
  type SearchClient,
  type SearchResponse,
} from "./api";
import { defaultFilters, type Filters, toSearchRequest } from "./search/filters";
import { FilterSidebar } from "./components/FilterSidebar";
import { Pagination } from "./components/Pagination";
import { ResultCard } from "./components/ResultCard";
import { SearchBar } from "./components/SearchBar";
import {
  EmptyState,
  ErrorState,
  IdleState,
  LoadingSkeleton,
} from "./components/States";

type Status = "idle" | "loading" | "success" | "error";

// Wrapped in an object so resubmitting the same text still re-triggers the
// search effect (object identity changes on every submit).
interface Submitted {
  q: string;
}

/** Debounce between a state change and the request, batching rapid filter
 * clicks / typing. Stale in-flight requests are aborted by SearchClient. */
const SEARCH_DEBOUNCE_MS = 250;

export function SearchPage({ client }: { client: SearchClient }) {
  const [filters, setFilters] = useState<Filters>(defaultFilters);
  const [submitted, setSubmitted] = useState<Submitted | null>(null);
  const [offset, setOffset] = useState(0);
  const [status, setStatus] = useState<Status>("idle");
  const [result, setResult] = useState<SearchResponse | null>(null);
  const [errorMsg, setErrorMsg] = useState("");

  useEffect(() => {
    if (submitted === null) {
      return;
    }
    const timer = setTimeout(() => {
      setStatus("loading");
      client
        .search(toSearchRequest(submitted.q, filters, offset))
        .then((resp) => {
          setResult(resp);
          setStatus("success");
        })
        .catch((err: unknown) => {
          if (isAbortError(err)) {
            return; // superseded by a newer search
          }
          setErrorMsg(err instanceof Error ? err.message : String(err));
          setStatus("error");
        });
    }, SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [client, submitted, filters, offset]);

  function handleSubmit(q: string) {
    setOffset(0);
    setSubmitted({ q });
  }

  function handleFiltersChange(f: Filters) {
    setFilters(f);
    setOffset(0);
  }

  function handleRetry() {
    if (submitted !== null) {
      setSubmitted({ q: submitted.q }); // new identity re-runs the effect
    }
  }

  return (
    <div className="search-page">
      <div className="search-bar-row">
        <SearchBar onSubmit={handleSubmit} />
      </div>
      <div className="search-body">
        <FilterSidebar filters={filters} onChange={handleFiltersChange} />
        <main className="results-pane">
          {status === "success" && result !== null && (
            <>
              {result.degraded !== "" && (
                <div className="degraded-banner" role="status">
                  Results may be incomplete: {result.degraded}
                </div>
              )}
              <div className="results-summary">
                <span>
                  {result.total.toLocaleString()}{" "}
                  {result.total === 1 ? "result" : "results"} ·{" "}
                  {result.took_ms} ms
                </span>
                {result.cached && <span className="cached-pill">cached</span>}
              </div>
              {result.hits.length === 0 ? (
                <EmptyState />
              ) : (
                <div className="results">
                  {result.hits.map((hit) => (
                    <ResultCard key={hit.doc_id} hit={hit} />
                  ))}
                </div>
              )}
              <Pagination
                offset={offset}
                limit={DEFAULT_LIMIT}
                total={result.total}
                onOffsetChange={setOffset}
              />
            </>
          )}
          {status === "loading" && <LoadingSkeleton />}
          {status === "error" && (
            <ErrorState message={errorMsg} onRetry={handleRetry} />
          )}
          {status === "idle" && <IdleState />}
        </main>
      </div>
    </div>
  );
}
