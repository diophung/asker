// Loading / error / empty / pre-search presentational states.

export function LoadingSkeleton() {
  return (
    <div className="results" aria-busy="true" aria-label="Loading results">
      {[0, 1, 2, 3, 4].map((i) => (
        <div key={i} className="result-card skeleton-card">
          <div className="skeleton skeleton-title" />
          <div className="skeleton skeleton-line" />
          <div className="skeleton skeleton-line short" />
        </div>
      ))}
    </div>
  );
}

export function ErrorState({
  message,
  onRetry,
}: {
  message: string;
  onRetry: () => void;
}) {
  return (
    <div className="state-box state-error" role="alert">
      <p className="state-title">Search failed</p>
      <p className="state-detail">{message}</p>
      <button type="button" onClick={onRetry}>
        Retry
      </button>
    </div>
  );
}

export function EmptyState() {
  return (
    <div className="state-box">
      <p className="state-title">No results</p>
      <p className="state-detail">
        Try different keywords or remove some filters.
      </p>
    </div>
  );
}

export function IdleState() {
  return (
    <div className="state-box state-idle">
      <p className="state-detail">
        Search across your email, chats, files, and more.
      </p>
    </div>
  );
}
