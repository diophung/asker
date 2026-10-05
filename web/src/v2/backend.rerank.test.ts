import { afterEach, describe, expect, it, vi } from "vitest";
import type { SearchMode } from "./backend";
import type { SourceFilter } from "./types";

const auth = vi.hoisted(() => ({
  getToken: vi.fn().mockResolvedValue("test-token"),
  invalidateToken: vi.fn(),
}));
vi.mock("./auth", () => auth);

afterEach(() => {
  vi.unstubAllGlobals();
  vi.unstubAllEnvs();
  vi.resetModules();
  vi.clearAllMocks();
});

async function backend(flag = "0") {
  vi.stubEnv("VITE_RERANK_DEFAULT", flag);
  vi.resetModules();
  return import("./backend");
}

const hit = {
  doc_id: "synthetic-result", connector_id: "gmail", type: "EMAIL",
  title: "Synthetic result", snippet: "Synthetic preview", score: 1,
  created: "2026-10-01T10:00:00Z", modified: "2026-10-01T10:00:00Z",
  metadata: {}, source_url: "", start_ms: 0, end_ms: 0,
  modality: "", thumbnail_key: "", explanation: "",
};

function response(overrides: Record<string, unknown> = {}) {
  const fetch = vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({ hits: [hit], people: [], total: 1, took_ms: 1, ...overrides }),
  });
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

describe("browser rerank build flag", () => {
  it.each(["", "0", "true", "2"])("stays off unless the value is exactly 1 (%s)", async (flag) => {
    const { searchBackend } = await backend(flag);
    const fetch = response();
    await searchBackend("synthetic", "all");
    expect(new URL(fetch.mock.calls[0][0], "http://localhost").searchParams.has("rerank")).toBe(false);
  });

  it.each<SourceFilter>(["all", "email", "files", "messages", "calendar", "photos"])(
    "requests reranking only for enabled hybrid document searches (%s)", async (source) => {
      const { searchBackend } = await backend("1");
      const fetch = response({ rerank_requested: true, rerank_applied: true });
      const controller = new AbortController();
      await searchBackend("newly indexed item", source, controller.signal);
      const [url, init] = fetch.mock.calls[0];
      const params = new URL(url, "http://localhost").searchParams;
      expect(params.get("rerank")).toBe("1");
      expect(params.get("mode")).toBe("hybrid");
      expect(params.get("q")).toBe("newly indexed item");
      expect(params.get("limit")).toBe("20");
      expect(params.get("offset")).toBe("0");
      expect(init.headers).toEqual({ Authorization: "Bearer test-token", "Cache-Control": "no-cache" });
      expect(init.signal).toBe(controller.signal);
    },
  );

  it.each<SearchMode>(["keyword", "vector"])("leaves %s requests unchanged", async (mode) => {
    const { searchBackend, setSearchMode } = await backend("1");
    setSearchMode(mode);
    const fetch = response();
    await searchBackend("synthetic", "all");
    const params = new URL(fetch.mock.calls[0][0], "http://localhost").searchParams;
    expect(params.get("mode")).toBe(mode);
    expect(params.has("rerank")).toBe(false);
  });

  it("leaves derived people searches unchanged", async () => {
    const { searchBackend } = await backend("1");
    const fetch = response({ total: 0 });
    await searchBackend("synthetic", "people");
    const url = new URL(fetch.mock.calls[0][0], "http://localhost");
    expect(url.pathname).toBe("/v1/search/people");
    expect(url.searchParams.has("rerank")).toBe(false);
    expect(url.searchParams.has("mode")).toBe(false);
  });
});

describe("rerank response provenance", () => {
  it.each([false, undefined])("marks nonempty requested results when applied is %s", async (applied) => {
    const { searchBackend, backendMeta } = await backend("1");
    response({ rerank_requested: true, rerank_applied: applied, degraded: "" });
    expect(await searchBackend("synthetic", "all")).toHaveLength(1);
    expect(backendMeta("synthetic")?.degraded).toBe("rerank-unavailable");
  });

  it("retains existing degraded provenance", async () => {
    const { searchBackend, backendMeta } = await backend("1");
    response({ rerank_requested: true, rerank_applied: false, degraded: "embedding-unavailable" });
    await searchBackend("synthetic", "all");
    expect(backendMeta("synthetic")?.degraded).toBe("embedding-unavailable");
  });

  it.each(["", undefined])("keeps a clean no-match response clean (%s)", async (degraded) => {
    const { searchBackend, backendMeta } = await backend("1");
    response({ hits: [], total: 0, rerank_requested: true, rerank_applied: false, degraded });
    expect(await searchBackend("synthetic", "all")).toEqual([]);
    expect(backendMeta("synthetic")?.degraded).toBe(degraded);
  });

  it.each([
    { rerank_requested: false, rerank_applied: false },
    { rerank_requested: true, rerank_applied: true },
    {},
  ])("preserves clean results when no rerank failure is reported (%j)", async (provenance) => {
    const { searchBackend, backendMeta } = await backend("1");
    response({ ...provenance, degraded: "" });
    await searchBackend("synthetic", "all");
    expect(backendMeta("synthetic")?.degraded).toBe("");
  });

  it("keeps token invalidation on unauthorized responses", async () => {
    const { searchBackend } = await backend("1");
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 401 }));
    await expect(searchBackend("synthetic", "all")).rejects.toThrow("HTTP 401");
    expect(auth.invalidateToken).toHaveBeenCalledWith("test-token");
  });

  it("does not publish rerank metadata after the caller aborts", async () => {
    const { searchBackend, backendMeta } = await backend("1");
    const controller = new AbortController();
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
      ok: true,
      json: async () => {
        controller.abort();
        return { hits: [hit], total: 1, took_ms: 1, rerank_requested: true, rerank_applied: false };
      },
    }));
    await expect(searchBackend("synthetic", "all", controller.signal)).rejects.toThrow();
    expect(backendMeta("synthetic")).toBeNull();
  });
});
