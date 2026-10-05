import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { SearchResult } from "./types";

const state = vi.hoisted(() => ({ signedIn: true, listeners: new Set<() => void>() }));
const search = vi.hoisted(() => vi.fn());
vi.mock("./auth", async (original) => ({
  ...await original<typeof import("./auth")>(),
  isSignedIn: () => state.signedIn,
  currentUser: () => "alice",
  subscribe: (listener: () => void) => { state.listeners.add(listener); return () => state.listeners.delete(listener); },
  signIn: async () => { state.signedIn = true; state.listeners.forEach((fn) => fn()); },
}));
vi.mock("./backend", async (original) => ({
  ...await original<typeof import("./backend")>(), BACKEND_ENABLED: true,
}));
vi.mock("./data", async (original) => ({
  ...await original<typeof import("./data")>(), searchPersonalData: search,
  getRecentSearches: async () => [],
}));
import { SearchApp } from "./SearchApp";

const hit = (title: string): SearchResult => ({ id: title, title, type: "email", source: "Email",
  who: "Alice", when: "today", snippet: title, sender: "Alice", haystack: title });
beforeEach(() => {
  state.signedIn = true;
  state.listeners.clear();
  search.mockReset();
  window.history.replaceState({}, "", "/?q=original");
});
afterEach(() => cleanup());

it("ends an expired session, cancels its work, and resumes the current URL after sign-in", async () => {
  let finishOld!: (hits: SearchResult[]) => void;
  search.mockResolvedValueOnce([hit("Original result")])
    .mockImplementationOnce(() => new Promise((resolve) => { finishOld = resolve; }))
    .mockResolvedValueOnce([hit("Recovered result")]);
  render(<SearchApp />);
  await screen.findByRole("link", { name: "Original result" });
  fireEvent.change(screen.getByRole("combobox"), { target: { value: "current query" } });
  fireEvent.click(within(screen.getByRole("search")).getByRole("button", { name: "Search" }));
  const signal = search.mock.calls[1][2] as AbortSignal;
  act(() => { state.signedIn = false; state.listeners.forEach((fn) => fn()); });
  expect(signal.aborted).toBe(true);
  expect(screen.getByRole("status").textContent).toMatch(/session ended/);
  expect(screen.queryByRole("link", { name: "Original result" })).toBeNull();
  act(() => finishOld([hit("Obsolete result")]));
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await screen.findByRole("link", { name: "Recovered result" });
  expect(search.mock.calls[2][0]).toBe("current query");
  expect(screen.queryByRole("link", { name: "Obsolete result" })).toBeNull();
});
