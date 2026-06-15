import {
  type FormEvent,
  type KeyboardEvent,
  useId,
  useRef,
  useState,
} from "react";
import { Clock, FileText, Mic, Search, X } from "lucide-react";
import type { Suggestion } from "./types";
import { Avatar } from "./ui";
import { useVoiceInput } from "./useVoiceInput";

export interface SearchBoxProps {
  value: string;
  onChange: (next: string) => void;
  /** Submit the current query (Enter, the Search button, or a picked row). */
  onSubmit: (query: string) => void;
  /** Typed autocomplete rows for the current value. */
  suggestions: Suggestion[];
  /** Remove a recent-search row (the ✕); omitted in mock mode. */
  onRemoveRecent?: (text: string) => void;
  autoFocus?: boolean;
  variant: "home" | "header";
}

/**
 * The search box (the hero). Rounded pill, search/mic/clear affordances, and an
 * autocomplete dropdown that mixes recent searches, people, and documents from
 * the corpus — the first signal the engine knows YOUR world. Arrow-key
 * navigable; Enter selects the active row or submits the typed query.
 */
export function SearchBox({
  value,
  onChange,
  onSubmit,
  suggestions,
  onRemoveRecent,
  autoFocus,
  variant,
}: SearchBoxProps) {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1); // -1 = the typed query itself
  const inputRef = useRef<HTMLInputElement>(null);
  const listId = useId();

  const showList = open && suggestions.length > 0;

  function submit(query: string) {
    const q = query.trim();
    if (q === "") {
      return;
    }
    setOpen(false);
    setActive(-1);
    inputRef.current?.blur();
    onSubmit(q);
  }

  function handleSubmit(e: FormEvent) {
    e.preventDefault();
    submit(value);
  }

  function pick(s: Suggestion) {
    onChange(s.text);
    submit(s.text);
  }

  // Voice input (Web Speech API): dictated words stream into the box as you
  // speak; the search runs automatically when you stop talking.
  const voice = useVoiceInput({
    onTranscript: (text) => {
      onChange(text);
      setActive(-1);
      setOpen(true);
    },
    onFinal: (text) => {
      submit(text);
    },
  });

  function handleKeyDown(e: KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Escape") {
      setOpen(false);
      setActive(-1);
      return;
    }
    if (!showList) {
      return;
    }
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((a) => (a + 1) % suggestions.length);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => (a <= 0 ? suggestions.length - 1 : a - 1));
    } else if (e.key === "Enter" && active >= 0) {
      e.preventDefault();
      pick(suggestions[active]);
    }
  }

  return (
    <div className="relative w-full">
      <form
        role="search"
        onSubmit={handleSubmit}
        className={[
          "flex w-full items-center gap-3 rounded-full bg-white px-4",
          "border border-gline transition-shadow duration-150",
          "hover:shadow-[0_1px_6px_rgba(32,33,36,0.18)]",
          "focus-within:border-transparent focus-within:shadow-[0_1px_8px_rgba(32,33,36,0.28)]",
          variant === "home" ? "h-12" : "h-11",
        ].join(" ")}
      >
        <button
          type="submit"
          aria-label="Search"
          onMouseDown={(e) => e.preventDefault()}
          className="shrink-0 rounded-full p-0.5 text-gmuted hover:text-gblue"
        >
          <Search aria-hidden="true" className="size-5" />
        </button>
        <input
          ref={inputRef}
          type="text"
          autoFocus={autoFocus}
          value={value}
          role="combobox"
          aria-expanded={showList}
          aria-controls={listId}
          aria-autocomplete="list"
          aria-activedescendant={active >= 0 ? `${listId}-opt-${active}` : undefined}
          aria-label="Search your data"
          autoComplete="off"
          spellCheck={false}
          placeholder="Search your email, files, and messages"
          onChange={(e) => {
            onChange(e.target.value);
            setActive(-1);
            setOpen(true);
          }}
          onFocus={() => setOpen(true)}
          onBlur={() => window.setTimeout(() => setOpen(false), 120)}
          onKeyDown={handleKeyDown}
          className="min-w-0 flex-1 bg-transparent text-[16px] text-gink outline-none placeholder:text-gmuted"
        />
        {value !== "" && (
          <button
            type="button"
            aria-label="Clear search"
            onMouseDown={(e) => e.preventDefault()}
            onClick={() => {
              onChange("");
              inputRef.current?.focus();
              setOpen(true);
            }}
            className="shrink-0 rounded-full p-1 text-gmuted hover:bg-gbg-soft"
          >
            <X className="size-4" />
          </button>
        )}
        {voice.supported && (
          <button
            type="button"
            aria-label={voice.listening ? "Stop voice input" : "Search by voice"}
            aria-pressed={voice.listening}
            title={
              voice.error !== ""
                ? voice.error
                : voice.listening
                  ? "Listening…"
                  : "Search by voice"
            }
            onMouseDown={(e) => e.preventDefault()}
            onClick={voice.toggle}
            className={[
              "shrink-0 rounded-full p-1 transition-colors",
              voice.listening
                ? "animate-pulse bg-gblue/10 text-[#c5221f]"
                : "text-gblue/90 hover:bg-gbg-soft",
            ].join(" ")}
          >
            <Mic className="size-5" />
          </button>
        )}
      </form>

      {showList && (
        <ul
          id={listId}
          role="listbox"
          aria-label="Suggestions"
          className="absolute left-0 right-0 top-[calc(100%+6px)] z-20 overflow-hidden rounded-2xl border border-gline bg-white py-2 shadow-[0_4px_18px_rgba(32,33,36,0.22)]"
        >
          {suggestions.map((s, i) => (
            <li
              id={`${listId}-opt-${i}`}
              key={`${s.kind}-${s.text}`}
              role="option"
              aria-selected={i === active}
              onMouseDown={(e) => e.preventDefault()}
              onMouseEnter={() => setActive(i)}
              onClick={() => pick(s)}
              className={[
                "flex cursor-pointer items-center gap-3 px-4 py-2",
                i === active ? "bg-gbg-soft" : "",
              ].join(" ")}
            >
              <SuggestionIcon suggestion={s} />
              <span className="min-w-0 flex-1">
                <span className="block truncate text-[15px] text-gink">
                  {s.text}
                </span>
                {s.kind !== "recent" && (
                  <span className="block truncate text-[13px] text-gmuted">
                    {s.sub}
                  </span>
                )}
              </span>
              {s.kind === "recent" &&
                (onRemoveRecent ? (
                  <button
                    type="button"
                    aria-label={`Remove ${s.text} from recent searches`}
                    onMouseDown={(e) => e.preventDefault()}
                    onClick={(e) => {
                      e.stopPropagation();
                      onRemoveRecent(s.text);
                    }}
                    className="shrink-0 rounded p-1 text-[13px] text-gmuted hover:bg-white hover:text-gink"
                  >
                    Remove
                  </button>
                ) : (
                  <span className="shrink-0 text-[13px] text-gmuted">Recent</span>
                ))}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function SuggestionIcon({ suggestion }: { suggestion: Suggestion }) {
  if (suggestion.kind === "person") {
    return <Avatar name={suggestion.text} size={26} />;
  }
  const Icon = suggestion.kind === "recent" ? Clock : FileText;
  return <Icon aria-hidden="true" className="size-5 shrink-0 text-gmuted" />;
}
