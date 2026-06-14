// Typed client for the gateway REST search API (pinned M1 contract):
//
//   GET /v1/search?q=&types=EMAIL,CHAT_MESSAGE,...&from=<RFC3339>&to=<RFC3339>
//       &participant=&limit=20&offset=0&mode=hybrid|keyword
//   Authorization: Bearer <Keycloak JWT>
//   200 => {"hits":[...],"total":N,"degraded":"","took_ms":N,"cached":false}
//   errors => {"error":"..."}; 401 unauthenticated; 429 rate-limited.

/** asker.v1.DocType enum names accepted by the `types` query parameter. */
export type DocTypeName =
  | "EMAIL"
  | "CHAT_MESSAGE"
  | "FILE"
  | "CALENDAR_EVENT"
  | "WIKI_PAGE"
  | "TICKET"
  | "IMAGE"
  | "VIDEO"
  | "AUDIO";

export type SearchMode = "hybrid" | "keyword";

export interface SearchRequest {
  q: string;
  /** DocType enum names; sent comma-separated. Omitted when empty. */
  types?: DocTypeName[];
  /** RFC3339 timestamp lower bound (inclusive). Omitted when empty. */
  from?: string;
  /** RFC3339 timestamp upper bound (inclusive). Omitted when empty. */
  to?: string;
  /** Participant filter (email address or name). Omitted when empty. */
  participant?: string;
  limit?: number;
  offset?: number;
  mode?: SearchMode;
}

export interface Hit {
  doc_id: string;
  connector_id: string;
  type: string;
  title: string;
  /** Plain text with highlights wrapped in <hi>...</hi> tokens. */
  snippet: string;
  score: number;
  created: string;
  modified: string;
  metadata: Record<string, string>;
  // Media fields (M3): present (non-zero / non-empty) only on media hits.
  // The gateway hand-writes its JSON with snake_case tags for every field
  // (doc_id, connector_id, took_ms, ...); these media fields follow the same
  // convention. They are optional because text-document hits omit them
  // (Go zero values: 0 / "" with omitempty-style absence handled defensively).
  /** Matched-chunk start offset in milliseconds (VIDEO/AUDIO); 0 when N/A. */
  start_ms?: number;
  /** Matched-chunk end offset in milliseconds (VIDEO/AUDIO); 0 when N/A. */
  end_ms?: number;
  /** What matched: e.g. "transcript", "caption", "ocr", "visual". */
  modality?: string;
  /** Blob key of a thumbnail/poster, fetched via GET /v1/media?key=. */
  thumbnail_key?: string;
  /**
   * Browser-openable link to the original item at its source (the Gmail
   * message in Gmail, the Drive file, the Slack permalink, ...). The gateway
   * derives this from connector metadata and emits only http(s) URLs; "" or
   * absent when the source has no web URL (e.g. uploaded files). Rendered as
   * an external "View original" link.
   */
  source_url?: string;
}

export interface SearchResponse {
  hits: Hit[];
  total: number;
  /** Non-empty when the backend degraded the query (e.g. keyword-only). */
  degraded: string;
  took_ms: number;
  cached: boolean;
}

export const DEFAULT_LIMIT = 20;

/** Error from the gateway, carrying the HTTP status code. */
export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

/**
 * Encode a SearchRequest as the /v1/search query string (without the "?").
 * `types` values are joined with literal commas per the contract; every other
 * value is percent-encoded. limit/offset/mode are always present.
 */
