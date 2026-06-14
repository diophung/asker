// Small presentational primitives shared across result types. No data concerns.

import {
  Calendar as CalendarIcon,
  File,
  FileText,
  Image as ImageIcon,
  Mail,
  MessageSquare,
  Presentation,
  Sheet,
  User,
  type LucideIcon,
} from "lucide-react";
import type { FileKind, ResultType, SourceName } from "./types";

/**
 * Source brand colors — the ONLY place brand color appears, and only as a small
 * badge dot typing each result (never a background or large fill).
 */
const SOURCE_COLOR: Record<SourceName, string> = {
  Gmail: "#ea4335",
  Drive: "#1ea362",
  Slack: "#4a154b",
  Calendar: "#4285f4",
  Photos: "#fbbc04",
  Contacts: "#9334e6",
};

export function SourceDot({ source }: { source: SourceName }) {
  return (
    <span
      aria-hidden="true"
      className="inline-block size-2 shrink-0 rounded-full align-middle"
      style={{ background: SOURCE_COLOR[source] }}
    />
  );
}

const TYPE_ICON: Record<ResultType, LucideIcon> = {
  email: Mail,
  file: FileText,
  message: MessageSquare,
  calendar: CalendarIcon,
  photo: ImageIcon,
  person: User,
};

export function typeIcon(type: ResultType): LucideIcon {
  return TYPE_ICON[type];
}

const FILE_ICON: Record<FileKind, LucideIcon> = {
  pdf: FileText,
  doc: FileText,
  sheet: Sheet,
  slides: Presentation,
  image: ImageIcon,
  other: File,
};

export function fileIcon(kind: FileKind): LucideIcon {
  return FILE_ICON[kind];
}

// --- Initials avatar (no backend images; deterministic color per name). -------

const AVATAR_PALETTE = [
  "#1a73e8",
  "#ea4335",
  "#1ea362",
  "#9334e6",
  "#f29900",
  "#d93025",
  "#12b5cb",
  "#4a154b",
];

function initials(name: string): string {
  const parts = name.trim().split(/\s+/);
  const first = parts[0]?.[0] ?? "";
  const last = parts.length > 1 ? (parts[parts.length - 1][0] ?? "") : "";
  return (first + last).toUpperCase() || "?";
}

function colorFor(name: string): string {
  let h = 0;
  for (let i = 0; i < name.length; i++) {
    h = (h * 31 + name.charCodeAt(i)) | 0;
  }
  return AVATAR_PALETTE[Math.abs(h) % AVATAR_PALETTE.length];
}

export function Avatar({
  name,
  size = 32,
}: {
  name: string;
  size?: number;
}) {
  return (
    <span
      aria-hidden="true"
      className="inline-flex shrink-0 items-center justify-center rounded-full font-medium text-white"
      style={{
        width: size,
        height: size,
        background: colorFor(name),
        fontSize: Math.round(size * 0.4),
      }}
    >
      {initials(name)}
    </span>
  );
}

// --- Bold the query terms inside a snippet (Google's snippet emphasis). --------

export function Highlight({ text, query }: { text: string; query: string }) {
  const terms = query
    .trim()
    .toLowerCase()
    .split(/\s+/)
    .filter((t) => t.length >= 2);
  if (terms.length === 0) {
    return <>{text}</>;
  }
  // Split on any term, case-insensitively, keeping the delimiters.
  const escaped = terms.map((t) => t.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
  const re = new RegExp(`(${escaped.join("|")})`, "ig");
  const parts = text.split(re);
  return (
    <>
      {parts.map((part, i) =>
        terms.includes(part.toLowerCase()) ? (
          <strong key={i} className="font-bold text-gink">
            {part}
          </strong>
        ) : (
          <span key={i}>{part}</span>
        ),
      )}
    </>
  );
}
