import { describe, expect, it } from "vitest";
import { ApiError, ConnectorClient, type FetchFn } from "./api";
import {
  connectorsMock,
  createdInstance,
  jsonResponse,
} from "./mocks/connectorsMock";

const token = async () => "test-token";

interface Captured {
  url: string;
  method: string;
  auth: string | null;
  contentType: string | null;
  body: string | undefined;
}

function recorder(response: () => Response): {
  fetchFn: FetchFn;
  calls: Captured[];
} {
  const calls: Captured[] = [];
  const fetchFn: FetchFn = async (input, init) => {
    const headers = new Headers(init.headers);
    calls.push({
      url: input,
      method: init.method ?? "GET",
      auth: headers.get("Authorization"),
      contentType: headers.get("Content-Type"),
      body: typeof init.body === "string" ? init.body : undefined,
    });
    return response();
  };
  return { fetchFn, calls };
}

describe("ConnectorClient.listConnectors", () => {
  it("GETs /v1/connectors with a bearer token and parses rows", async () => {
    const { fetchFn, calls } = recorder(() => jsonResponse(connectorsMock));
    const client = new ConnectorClient({
      baseUrl: "http://gw:8080/",
      getToken: token,
      fetchFn,
    });

    const rows = await client.listConnectors();

    expect(calls[0].url).toBe("http://gw:8080/v1/connectors");
    expect(calls[0].method).toBe("GET");
    expect(calls[0].auth).toBe("Bearer test-token");
    expect(rows).toEqual(connectorsMock);
  });
});

describe("ConnectorClient.createConnector", () => {
  it("POSTs the {connector_id, display_name, config} shape", async () => {
    const { fetchFn, calls } = recorder(() => jsonResponse(createdInstance));
    const client = new ConnectorClient({
      baseUrl: "http://gw:8080",
      getToken: token,
      fetchFn,
    });

    const inst = await client.createConnector("gmail", "Work Gmail", {
      user_email: "alice@example.com",
    });

    expect(calls[0].url).toBe("http://gw:8080/v1/connectors");
    expect(calls[0].method).toBe("POST");
    expect(calls[0].contentType).toBe("application/json");
    expect(JSON.parse(calls[0].body ?? "")).toEqual({
      connector_id: "gmail",
      display_name: "Work Gmail",
      config: { user_email: "alice@example.com" },
    });
    expect(inst).toEqual(createdInstance);
  });
});

describe("ConnectorClient.putConnectorToken", () => {
  it("PUTs {token} to the instance token endpoint, id-encoded", async () => {
    const { fetchFn, calls } = recorder(() => new Response(null, { status: 204 }));
    const client = new ConnectorClient({
      baseUrl: "http://gw:8080",
      getToken: token,
      fetchFn,
    });

    await client.putConnectorToken("inst/1", "secret-token");

    expect(calls[0].url).toBe("http://gw:8080/v1/connectors/inst%2F1/token");
    expect(calls[0].method).toBe("PUT");
    expect(JSON.parse(calls[0].body ?? "")).toEqual({ token: "secret-token" });
  });
});

describe("ConnectorClient.deleteConnector", () => {
  it("DELETEs the instance by id", async () => {
    const { fetchFn, calls } = recorder(() => new Response(null, { status: 204 }));
    const client = new ConnectorClient({
      baseUrl: "http://gw:8080",
      getToken: token,
      fetchFn,
    });

    await client.deleteConnector("inst-1");

    expect(calls[0].url).toBe("http://gw:8080/v1/connectors/inst-1");
    expect(calls[0].method).toBe("DELETE");
  });
});

describe("ConnectorClient errors", () => {
  it("throws ApiError carrying the gateway message", async () => {
    const client = new ConnectorClient({
      baseUrl: "http://gw:8080",
      getToken: token,
      fetchFn: async () => jsonResponse({ error: "bad config" }, 400),
    });
    const err = await client.listConnectors().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(400);
    expect((err as ApiError).message).toBe("bad config");
  });
});
