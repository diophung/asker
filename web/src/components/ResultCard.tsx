import type { Hit } from "../api";
import { typeLabel } from "../search/filters";
import { Snippet } from "./Snippet";

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

export function ResultCard({ hit }: { hit: Hit }) {
  const date = formatDate(hit.modified !== "" ? hit.modified : hit.created);
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
      <Snippet text={hit.snippet} />
      <footer className="result-meta">
        {date !== "" && <time className="result-date">{date}</time>}
      </footer>
    </article>
  );
}
