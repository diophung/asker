import type { DocTypeName, SearchMode } from "../api";
import { DOC_TYPE_OPTIONS, type Filters } from "../search/filters";

export interface FilterSidebarProps {
  filters: Filters;
  onChange: (f: Filters) => void;
}

export function FilterSidebar({ filters, onChange }: FilterSidebarProps) {
  function toggleType(t: DocTypeName, checked: boolean) {
    const types = checked
      ? [...filters.types, t]
      : filters.types.filter((v) => v !== t);
    onChange({ ...filters, types });
  }

  return (
    <aside className="sidebar" aria-label="Search filters">
      <fieldset className="filter-group">
        <legend>Type</legend>
        {DOC_TYPE_OPTIONS.map((opt) => (
          <label key={opt.value} className="filter-check">
            <input
              type="checkbox"
              checked={filters.types.includes(opt.value)}
              onChange={(e) => toggleType(opt.value, e.target.checked)}
            />
            {opt.label}
          </label>
        ))}
      </fieldset>

      <fieldset className="filter-group">
        <legend>Date</legend>
        <label className="filter-field">
          From
          <input
            type="date"
            value={filters.from}
            max={filters.to !== "" ? filters.to : undefined}
            onChange={(e) => onChange({ ...filters, from: e.target.value })}
          />
        </label>
        <label className="filter-field">
          To
          <input
            type="date"
            value={filters.to}
            min={filters.from !== "" ? filters.from : undefined}
            onChange={(e) => onChange({ ...filters, to: e.target.value })}
          />
        </label>
      </fieldset>

      <fieldset className="filter-group">
        <legend>Participant</legend>
        <label className="filter-field">
          <span className="visually-hidden">Participant</span>
          <input
            type="text"
            placeholder="alice@example.com"
            value={filters.participant}
            onChange={(e) =>
              onChange({ ...filters, participant: e.target.value })
            }
          />
        </label>
      </fieldset>

      <fieldset className="filter-group">
        <legend>Mode</legend>
        <label className="filter-field">
          <span className="visually-hidden">Search mode</span>
          <select
            value={filters.mode}
            onChange={(e) =>
              onChange({ ...filters, mode: e.target.value as SearchMode })
            }
          >
            <option value="hybrid">Hybrid</option>
            <option value="keyword">Keyword</option>
          </select>
        </label>
      </fieldset>
    </aside>
  );
}
