import { Lock, RotateCw } from "lucide-react";

/**
 * The privacy signature — the one bold move. Where Google signals the AUTHORITY
 * of public pages, Asker signals ISOLATION: this is yours, and only yours.
 */
export function MetaLine({
  approx,
  seconds,
}: {
  approx: string;
  seconds: string;
}) {
  return (
    <p className="flex flex-wrap items-center gap-x-1.5 text-[13px] text-gmuted">
      <span>
        About {approx} results from your data ({seconds} seconds)
      </span>
      <span aria-hidden="true">·</span>
      <span className="inline-flex items-center gap-1 text-gprov">
        <Lock className="size-3" />
        only you can see these
      </span>
    </p>
  );
}

/** Google-style skeleton rows that match the result layout (not a spinner). */
export function LoadingSkeleton() {
  return (
    <div className="space-y-7" aria-busy="true" aria-label="Searching your data">
      {[0, 1, 2, 3, 4].map((i) => (
        <div key={i} className="flex gap-3">
          <div className="v2-shimmer size-9 shrink-0 rounded-lg" />
          <div className="min-w-0 flex-1 space-y-2">
            <div className="v2-shimmer h-3 w-44 rounded" />
            <div className="v2-shimmer h-4 w-3/5 rounded" />
            <div className="v2-shimmer h-3 w-full rounded" />
            <div className="v2-shimmer h-3 w-4/5 rounded" />
          </div>
        </div>
      ))}
    </div>
  );
}

export function NoResults({
  query,
  suggestions,
  onSuggest,
}: {
  query: string;
  suggestions: string[];
  onSuggest: (q: string) => void;
}) {
  return (
    <div className="max-w-prose">
      <p className="text-[16px] text-gink">
        Nothing matched <em className="not-italic font-medium">{query}</em> in
        your data. Try a name, a date, or fewer words.
      </p>
      <p className="mt-3 text-[13px] text-gmuted">Try one of these:</p>
      <div className="mt-2 flex flex-wrap gap-2">
        {suggestions.map((s) => (
          <button
            key={s}
            type="button"
            onClick={() => onSuggest(s)}
            className="rounded-full border border-gline px-3 py-1.5 text-[13px] text-gink hover:bg-gbg-soft"
          >
            {s}
          </button>
        ))}
      </div>
    </div>
  );
}

export function ErrorState({ onRetry }: { onRetry: () => void }) {
  return (
    <div className="max-w-prose">
      <p className="text-[16px] text-gink">
        Couldn&rsquo;t reach your data just now.
      </p>
      <button
        type="button"
        onClick={onRetry}
        className="mt-3 inline-flex items-center gap-2 rounded-full bg-gblue px-4 py-2 text-[14px] font-medium text-white hover:brightness-95"
      >
        <RotateCw className="size-4" /> Retry
      </button>
    </div>
  );
}
