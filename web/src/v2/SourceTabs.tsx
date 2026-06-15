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
import { searchUrl } from "./router";

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
  /** The current query — each tab links to its own endpoint for this query. */
  query: string;
}

/**
 * Google's "All / Images / News" row, repurposed for sources. Each tab is a
 * real link (an <a href>) to that source's own URL/endpoint, so switching tabs
 * is a FULL PAGE LOAD served by a distinct backend endpoint — not a client-side
 * filter. cmd/middle-click opens a tab in a new browser tab, as expected of
 * real links.
 *
 * The row scrolls horizontally on very narrow screens but hides the scrollbar
 * (.no-scrollbar) so there is no stray scrollbar under the tabs on desktop.
 */
export function SourceTabs({ active, query }: SourceTabsProps) {
  return (
    <nav
      aria-label="Filter by source"
      className="no-scrollbar -mx-2 overflow-x-auto border-b border-gline"
    >
      <ul className="flex min-w-max items-center gap-1 px-2 text-[13px]">
        {TABS.map(({ id, label, icon: Icon }) => {
          const isActive = id === active;
          return (
            <li key={id}>
              <a
                href={searchUrl(id, query)}
                aria-current={isActive ? "page" : undefined}
                className={[
                  "flex items-center gap-1.5 border-b-[3px] px-3 py-3 -mb-px transition-colors",
                  isActive
                    ? "border-gblue text-gblue"
                    : "border-transparent text-gmuted hover:text-gink",
                ].join(" ")}
              >
                <Icon aria-hidden="true" className="size-4" />
                <span>{label}</span>
              </a>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}
