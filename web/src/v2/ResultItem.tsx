import { useState, type ReactNode } from "react";
import {
  Check,
  CornerUpLeft,
  Heart,
  Info,
  MapPin,
  Paperclip,
  Send,
  ThumbsDown,
  ThumbsUp,
} from "lucide-react";
import type { SearchResult, SourceName } from "./types";
import { Avatar, fileIcon, Highlight, SourceIcon } from "./ui";

/** The personalization signal a result emits: "more like this" (positive) or
 * "fewer like this" (negative). Threaded from SearchApp -> sendFeedback. */
export type FeedbackAction = "show_more" | "show_fewer";
export type ResultFeedback = (result: SearchResult, action: FeedbackAction) => void;

/** The muted "Why this?" line + the More/Fewer feedback controls. Only the
 * email/file/message branch shows these (the text-forward results). */
function WhyAndFeedback({
  result,
  onFeedback,
}: {
  result: SearchResult;
  onFeedback?: ResultFeedback;
}) {
  const why = result.explanation?.trim();
  // Track the chosen action so a click gives immediate, visible confirmation —
  // without it the feedback fires silently (HTTP 200, no UI change) and reads as
  // "nothing happened". One choice per result; re-clicking is a no-op.
  const [acked, setAcked] = useState<FeedbackAction | null>(null);
  if (!why && !onFeedback) {
    return null;
  }
  const ack = (action: FeedbackAction) => {
    if (acked || !onFeedback) {
      return;
    }
    onFeedback(result, action);
    setAcked(action);
  };
  return (
    <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1.5">
      {why ? (
        <span className="inline-flex items-center gap-1 text-[12.5px] text-gmuted">
          <Info aria-hidden="true" className="size-3.5 shrink-0" />
          <span>
            <span className="sr-only">Why this result: </span>
            {why}
          </span>
        </span>
      ) : null}
      {onFeedback ? (
        acked ? (
          <span
            role="status"
            className="inline-flex items-center gap-1 text-[12px] text-gprov"
          >
            <Check aria-hidden="true" className="size-3.5 shrink-0" />
            {acked === "show_more"
              ? "Thanks — we'll show more like this"
              : "Thanks — we'll show fewer like this"}
          </span>
        ) : (
          <span className="inline-flex items-center gap-1.5">
            <button
              type="button"
              onClick={() => ack("show_more")}
              aria-label="More results like this"
              className="inline-flex items-center gap-1 rounded-full border border-gline px-2.5 py-1 text-[12px] text-gmuted hover:bg-gbg-soft hover:text-gblue"
            >
              <ThumbsUp aria-hidden="true" className="size-3.5" /> More like this
            </button>
            <button
              type="button"
              onClick={() => ack("show_fewer")}
              aria-label="Fewer results like this"
              className="inline-flex items-center gap-1 rounded-full border border-gline px-2.5 py-1 text-[12px] text-gmuted hover:bg-gbg-soft hover:text-[#c5221f]"
            >
              <ThumbsDown aria-hidden="true" className="size-3.5" /> Fewer like this
            </button>
          </span>
        )
      ) : null}
    </div>
  );
}

/** Shared provenance line — Google's green-URL equivalent. */
function Provenance({
  source,
  who,
  when,
}: {
  source: SourceName;
  who: string;
  when: string;
}) {
  return (
    <div className="flex items-center gap-1.5 text-[13px] leading-5 text-gmuted">
      <SourceIcon source={source} size={14} />
      <span className="font-medium text-gprov">{source}</span>
      <span aria-hidden="true">·</span>
      <span className="truncate">{who}</span>
      <span aria-hidden="true">·</span>
      <span className="shrink-0">{when}</span>
    </div>
  );
}

/** Blue link title — the primary tap target. Opens the real item when the
 * backend supplied a url; otherwise an inert in-app anchor (mock/no link). */
function TitleLink({
  id,
  url,
  children,
}: {
  id: string;
  url?: string;
  children: ReactNode;
}) {
  const cls =
    "text-[20px] leading-7 text-gtitle visited:text-gtitle-visited hover:underline";
  if (url !== undefined && url !== "") {
    return (
      <a href={url} target="_blank" rel="noopener noreferrer" className={cls}>
        {children}
      </a>
    );
  }
  return (
    <a href={`#${id}`} onClick={(e) => e.preventDefault()} className={cls}>
      {children}
    </a>
  );
}

function Snippet({ text, query }: { text: string; query: string }) {
  return (
    <p className="mt-0.5 text-[14px] leading-[1.58] text-gsnippet">
      <Highlight text={text} query={query} />
    </p>
  );
}

/** A small inline meta chip (reply count, reactions, location, …). */
function Chip({
  icon: Icon,
  children,
}: {
  icon: typeof Heart;
  children: ReactNode;
}) {
  return (
    <span className="inline-flex items-center gap-1 text-[12.5px] text-gmuted">
      <Icon aria-hidden="true" className="size-3.5" />
      {children}
    </span>
  );
}

