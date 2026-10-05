import { afterEach, describe, expect, it, vi } from "vitest";
import { searchBackend } from "./backend";
import type { SourceFilter } from "./types";

vi.mock("./auth", () => ({ getToken: vi.fn().mockResolvedValue("test-token") }));
afterEach(() => vi.unstubAllGlobals());

describe("browser search cache freshness", () => {
  it.each<SourceFilter>(["all", "email", "files", "messages", "calendar", "photos", "people"])(
    "bypasses cached search responses for the %s route, including repeated queries",
    async (source) => {
      const fetch = vi.fn().mockResolvedValue({
        ok: true, json: async () => ({ hits: [], people: [], total: 0, took_ms: 1 }),
      });
      vi.stubGlobal("fetch", fetch);
      const controller = new AbortController();
      await searchBackend("newly indexed item", source, controller.signal);
      await searchBackend("newly indexed item", source, controller.signal);
      expect(fetch).toHaveBeenCalledTimes(2);
      const path = source === "all" ? "/v1/search" : `/v1/search/${source}`;
      for (const [url, init] of fetch.mock.calls) {
        expect(String(url).split("?")[0]).toBe(path);
        expect(new URL(String(url), "http://localhost").searchParams.get("q")).toBe("newly indexed item");
        expect(init.headers).toEqual({ Authorization: "Bearer test-token", "Cache-Control": "no-cache" });
        expect(init.signal).toBe(controller.signal);
      }
    },
  );
});
