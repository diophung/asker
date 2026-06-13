// Format an RFC3339 timestamp as a short relative phrase ("3m ago"), used by
// the connectors view for last_sync_completed. Returns "" for blank/invalid
// input so callers can omit the field entirely.

const UNITS: ReadonlyArray<{ secs: number; label: string }> = [
  { secs: 86400, label: "d" },
  { secs: 3600, label: "h" },
  { secs: 60, label: "m" },
  { secs: 1, label: "s" },
];

/** Relative time from `value` to `now` (default: Date.now()), e.g. "5m ago". */
export function relativeTime(value: string, now: number = Date.now()): string {
  if (value === "") {
    return "";
  }
  const t = new Date(value).getTime();
  if (Number.isNaN(t)) {
    return "";
  }
  const deltaSecs = Math.round((now - t) / 1000);
  if (deltaSecs < 5) {
    return "just now";
  }
  for (const unit of UNITS) {
    if (deltaSecs >= unit.secs) {
      return `${Math.floor(deltaSecs / unit.secs)}${unit.label} ago`;
    }
  }
  return "just now";
}
