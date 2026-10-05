import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { SearchFallback } from "./states";
afterEach(cleanup);
describe("search fallback disclosure", () => {
  it("explains keyword fallback", () => {
    render(<SearchFallback reasons="keyword-only,rerank-unavailable" />);
    expect(screen.getByRole("status").textContent).toContain("Showing keyword matches");
  });
  it("discloses unsupported filters before users rely on results", () => {
    render(<SearchFallback reasons="unsupported-filter-syntax" />);
    expect(screen.getByRole("status").textContent).toContain("filter wasn’t recognized");
  });
});
