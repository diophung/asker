import {
  Calendar,
  FileText,
  Image,
  LayoutGrid,
  Mail,
  MessageSquare,
  User,
  type LucideIcon,
} from "lucide-react";
import type { SourceFilter } from "./types";

const TABS: { id: SourceFilter; label: string; icon: LucideIcon }[] = [
  { id: "all", label: "All", icon: LayoutGrid },
  { id: "email", label: "Email", icon: Mail },
  { id: "files", label: "Files", icon: FileText },
  { id: "messages", label: "Messages", icon: MessageSquare },
  { id: "calendar", label: "Calendar", icon: Calendar },
  { id: "photos", label: "Photos", icon: Image },
  { id: "people", label: "People", icon: User },
];

export interface SourceTabsProps {
  active: SourceFilter;
  onChange: (next: SourceFilter) => void;
  /** Per-tab result counts (from the unfiltered query); shown where present. */
  counts: Partial<Record<SourceFilter, number>>;
}

/**
 * Google's "All / Images / News" row, repurposed for sources. Switching a tab
 * re-runs the search for that source (the parent does the re-query) — it does
 * not merely hide rows.
 */
export function SourceTabs({ active, onChange, counts }: SourceTabsProps) {
  return (
    <nav
      aria-label="Filter by source"
      className="-mx-2 overflow-x-auto border-b border-gline"
    >
      <ul className="flex min-w-max items-center gap-1 px-2 text-[13px]">
        {TABS.map(({ id, label, icon: Icon }) => {
          const isActive = id === active;
          const count = counts[id];
          return (
            <li key={id}>
              <button
                type="button"
                aria-current={isActive ? "page" : undefined}
                onClick={() => onChange(id)}
                className={[
                  "flex items-center gap-1.5 border-b-[3px] px-3 py-3 -mb-px transition-colors",
                  isActive
                    ? "border-gblue text-gblue"
                    : "border-transparent text-gmuted hover:text-gink",
                ].join(" ")}
              >
                <Icon aria-hidden="true" className="size-4" />
                <span>{label}</span>
                {count !== undefined && count > 0 && (
                  <span
                    className={isActive ? "text-gblue/70" : "text-gmuted/70"}
                  >
                    {count}
                  </span>
                )}
              </button>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}
