import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { searchBackend } from "./backend";
import { ResultItem } from "./ResultItem";

vi.mock("./auth", () => ({ getToken: vi.fn().mockResolvedValue("test-token") }));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function wireHit(connector: string, type: string, index: number) {
  return {
    doc_id: `result-${index}`, connector_id: connector, type,
    title: `Result ${index}`, snippet: "Synthetic search result", score: 1,
    created: "2026-10-01T10:00:00Z", modified: "2026-10-01T10:00:00Z",
    metadata: {}, source_url: "", start_ms: 0, end_ms: 0,
    modality: "", thumbnail_key: "", explanation: "",
  };
}

function respondWith(hits: ReturnType<typeof wireHit>[]) {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
    ok: true, json: async () => ({ hits, total: hits.length, took_ms: 1 }),
  }));
}

describe("search result source provenance", () => {
  it("labels unknown connectors by document category without assigning a brand logo", async () => {
    const cases = [
      ["EMAIL", "Email"], ["FILE", "Files"], ["WIKI_PAGE", "Files"],
      ["TICKET", "Files"], ["CHAT_MESSAGE", "Messages"],
      ["CALENDAR_EVENT", "Events"], ["IMAGE", "Media"], ["VIDEO", "Media"],
      ["AUDIO", "Media"], ["FUTURE_TYPE", "Source"],
    ];
    respondWith(cases.map(([type], index) => wireHit(`eval-local-${type}`, type, index)));
    const results = await searchBackend("synthetic", "all");
    expect(results.map((result) => result.source)).toEqual(cases.map(([, source]) => source));

    const { container } = render(<>{results.map((result) => (
      <ResultItem key={result.id} result={result} query="" />
    ))}</>);
    for (const result of results) {
      const article = screen.getByRole("link", { name: result.title }).closest("article")!;
      expect(within(article).getByText(result.source)).toBeTruthy();
    }
    expect(container.querySelector("img")).toBeNull();
    expect(screen.queryByText("Drive")).toBeNull();
  });

  it("preserves the existing known connector source mappings", async () => {
    const cases = [
      ["gmail", "Gmail"], ["outlook-mail", "Gmail"], ["gdrive", "Drive"],
      ["s3", "Drive"], ["jira", "Drive"], ["confluence", "Drive"],
      ["slack", "Slack"], ["msteams", "Slack"], ["whatsapp-export", "Slack"],
      ["imessage-agent", "Slack"], ["gcal", "Calendar"],
      ["outlook-cal", "Calendar"], ["ical", "Calendar"], ["upload", "Photos"],
      ["contacts", "Contacts"],
    ];
    respondWith(cases.map(([connector], index) => wireHit(connector, "EMAIL", index)));
    const results = await searchBackend("synthetic", "all");
    expect(results.map((result) => result.source)).toEqual(cases.map(([, source]) => source));
  });

  it("treats connector ids that match Object prototype keys as unknown connectors", async () => {
    respondWith([wireHit("constructor", "EMAIL", 0), wireHit("__proto__", "FILE", 1)]);
    const results = await searchBackend("synthetic", "all");
    expect(results.map((result) => result.source)).toEqual(["Email", "Files"]);
  });

  it("retains URL safety when mapping unknown connectors and provider metadata", async () => {
    respondWith([
      { ...wireHit("eval-local-calendar", "CALENDAR_EVENT", 0),
        source_url: "javascript:alert(1)",
        metadata: { html_link: "data:text/html,unsafe", web_link: "https://calendar.example/event" } },
      { ...wireHit("eval-local-chat", "CHAT_MESSAGE", 1),
        metadata: { permalink: "javascript:alert(1)" } },
    ]);
    const results = await searchBackend("synthetic", "all");
    expect(results.map((result) => result.url)).toEqual(["https://calendar.example/event", ""]);
  });
});
