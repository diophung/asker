import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { Hit } from "../api";
import { ResultCard } from "./ResultCard";

afterEach(cleanup);

function hitWith(sourceURL: string): Hit {
  return {
    doc_id: "d1",
    connector_id: "gcal",
    type: "TYPE_EVENT",
    title: "Team sync",
    snippet: "agenda",
    score: 1,
    created: "",
    modified: "",
    metadata: {},
    source_url: sourceURL,
  } as unknown as Hit;
}

describe("ResultCard source link", () => {
  // A calendar invite / chat message from an attacker carries the link, so the
  // href value is attacker controlled. Rendering it unchecked would run script
  // in Asker's origin and expose the bearer token.
  it("does not render a javascript: source_url as a link", () => {
    const { container } = render(
      <ResultCard hit={hitWith("javascript:alert(document.cookie)")} />,
    );
    const link = container.querySelector("a.source-link");
    expect(link).toBeNull();
    expect(container.innerHTML).not.toContain("javascript:");
  });

  it("still renders a normal https source_url", () => {
    const { container } = render(
      <ResultCard hit={hitWith("https://calendar.google.com/event?eid=1")} />,
    );
    const link = container.querySelector("a.source-link");
    expect(link?.getAttribute("href")).toBe(
      "https://calendar.google.com/event?eid=1",
    );
  });
});
