export interface PaginationProps {
  offset: number;
  limit: number;
  total: number;
  onOffsetChange: (offset: number) => void;
}

export function Pagination({
  offset,
  limit,
  total,
  onOffsetChange,
}: PaginationProps) {
  const page = Math.floor(offset / limit) + 1;
  const pages = Math.max(1, Math.ceil(total / limit));
  const hasPrev = offset > 0;
  const hasNext = offset + limit < total;
  if (!hasPrev && !hasNext) {
    return null;
  }
  return (
    <nav className="pagination" aria-label="Result pages">
      <button
        type="button"
        disabled={!hasPrev}
        onClick={() => onOffsetChange(Math.max(0, offset - limit))}
      >
        ← Previous
      </button>
      <span className="pagination-status">
        Page {page} of {pages}
      </span>
      <button
        type="button"
        disabled={!hasNext}
        onClick={() => onOffsetChange(offset + limit)}
      >
        Next →
      </button>
    </nav>
  );
}