export function buildSearchQuery(req: SearchRequest): string {
  const parts: string[] = [`q=${encodeURIComponent(req.q)}`];
  if (req.types !== undefined && req.types.length > 0) {
    parts.push(`types=${req.types.map(encodeURIComponent).join(",")}`);
  }
  if (req.from !== undefined && req.from !== "") {
    parts.push(`from=${encodeURIComponent(req.from)}`);
  }
  if (req.to !== undefined && req.to !== "") {
    parts.push(`to=${encodeURIComponent(req.to)}`);
  }
  if (req.participant !== undefined && req.participant !== "") {
    parts.push(`participant=${encodeURIComponent(req.participant)}`);
  }
  parts.push(`limit=${req.limit ?? DEFAULT_LIMIT}`);
  parts.push(`offset=${req.offset ?? 0}`);
  parts.push(`mode=${req.mode ?? "hybrid"}`);
  return parts.join("&");
}

export type FetchFn = (
  input: string,
  init: RequestInit,
) => Promise<Response>;

export interface SearchClientOptions {
  /** Gateway base URL, e.g. http://localhost:8080 (no trailing slash). */
  baseUrl: string;
  /** Returns a fresh bearer token; called before every request. */
  getToken: () => Promise<string>;
  /** Injectable for tests; defaults to global fetch. */
  fetchFn?: FetchFn;
}

/** True when the error is a cancellation of a superseded request.
 * fetch aborts reject with a DOMException, which is NOT an instanceof Error
 * in most engines, so we match on the name. */
export function isAbortError(err: unknown): boolean {
  return (
    (err instanceof DOMException || err instanceof Error) &&
    err.name === "AbortError"
  );
}

/**
 * Search client that cancels stale in-flight requests: each call to search()
 * aborts the previous one, so out-of-order responses can never clobber newer
 * results. Aborted calls reject with an AbortError (see isAbortError).
 */
export class SearchClient {
  private readonly baseUrl: string;
  private readonly getToken: () => Promise<string>;
  private readonly fetchFn: FetchFn;
  private controller: AbortController | null = null;

  constructor(opts: SearchClientOptions) {
    this.baseUrl = opts.baseUrl.replace(/\/+$/, "");
    this.getToken = opts.getToken;
    this.fetchFn =
      opts.fetchFn ?? ((input, init) => globalThis.fetch(input, init));
  }

  async search(req: SearchRequest): Promise<SearchResponse> {
    this.controller?.abort();
    const controller = new AbortController();
    this.controller = controller;

    const token = await this.getToken();
    controller.signal.throwIfAborted();

    const url = `${this.baseUrl}/v1/search?${buildSearchQuery(req)}`;
    const res = await this.fetchFn(url, {
      method: "GET",
      headers: { Authorization: `Bearer ${token}` },
      signal: controller.signal,
    });

    if (!res.ok) {
      throw new ApiError(res.status, await errorMessage(res));
    }
    return (await res.json()) as SearchResponse;
  }
}

// Connectors management REST contract (served by the gateway behind OIDC):
//
//   POST   /v1/connectors {connector_id, display_name, config:{}} -> instance
//   GET    /v1/connectors -> ConnectorInstanceStatus[]
//   DELETE /v1/connectors/{id}
//   PUT    /v1/connectors/{id}/token {token}
//   Authorization: Bearer <Keycloak JWT> on every call.

/** A configured connector instance owned by the tenant. */
export interface ConnectorInstance {
  id: string;
  connector_id: string;
  display_name: string;
  config: Record<string, unknown>;
  status: string;
  created: string;
  updated: string;
}

/** Sync progress reported alongside each instance. */
export interface ConnectorSync {
  phase: string;
  last_sync_started: string;
  last_sync_completed: string;
  last_error: string;
  docs_emitted: number;
}

/** One row of GET /v1/connectors: an instance plus its sync state. */
export interface ConnectorInstanceStatus {
  instance: ConnectorInstance;
  sync: ConnectorSync;
}

