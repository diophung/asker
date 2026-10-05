import { act, cleanup, fireEvent, render, screen, within, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { SearchResult } from "./types";
import { StrictMode } from "react";

const search = vi.hoisted(() => vi.fn());
const metadata = vi.hoisted(() => vi.fn());
vi.mock("./data", async (original) => ({
  ...await original<typeof import("./data")>(), searchPersonalData: search, metaFor: metadata,
}));
import { SearchApp } from "./SearchApp";

beforeEach(() => {
  window.history.replaceState({}, "", "/");
  search.mockReset();
  metadata.mockReset().mockReturnValue({ approx: "1", seconds: "0.02" });
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

function submit(text: string) {
  fireEvent.change(screen.getByRole("combobox"), { target: { value: text } });
  fireEvent.click(within(screen.getByRole("search")).getByRole("button", { name: "Search" }));
}

function hit(title: string): SearchResult {
  return { id: title, type: "email", title, source: "Gmail", who: "Test sender", when: "today",
    snippet: title, sender: "Test sender", haystack: title };
}

describe("search cancellation", () => {
  it("aborts stalled work at the browser deadline and permits a fresh retry", async () => {
    vi.useFakeTimers();
    let finish!: (hits: SearchResult[]) => void;
    search.mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }))
      .mockResolvedValueOnce([hit("Retry result")]);
    render(<SearchApp />);
    submit("stalled request");
    const signal = search.mock.calls[0][2] as AbortSignal;
    await act(async () => { vi.advanceTimersByTime(5_000); });
    expect(signal.aborted).toBe(true);
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    await act(async () => { finish([hit("Late result")]); });
    expect(screen.queryByRole("link", { name: "Late result" })).toBeNull();
    vi.useRealTimers();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await screen.findByRole("link", { name: "Retry result" });
  });
  it("exposes the rendered timing sample in the DOM and clears it for a new request", async () => {
    const frame = vi.spyOn(window, "requestAnimationFrame").mockReturnValue(1);
    search.mockResolvedValueOnce([hit("Timed match")]);
    search.mockImplementationOnce(() => new Promise(() => {}));
    const onRendered = vi.fn();
    window.addEventListener("asker:search-rendered", onRendered);
    try {
      const { container } = render(<SearchApp />);
      submit("budget");
      await screen.findByRole("link", { name: "Timed match" });
      expect(container.querySelector("[data-search-elapsed-ms]")).toBeNull();
      act(() => frame.mock.calls.at(-1)![0](performance.now()));
      const rendered = container.querySelector("[data-search-elapsed-ms]")!;
      const event = onRendered.mock.calls[0][0] as CustomEvent;
      const elapsed = Number(rendered.getAttribute("data-search-elapsed-ms"));
      expect(Number.isFinite(elapsed)).toBe(true);
      expect(elapsed).toBeGreaterThanOrEqual(0);
      expect(elapsed).toBe(event.detail.elapsedMs);
      expect(event.detail.resultCount).toBe(1);
      submit("another query");
      expect(container.querySelector("[data-search-elapsed-ms]")).toBeNull();
      // Even a frame delivered after cancellation cannot mark the new request
      // complete using the previous request's measurement.
      act(() => frame.mock.calls.at(-1)![0](performance.now()));
      expect(container.querySelector("[data-search-elapsed-ms]")).toBeNull();
      expect(onRendered).toHaveBeenCalledTimes(1);
    } finally {
      window.removeEventListener("asker:search-rendered", onRendered);
    }
  });

  it("exposes completion timing for an empty result set", async () => {
    const frame = vi.spyOn(window, "requestAnimationFrame").mockReturnValue(1);
    search.mockResolvedValueOnce([]);
    const { container } = render(<SearchApp />);
    submit("unmatched request");
    await screen.findByText(/Nothing matched/);
    act(() => frame.mock.calls.at(-1)![0](performance.now()));
    const rendered = container.querySelector("[data-search-elapsed-ms]")!;
    expect(rendered).not.toBeNull();
    expect(Number(rendered.getAttribute("data-search-elapsed-ms"))).toBeGreaterThanOrEqual(0);
  });

  it("restarts a bookmarked query after StrictMode mount cleanup", async () => {
    window.history.replaceState({}, "", "/?q=budget");
    search.mockImplementation((_query, _source, signal: AbortSignal) => new Promise((resolve, reject) => {
      signal.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")));
      queueMicrotask(() => { if (!signal.aborted) resolve([hit("Bookmarked match")]); });
    }));
    render(<StrictMode><SearchApp /></StrictMode>);
    await screen.findByRole("link", { name: "Bookmarked match" });
    expect(search).toHaveBeenCalledTimes(2);
    expect((search.mock.calls[0][2] as AbortSignal).aborted).toBe(true);
    expect((search.mock.calls[1][2] as AbortSignal).aborted).toBe(false);
  });

  it("refreshes timing and fallback disclosure when the same query returns the same hit count", async () => {
    const hits = [hit("Stable match")];
    search.mockResolvedValue(hits);
    render(<SearchApp />);
    submit("budget");
    await screen.findByRole("link", { name: "Stable match" });
    expect(screen.queryByText(/Semantic search is unavailable/)).toBeNull();
    metadata.mockReturnValue({ approx: "1", seconds: "0.15", degraded: "keyword-only" });
    submit("budget");
    await screen.findByText(/Semantic search is unavailable/);
    expect(screen.getByText(/0.15 seconds/)).toBeTruthy();
    metadata.mockReturnValue({ approx: "1", seconds: "0.03" });
    submit("budget");
    await waitFor(() => expect(screen.queryByText(/Semantic search is unavailable/)).toBeNull());
    expect(screen.getByText(/0.03 seconds/)).toBeTruthy();
  });

  it("aborts superseded work and does not render its late results", async () => {
    let oldResolve!: (hits: SearchResult[]) => void;
    search.mockImplementationOnce(() => new Promise<SearchResult[]>((resolve) => { oldResolve = resolve; }));
    search.mockResolvedValueOnce([hit("Current match")]);
    render(<SearchApp />);
    submit("first request");
    const oldSignal = search.mock.calls[0][2] as AbortSignal;
    submit("second request");
    expect(oldSignal.aborted).toBe(true);
    await screen.findByRole("link", { name: "Current match" });
    oldResolve([hit("Obsolete match")]);
    await waitFor(() => expect(screen.queryByRole("link", { name: "Obsolete match" })).toBeNull());
  });

  it("going home cancels pending work so late results cannot replace home", async () => {
    let resolve!: (hits: SearchResult[]) => void;
    search.mockImplementationOnce(() => new Promise<SearchResult[]>((r) => { resolve = r; }));
    render(<SearchApp />);
    submit("pending request");
    const signal = search.mock.calls[0][2] as AbortSignal;
    fireEvent.click(screen.getByRole("button", { name: "Asker home" }));
    expect(signal.aborted).toBe(true);
    resolve([hit("Late match")]);
    expect(await screen.findByText(/Everything you.?ve ever saved/i)).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("link", { name: "Late match" })).toBeNull());
  });
});
