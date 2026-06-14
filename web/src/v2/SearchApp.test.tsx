import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { SearchApp } from "./SearchApp";

afterEach(cleanup);

function typeAndSearch(query: string) {
  fireEvent.change(screen.getByRole("combobox"), { target: { value: query } });
  fireEvent.click(screen.getByRole("button", { name: "Search" }));
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

  it("switching the source tab re-runs the search for that source", async () => {
    render(<SearchApp />);
    typeAndSearch("q3");
    await screen.findByText(/results from your data/i);

    fireEvent.click(screen.getByRole("button", { name: /Files/ }));

    // A file result for the new source is rendered after the re-run.
    expect(await screen.findByText("Q3 Planning Doc")).toBeTruthy();
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
