// Safe rendering of snippet highlight tokens.
//
// The gateway wraps query highlights in <hi>...</hi>. We NEVER use
// dangerouslySetInnerHTML: the snippet is split on the literal tokens and
// emitted as React text nodes (auto-escaped) with <mark> elements around the
// highlighted runs. Any other markup in the snippet renders as literal text.
import type { ReactNode } from "react";

const HI_TOKEN = /<hi>(.*?)<\/hi>/gs;

/**
 * Split a snippet on <hi>...</hi> tokens into React nodes. Even segments are
 * plain text; captured segments become <mark>. Stray/unbalanced tags are left
 * as visible literal text — never interpreted as HTML.
 */
export function renderSnippet(snippet: string): ReactNode[] {
  const parts = snippet.split(HI_TOKEN);
  return parts.map((part, i) => {
    if (i % 2 === 1) {
      return <mark key={i}>{part}</mark>;
    }
    return part;
  });
}

export function Snippet({ text }: { text: string }) {
  return <p className="snippet">{renderSnippet(text)}</p>;
}
