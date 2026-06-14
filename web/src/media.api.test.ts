import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, MediaClient, type FetchFn } from "./api";

const token = async () => "test-token";

function blobResponse(body: string, contentType: string): Response {
  return new Response(body, {
    status: 200,
    headers: { "Content-Type": contentType },
  });
}

function errorResponse(status: number, error: string): Response {
  return new Response(JSON.stringify({ error }), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => vi.restoreAllMocks());

describe("MediaClient.fetchThumbnail", () => {
  it("sends the bearer token, key-encodes the URL, and returns an object URL", async () => {
    const created = vi
      .spyOn(URL, "createObjectURL")
      .mockReturnValue("blob:created-url");

    let gotUrl = "";
    let gotAuth: string | null = null;
    const fetchFn: FetchFn = async (input, init) => {
      gotUrl = input;
      gotAuth = new Headers(init.headers).get("Authorization");
      return blobResponse("PNGDATA", "image/png");
    };
    const client = new MediaClient({
      baseUrl: "http://gw:8080",
      getToken: token,
      fetchFn,
    });

    const url = await client.fetchThumbnail("tenants/t1/thumbs/a b.jpg");

    expect(gotUrl).toBe(
      "http://gw:8080/v1/media?key=tenants%2Ft1%2Fthumbs%2Fa%20b.jpg",
    );
    expect(gotAuth).toBe("Bearer test-token");
    expect(created).toHaveBeenCalledTimes(1);
    expect(url).toBe("blob:created-url");
  });

  it("throws ApiError with the gateway message on a non-2xx response", async () => {
    const client = new MediaClient({
      baseUrl: "http://gw:8080",
      getToken: token,
      fetchFn: async () => errorResponse(404, "media not found"),
    });
    const err = await client
      .fetchThumbnail("missing")
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(404);
    expect((err as ApiError).message).toBe("media not found");
  });

  it("does not fetch when the signal is already aborted", async () => {
    const controller = new AbortController();
    controller.abort();
    let fetched = false;
    const client = new MediaClient({
      baseUrl: "http://gw:8080",
      getToken: token,
      fetchFn: async () => {
        fetched = true;
        return blobResponse("x", "image/png");
      },
    });
    await expect(
      client.fetchThumbnail("k", controller.signal),
    ).rejects.toThrow();
    expect(fetched).toBe(false);
  });
});
