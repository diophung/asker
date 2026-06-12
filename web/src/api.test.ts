import { describe, expect, it } from "vitest";
import {
  ApiError,
  buildSearchQuery,
  DEFAULT_LIMIT,
  isAbortError,
  SearchClient,
  type FetchFn,
} from "./api";
import {
  mockErrorResponse,
  mockSearchResponse,
  searchMock,
} from "./mocks/searchMock";

const token = async () => "test-token";

describe("buildSearchQuery", () => {
  it("encodes the minimal request with defaults", () => {
    expect(buildSearchQuery({ q: "hello" })).toBe(
      `q=hello&limit=${DEFAULT_LIMIT}&offset=0&mode=hybrid`,
    );
  });

  it("encodes every parameter per the pinned shape", () => {
    const qs = buildSearchQuery({
      q: "hello world",
      types: ["EMAIL", "CHAT_MESSAGE"],
      from: "2026-01-01T00:00:00Z",
      to: "2026-01-31T23:59:59Z",
      participant: "alice@example.com",
      limit: 10,
      offset: 20,
      mode: "keyword",
    });
    expect(qs).toBe(
      "q=hello%20world" +
        "&types=EMAIL,CHAT_MESSAGE" +
        "&from=2026-01-01T00%3A00%3A00Z" +
        "&to=2026-01-31T23%3A59%3A59Z" +
        "&participant=alice%40example.com" +
        "&limit=10&offset=20&mode=keyword",
    );
  });

  it("joins types with literal commas", () => {
    const qs = buildSearchQuery({ q: "x", types: ["FILE", "IMAGE", "AUDIO"] });
    expect(qs).toContain("types=FILE,IMAGE,AUDIO");
    expect(qs).not.toContain("%2C");
  });

  it("omits empty optional parameters", () => {
    const qs = buildSearchQuery({
      q: "x",
      types: [],
      from: "",
      to: "",
      participant: "",
    });
    expect(qs).toBe("q=x&limit=20&offset=0&mode=hybrid");
  });

  it("percent-encodes reserved characters in q and participant", () => {
    const qs = buildSearchQuery({ q: "a&b=c", participant: "x,y&z" });
    expect(qs).toContain("q=a%26b%3Dc");
    expect(qs).toContain("participant=x%2Cy%26z");
  });
});

describe("SearchClient.search", () => {
  it("sends a bearer token and parses the response", async () => {
    let gotUrl = "";
    let gotAuth: string | null = null;
    const fetchFn: FetchFn = async (input, init) => {
      gotUrl = input;
      gotAuth = new Headers(init.headers).get("Authorization");
      return mockSearchResponse();
    };
    const client = new SearchClient({
      baseUrl: "http://gw:8080",
      getToken: token,
      fetchFn,
    });

    const resp = await client.search({ q: "planning", types: ["EMAIL"] });

    expect(gotUrl).toBe(
      "http://gw:8080/v1/search?q=planning&types=EMAIL&limit=20&offset=0&mode=hybrid",
    );
    expect(gotAuth).toBe("Bearer test-token");
    expect(resp).toEqual(searchMock);
    expect(resp.hits).toHaveLength(2);
    expect(resp.total).toBe(42);
  });

  it("aborts an in-flight request when a new one starts", async () => {
    const signals: AbortSignal[] = [];
    const fetchFn: FetchFn = (_input, init) => {
      const signal = init.signal;
      if (!(signal instanceof AbortSignal)) {
        throw new Error("missing abort signal");
      }
      signals.push(signal);
      if (signals.length === 1) {
        // First request hangs until aborted.
        return new Promise((_resolve, reject) => {
          signal.addEventListener("abort", () =>
            reject(new DOMException("aborted", "AbortError")),
          );
        });
      }
      return Promise.resolve(mockSearchResponse());
    };
    const client = new SearchClient({
      baseUrl: "http://gw:8080",
      getToken: token,
      fetchFn,
    });

    // Catch immediately so the expected rejection is never unhandled.
    const first = client.search({ q: "stale" }).catch((e: unknown) => e);
    // Let the first request reach its fetch before superseding it.
    await new Promise((r) => setTimeout(r, 0));
    expect(signals).toHaveLength(1);

    const second = client.search({ q: "fresh" });

    await expect(second).resolves.toEqual(searchMock);
    expect(isAbortError(await first)).toBe(true);
    expect(signals[0].aborted).toBe(true);
    expect(signals[1].aborted).toBe(false);
  });

  it("rejects with an abort error when superseded before its fetch starts", async () => {
    let fetchCalls = 0;
    const fetchFn: FetchFn = () => {
      fetchCalls++;
      return Promise.resolve(mockSearchResponse());
    };
    const client = new SearchClient({
      baseUrl: "http://gw:8080",
      getToken: token,
      fetchFn,
    });

    // Issue both synchronously: the first is aborted while still awaiting the
    // token, so its fetch must never be sent.
    const first = client.search({ q: "stale" }).catch((e: unknown) => e);
    const second = client.search({ q: "fresh" });

    await expect(second).resolves.toEqual(searchMock);
    expect(isAbortError(await first)).toBe(true);
    expect(fetchCalls).toBe(1);
  });

  it("throws ApiError with the gateway error message", async () => {
    const client = new SearchClient({
      baseUrl: "http://gw:8080",
      getToken: token,
      fetchFn: async () => mockErrorResponse(429, "rate limit exceeded"),
    });
    const err = await client.search({ q: "x" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(429);
    expect((err as ApiError).message).toBe("rate limit exceeded");
  });

  it("falls back to a status message on non-JSON error bodies", async () => {
    const client = new SearchClient({
      baseUrl: "http://gw:8080",
      getToken: token,
      fetchFn: async () => new Response("boom", { status: 401 }),
    });
    const err = await client.search({ q: "x" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(401);
    expect((err as ApiError).message).toBe("not authenticated");
  });

  it("strips trailing slashes from the base URL", async () => {
    let gotUrl = "";
    const client = new SearchClient({
      baseUrl: "http://gw:8080/",
      getToken: token,
      fetchFn: async (input) => {
        gotUrl = input;
        return mockSearchResponse();
      },
    });
    await client.search({ q: "x" });
    expect(gotUrl.startsWith("http://gw:8080/v1/search?")).toBe(true);
  });
});
