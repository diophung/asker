import { describe, expect, it } from "vitest";
import { buildSearchQuery } from "../api";
import {
  defaultFilters,
  DOC_TYPE_OPTIONS,
  toSearchRequest,
  typeLabel,
  type Filters,
} from "./filters";

describe("toSearchRequest", () => {
  it("maps default filters to a minimal request", () => {
    const req = toSearchRequest("hello", defaultFilters, 0);
    expect(req).toEqual({ q: "hello", limit: 20, offset: 0, mode: "hybrid" });
  });

  it("widens date inputs to full-day RFC3339 bounds in UTC", () => {
    const f: Filters = { ...defaultFilters, from: "2026-01-01", to: "2026-01-31" };
    const req = toSearchRequest("x", f, 0);
    expect(req.from).toBe("2026-01-01T00:00:00Z");
    expect(req.to).toBe("2026-01-31T23:59:59Z");
  });

  it("carries types, trimmed participant, mode, and offset", () => {
    const f: Filters = {
      types: ["EMAIL", "TICKET"],
      from: "",
      to: "",
      participant: "  alice@example.com  ",
      mode: "keyword",
    };
    const req = toSearchRequest("budget", f, 40);
    expect(req).toEqual({
      q: "budget",
      types: ["EMAIL", "TICKET"],
      participant: "alice@example.com",
      limit: 20,
      offset: 40,
      mode: "keyword",
    });
  });

  it("omits blank fields entirely", () => {
    const f: Filters = { ...defaultFilters, participant: "   " };
    const req = toSearchRequest("x", f, 0);
    expect(req).not.toHaveProperty("types");
    expect(req).not.toHaveProperty("from");
    expect(req).not.toHaveProperty("to");
    expect(req).not.toHaveProperty("participant");
  });

  it("copies the types array (no aliasing of filter state)", () => {
    const f: Filters = { ...defaultFilters, types: ["EMAIL"] };
    const req = toSearchRequest("x", f, 0);
    expect(req.types).toEqual(["EMAIL"]);
    expect(req.types).not.toBe(f.types);
  });

  it("round-trips into the pinned query-string shape", () => {
    const f: Filters = {
      types: ["EMAIL", "CHAT_MESSAGE"],
      from: "2026-01-01",
      to: "2026-02-01",
      participant: "bob",
      mode: "hybrid",
    };
    const qs = buildSearchQuery(toSearchRequest("status update", f, 20));
    expect(qs).toBe(
      "q=status%20update" +
        "&types=EMAIL,CHAT_MESSAGE" +
        "&from=2026-01-01T00%3A00%3A00Z" +
        "&to=2026-02-01T23%3A59%3A59Z" +
        "&participant=bob" +
        "&limit=20&offset=20&mode=hybrid",
    );
  });
});

describe("DOC_TYPE_OPTIONS", () => {
  it("covers the nine filterable DocType enum names", () => {
    expect(DOC_TYPE_OPTIONS.map((o) => o.value)).toEqual([
      "EMAIL",
      "CHAT_MESSAGE",
      "FILE",
      "CALENDAR_EVENT",
      "WIKI_PAGE",
      "TICKET",
      "IMAGE",
      "VIDEO",
      "AUDIO",
    ]);
  });

  it("labels enum names for display and passes through unknowns", () => {
    expect(typeLabel("CHAT_MESSAGE")).toBe("Chat");
    expect(typeLabel("CALENDAR_EVENT")).toBe("Calendar");
    expect(typeLabel("SOMETHING_NEW")).toBe("SOMETHING_NEW");
  });
});