export function ResultItem({
  result,
  query,
  onFeedback,
}: {
  result: SearchResult;
  query: string;
  onFeedback?: ResultFeedback;
}) {
  // Person — a directory entry, not a blue link (the resolved entity also gets
  // the right-rail knowledge panel).
  if (result.type === "person") {
    return (
      <article className="flex items-center gap-3">
        <Avatar name={result.name} size={44} />
        <div className="min-w-0 flex-1">
          <Provenance source={result.source} who={result.who} when={result.when} />
          <h3 className="text-[18px] leading-6">
            <TitleLink id={result.id} url={result.url}>{result.name}</TitleLink>
          </h3>
          <p className="text-[13px] text-gmuted">{result.email}</p>
        </div>
        <a
          href={`mailto:${result.email}`}
          onClick={(e) => e.preventDefault()}
          className="inline-flex shrink-0 items-center gap-1.5 rounded-full border border-gline px-3 py-1.5 text-[13px] text-gblue hover:bg-gbg-soft"
        >
          <Send className="size-3.5" /> Email
        </a>
      </article>
    );
  }

  // Photo — thumbnail on the left, text forward beside it.
  if (result.type === "photo") {
    return (
      <article className="flex gap-4">
        <div
          aria-hidden="true"
          className="h-[68px] w-[92px] shrink-0 rounded-lg"
          style={{
            background: `linear-gradient(135deg, ${result.thumb[0]}, ${result.thumb[1]})`,
          }}
        />
        <div className="min-w-0 flex-1">
          <Provenance source={result.source} who={result.who} when={result.when} />
          <h3>
            <TitleLink id={result.id} url={result.url}>{result.title}</TitleLink>
          </h3>
          <Snippet text={result.snippet} query={query} />
          {result.place && (
            <div className="mt-1">
              <Chip icon={MapPin}>{result.place}</Chip>
            </div>
          )}
        </div>
      </article>
    );
  }

  // Calendar — a date block on the left.
  if (result.type === "calendar") {
    return (
      <article className="flex gap-4">
        <div className="flex h-[60px] w-[52px] shrink-0 flex-col items-center justify-center rounded-lg border border-gline">
          <span className="text-[11px] font-semibold uppercase tracking-wide text-gmuted">
            {result.month}
          </span>
          <span className="text-[22px] font-medium leading-6 text-gink">
            {result.day}
          </span>
        </div>
        <div className="min-w-0 flex-1">
          <Provenance source={result.source} who={result.who} when={result.when} />
          <h3>
            <TitleLink id={result.id} url={result.url}>{result.title}</TitleLink>
          </h3>
          <Snippet text={result.snippet} query={query} />
          <div className="mt-1.5 flex flex-wrap items-center gap-3">
            <span className="text-[12.5px] text-gink">{result.start}</span>
            {result.location && <Chip icon={MapPin}>{result.location}</Chip>}
            <AttendeeAvatars names={result.attendees} />
          </div>
        </div>
      </article>
    );
  }

  // Email / File / Message — the text-forward "blue link" rhythm.
  const leading =
    result.type === "email" ? (
      <Avatar name={result.sender} size={36} />
    ) : result.type === "file" ? (
      <FileGlyph result={result} />
    ) : (
      <Avatar name={result.sender} size={36} />
    );

  return (
    <article className="flex gap-3">
      <div className="pt-0.5">{leading}</div>
      <div className="min-w-0 flex-1">
        <Provenance source={result.source} who={result.who} when={result.when} />
        <h3 className="flex items-center gap-2">
          <TitleLink id={result.id} url={result.url}>{result.title}</TitleLink>
          {result.type === "email" && result.hasAttachment && (
            <Paperclip
              aria-label="Has attachment"
              className="size-4 shrink-0 text-gmuted"
            />
          )}
        </h3>
        <Snippet text={result.snippet} query={query} />
        <div className="mt-1 flex flex-wrap items-center gap-3">
          {result.type === "email" && result.replyCount ? (
            <Chip icon={CornerUpLeft}>{result.replyCount} in thread</Chip>
          ) : null}
          {result.type === "file" && (
            <span className="text-[12.5px] text-gmuted">
              {result.owner} · {result.folder}
            </span>
          )}
          {result.type === "message" && result.reactions ? (
            <Chip icon={Heart}>{result.reactions}</Chip>
          ) : null}
        </div>
        <WhyAndFeedback result={result} onFeedback={onFeedback} />
      </div>
    </article>
  );
}

function FileGlyph({
  result,
}: {
  result: Extract<SearchResult, { type: "file" }>;
}) {
  const Icon = fileIcon(result.fileKind);
  return (
    <span className="inline-flex size-9 items-center justify-center rounded-lg border border-gline text-gmuted">
      <Icon aria-hidden="true" className="size-5" />
    </span>
  );
}

function AttendeeAvatars({ names }: { names: string[] }) {
  return (
    <span className="flex items-center">
      {names.slice(0, 4).map((n, i) => (
        <span
          key={n}
          className="rounded-full ring-2 ring-white"
          style={{ marginLeft: i === 0 ? 0 : -8 }}
        >
          <Avatar name={n} size={22} />
        </span>
      ))}
    </span>
  );
}
