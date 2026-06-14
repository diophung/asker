import { describe, expect, it } from "vitest";
import { relativeTime } from "./relativeTime";

const now = Date.parse("2026-06-12T12:00:00Z");

describe("relativeTime", () => {
  it("returns empty string for blank or invalid input", () => {
    expect(relativeTime("", now)).toBe("");
    expect(relativeTime("not-a-date", now)).toBe("");
  });

  it("renders just now within a few seconds", () => {
    expect(relativeTime("2026-06-12T11:59:58Z", now)).toBe("just now");
  });

  it("renders minutes, hours, and days ago", () => {
    expect(relativeTime("2026-06-12T11:55:00Z", now)).toBe("5m ago");
    expect(relativeTime("2026-06-12T09:00:00Z", now)).toBe("3h ago");
    expect(relativeTime("2026-06-10T12:00:00Z", now)).toBe("2d ago");
  });
});