// --- wire normalization ------------------------------------------------------
// The gateway serializes the connector endpoints with protojson, which — unlike
// the rest of its (snake_case) JSON API — emits lowerCamelCase field names,
// renders int64 as a STRING, and carries the instance config as a base64
// `configJson` blob. Normalize that wire shape into the web's internal
// snake_case model in ONE place so the components/types stay simple and a missing
// field can never throw (e.g. `undefined.toLocaleString()` blanked the page).
interface RawConnectorInstance {
  id?: string;
  connectorId?: string;
  displayName?: string;
  configJson?: string;
  status?: string;
  created?: string;
  updated?: string;
}
interface RawConnectorSync {
  phase?: string;
  lastSyncStarted?: string;
  lastSyncCompleted?: string | null;
  lastError?: string;
  docsEmitted?: string | number;
}
interface RawConnectorRow {
  instance?: RawConnectorInstance;
  sync?: RawConnectorSync;
}

function decodeConfig(b64?: string): Record<string, unknown> {
  if (!b64) {
    return {};
  }
  try {
    return JSON.parse(atob(b64)) as Record<string, unknown>;
  } catch {
    return {};
  }
}

function normalizeInstance(r: RawConnectorInstance = {}): ConnectorInstance {
  return {
    id: r.id ?? "",
    connector_id: r.connectorId ?? "",
    display_name: r.displayName ?? "",
    config: decodeConfig(r.configJson),
    status: r.status ?? "",
    created: r.created ?? "",
    updated: r.updated ?? "",
  };
}

function normalizeSync(r: RawConnectorSync = {}): ConnectorSync {
  return {
    phase: r.phase ?? "",
    last_sync_started: r.lastSyncStarted ?? "",
    last_sync_completed: r.lastSyncCompleted ?? "",
    last_error: r.lastError ?? "",
    docs_emitted: Number(r.docsEmitted ?? 0) || 0,
  };
}

function normalizeRow(r: RawConnectorRow = {}): ConnectorInstanceStatus {
  return { instance: normalizeInstance(r.instance), sync: normalizeSync(r.sync) };
}

/**
 * Client for the gateway's connector-management endpoints. Each call attaches a
 * fresh bearer token and supports cancellation via an injected AbortSignal so
 * callers (e.g. polling) can drop stale in-flight requests.
 */
export class ConnectorClient {
  private readonly baseUrl: string;
  private readonly getToken: () => Promise<string>;
  private readonly fetchFn: FetchFn;

  constructor(opts: SearchClientOptions) {
    this.baseUrl = opts.baseUrl.replace(/\/+$/, "");
    this.getToken = opts.getToken;
    this.fetchFn =
      opts.fetchFn ?? ((input, init) => globalThis.fetch(input, init));
  }

  /** List the tenant's connector instances and their sync status. */
  async listConnectors(
    signal?: AbortSignal,
  ): Promise<ConnectorInstanceStatus[]> {
    const res = await this.request("/v1/connectors", { method: "GET" }, signal);
    const raw = (await res.json()) as RawConnectorRow[] | null;
    return (raw ?? []).map(normalizeRow);
  }

  /** Create a new connector instance; returns the created instance. */
  async createConnector(
    connectorId: string,
    displayName: string,
    config: Record<string, unknown>,
    signal?: AbortSignal,
  ): Promise<ConnectorInstance> {
    const res = await this.request(
      "/v1/connectors",
      {
        method: "POST",
        body: JSON.stringify({
          connector_id: connectorId,
          display_name: displayName,
          config,
        }),
      },
      signal,
    );
    return normalizeInstance((await res.json()) as RawConnectorInstance);
  }

  /** Delete a connector instance by id. */
  async deleteConnector(id: string, signal?: AbortSignal): Promise<void> {
    await this.request(
      `/v1/connectors/${encodeURIComponent(id)}`,
      { method: "DELETE" },
      signal,
    );
  }

