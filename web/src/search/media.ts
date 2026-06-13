// Presentation helpers for media (IMAGE/VIDEO/AUDIO) search hits.

/** DocType enum names that carry media payloads. */
export function isMediaType(type: string): boolean {
  return type === "IMAGE" || type === "VIDEO" || type === "AUDIO";
}

/** Timed media (a matched chunk can carry a start/end offset). */
export function isTimedMediaType(type: string): boolean {
  return type === "VIDEO" || type === "AUDIO";
}

/**
 * Format a millisecond offset as a clock timestamp:
 *   < 1 hour  -> M:SS    (e.g. 0:05, 12:34)
 *   >= 1 hour -> H:MM:SS (e.g. 1:02:03)
 * Negative or NaN inputs clamp to 0:00. Sub-second parts are floored.
 */
export function formatTimestamp(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) {
    ms = 0;
  }
  const totalSeconds = Math.floor(ms / 1000);
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  const ss = seconds.toString().padStart(2, "0");
  if (hours > 0) {
    const mm = minutes.toString().padStart(2, "0");
    return `${hours}:${mm}:${ss}`;
  }
  return `${minutes}:${ss}`;
}

/**
 * Human label for the chunk modality reported by the query service. Unknown
 * values fall through to the raw string (already plain text, rendered safely).
 */
export function modalityLabel(modality: string): string {
  switch (modality) {
    case "transcript":
      return "transcript";
    case "caption":
      return "caption";
    case "ocr":
      return "OCR";
    case "visual":
      return "visual";
    default:
      return modality;
  }
}
