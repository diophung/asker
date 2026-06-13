import type { Hit } from "../api";
import { typeLabel } from "../search/filters";
import {
  formatTimestamp,
  isMediaType,
  isTimedMediaType,
  modalityLabel,
} from "../search/media";
import { Snippet } from "./Snippet";
import { Thumbnail, type FetchThumbnail } from "./Thumbnail";

function formatDate(value: string): string {
  if (value === "") {
    return "";
  }
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) {
    return "";
  }
  return d.toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

/** Modality pill ("transcript" / "caption" / "OCR" / ...) for a media match. */
function ModalityBadge({ modality }: { modality: string }) {
  if (modality === "") {
    return null;
  }
  return (
    <span className="badge badge-modality">{modalityLabel(modality)}</span>
  );
}

/**
 * Timestamp deep-link for a timed media chunk. A real seeking player is out of
 * scope (future work); we render the formatted offset as a visible, accessible
 * control labelled "Jump to MM:SS". It targets the doc's media fragment
 * (#t=<seconds>) so the intent is preserved when a player is wired up.
 */
function TimestampLink({ startMs }: { startMs: number }) {
  const label = formatTimestamp(startMs);
  const seconds = Math.floor(Math.max(0, startMs) / 1000);
  return (
    <a className="media-jump" href={`#t=${seconds}`} aria-label={`Jump to ${label}`}>
      <span aria-hidden="true">▶</span> Jump to {label}
    </a>
  );
}

export function ResultCard({
  hit,
  fetchThumbnail,
}: {
  hit: Hit;
  fetchThumbnail?: FetchThumbnail;
}) {
  const date = formatDate(hit.modified !== "" ? hit.modified : hit.created);
  const media = isMediaType(hit.type);
  const thumbnailKey = hit.thumbnail_key ?? "";
  const modality = hit.modality ?? "";
  const startMs = hit.start_ms ?? 0;
  const showThumb =
    media && thumbnailKey !== "" && fetchThumbnail !== undefined;

  return (
    <article className="result-card">
      <header className="result-header">
        <h3 className="result-title">
          {hit.title !== "" ? hit.title : "(untitled)"}
        </h3>
        <div className="result-badges">
          <span className="badge badge-connector">{hit.connector_id}</span>
          <span className="badge badge-type">{typeLabel(hit.type)}</span>
        </div>
      </header>
      <div className={media ? "result-media" : undefined}>
        {showThumb && (
          <Thumbnail
            thumbnailKey={thumbnailKey}
            alt={hit.title !== "" ? hit.title : "media preview"}
            fetchThumbnail={fetchThumbnail}
          />
        )}
        <div className="result-body">
          <Snippet text={hit.snippet} />
          {isTimedMediaType(hit.type) && startMs > 0 && (
            <div className="media-controls">
              <TimestampLink startMs={startMs} />
              <ModalityBadge modality={modality} />
            </div>
          )}
          {hit.type === "IMAGE" && modality !== "" && (
            <div className="media-controls">
              <ModalityBadge modality={modality} />
            </div>
          )}
        </div>
      </div>
      <footer className="result-meta">
        {date !== "" && <time className="result-date">{date}</time>}
      </footer>
    </article>
  );
}