  /**
   * Begin the OAuth "Connect with <provider>" flow for an instance. Makes an
   * AUTHED GET /v1/connectors/{id}/oauth/start (the gateway derives the tenant
   * from the bearer and stores the server-side state keyed by an unguessable
   * `state`) and returns the provider authorize URL the browser must navigate
   * to. Throws ApiError if the connector is not OAuth / not configured.
   */
  async startOAuth(id: string, signal?: AbortSignal): Promise<string> {
    const res = await this.request(
      `/v1/connectors/${encodeURIComponent(id)}/oauth/start`,
      { method: "GET" },
      signal,
    );
    const body = (await res.json()) as { authorize_url?: unknown };
    if (typeof body.authorize_url !== "string" || body.authorize_url === "") {
      throw new ApiError(res.status, "oauth start: missing authorize_url");
    }
    return body.authorize_url;
  }

  /** Store (or replace) the auth token for a connector instance. */
  async putConnectorToken(
    id: string,
    token: string,
    signal?: AbortSignal,
  ): Promise<void> {
    await this.request(
      `/v1/connectors/${encodeURIComponent(id)}/token`,
      { method: "PUT", body: JSON.stringify({ token }) },
      signal,
    );
  }

  private async request(
    path: string,
    init: { method: string; body?: string },
    signal?: AbortSignal,
  ): Promise<Response> {
    const token = await this.getToken();
    signal?.throwIfAborted();
    const headers: Record<string, string> = {
      Authorization: `Bearer ${token}`,
    };
    if (init.body !== undefined) {
      headers["Content-Type"] = "application/json";
    }
    const res = await this.fetchFn(`${this.baseUrl}${path}`, {
      method: init.method,
      headers,
      body: init.body,
      signal,
    });
    if (!res.ok) {
      throw new ApiError(res.status, await errorMessage(res));
    }
    return res;
  }
}

/**
 * Client for the gateway's authenticated media proxy.
 *
 *   GET /v1/media?key=<blobKey>   Authorization: Bearer <JWT>
 *
 * The gateway derives the tenant from the verified JWT only and streams the
 * decrypted bytes from the (internal-only) hub with the upstream Content-Type.
 * <img> elements cannot send an Authorization header, so we fetch the bytes
 * here and hand back an object URL the caller assigns to `src` — and MUST
 * revoke (URL.revokeObjectURL) once the image unmounts to avoid leaking blobs.
 */
export class MediaClient {
  private readonly baseUrl: string;
  private readonly getToken: () => Promise<string>;
  private readonly fetchFn: FetchFn;

  constructor(opts: SearchClientOptions) {
    this.baseUrl = opts.baseUrl.replace(/\/+$/, "");
    this.getToken = opts.getToken;
    this.fetchFn =
      opts.fetchFn ?? ((input, init) => globalThis.fetch(input, init));
  }

  /**
   * Fetch a thumbnail/poster blob by key and return an object URL for it.
   * Sends the bearer token; throws ApiError on a non-2xx response (e.g. 404
   * when the key is absent, 401 when unauthenticated). The returned URL must
   * be revoked by the caller when no longer needed.
   */
  async fetchThumbnail(key: string, signal?: AbortSignal): Promise<string> {
    const token = await this.getToken();
    signal?.throwIfAborted();
    const url = `${this.baseUrl}/v1/media?key=${encodeURIComponent(key)}`;
    const res = await this.fetchFn(url, {
      method: "GET",
      headers: { Authorization: `Bearer ${token}` },
      signal,
    });
    if (!res.ok) {
      throw new ApiError(res.status, await errorMessage(res));
    }
    const blob = await res.blob();
    return URL.createObjectURL(blob);
  }
}

async function errorMessage(res: Response): Promise<string> {
  let message = "";
  try {
    const body = (await res.json()) as { error?: unknown };
    if (typeof body.error === "string" && body.error !== "") {
      message = body.error;
    }
  } catch {
    // Non-JSON error body; fall through to a status-based message.
  }
  if (message !== "") {
    return message;
  }
  switch (res.status) {
    case 401:
      return "not authenticated";
    case 429:
      return "rate limited — try again shortly";
    default:
      return `search failed (HTTP ${res.status})`;
  }
}
