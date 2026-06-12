import { type FormEvent, useState } from "react";

export interface SearchBarProps {
  onSubmit: (q: string) => void;
  disabled?: boolean;
}

export function SearchBar({ onSubmit, disabled }: SearchBarProps) {
  const [value, setValue] = useState("");

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const q = value.trim();
    if (q !== "") {
      onSubmit(q);
    }
  }

  return (
    <form className="search-bar" onSubmit={handleSubmit} role="search">
      <input
        type="search"
        className="search-input"
        placeholder="Search your everything…"
        aria-label="Search query"
        value={value}
        onChange={(e) => setValue(e.target.value)}
        disabled={disabled}
        autoFocus
      />
      <button type="submit" className="search-button" disabled={disabled}>
        Search
      </button>
    </form>
  );
}
