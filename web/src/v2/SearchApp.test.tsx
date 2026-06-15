import {
  cleanup,
  fireEvent,
  render,
  screen,
  within,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { SearchApp } from "./SearchApp";

afterEach(cleanup);

// The app reflects the submitted query into the URL (history.pushState), which
// persists across tests in jsdom; reset to home so each test starts fresh.
beforeEach(() => {
  window.history.replaceState({}, "", "/");
});

function typeAndSearch(query: string) {
  fireEvent.change(screen.getByRole("combobox"), { target: { value: query } });
  // The home page also has a "Search" CTA button outside the box; scope to the
  // search landmark so this clicks the in-box Search button unambiguously.
  fireEvent.click(
    within(screen.getByRole("search")).getByRole("button", { name: "Search" }),
  );
}

describe("SearchApp", () => {
  it("home shows the box + tagline, then glides to provenance-rich results", async () => {
    render(<SearchApp />);

    // Home state.
    expect(
      screen.getByText(/Everything you.?ve ever saved, in one search\./i),
    ).toBeTruthy();
    expect(screen.getByRole("combobox")).toBeTruthy();

    typeAndSearch("sarah chen");

    // Results stream in: the privacy meta line + isolation signature.
    expect(await screen.findByText(/results from your data/i)).toBeTruthy();
    expect(await screen.findByText(/only you can see these/i)).toBeTruthy();

    // Heterogeneous, provenance-attributed results name the person.
    expect((await screen.findAllByText(/Sarah Chen/i)).length).toBeGreaterThan(0);

    // The knowledge panel resolves the person from the user's own graph.
    expect(await screen.findByText(/Shared documents/i)).toBeTruthy();
  });

  it("source tabs are full-page links to per-source endpoints", async () => {
    render(<SearchApp />);
    typeAndSearch("q3");
    await screen.findByText(/results from your data/i);

    // Tabs are real links (a full page load per tab, each its own endpoint),
    // not client-side filters: the Files tab points at /search/files?q=q3.
    const filesTab = screen.getByRole("link", { name: /Files/ });
    expect(filesTab.getAttribute("href")).toBe("/search/files?q=q3");

    const peopleTab = screen.getByRole("link", { name: /People/ });
    expect(peopleTab.getAttribute("href")).toBe("/search/people?q=q3");
  });

  it("no-match offers corpus-aware suggestions, not a dead end", async () => {
    render(<SearchApp />);
    typeAndSearch("zzzznotathing");

    expect(
      await screen.findByText(/Nothing matched/i),
    ).toBeTruthy();
    // A clickable suggestion recovers the flow.
    expect(screen.getByRole("button", { name: "Q3 planning" })).toBeTruthy();
  });
});
