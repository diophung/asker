// Test-only fixtures matching the gateway's connector-management responses.
// Import only from *.test.* files — there is no runtime mock switch.
import type {
  ConnectorInstance,
  ConnectorInstanceStatus,
} from "../api";

export const gmailInstance: ConnectorInstanceStatus = {
  instance: {
    id: "inst-gmail-1",
    connector_id: "gmail",
    display_name: "Work Gmail",
    config: { user_email: "alice@example.com" },
    status: "active",
    created: "2026-06-01T08:00:00Z",
    updated: "2026-06-12T09:00:00Z",
  },
  sync: {
    phase: "incremental",
    last_sync_started: "2026-06-12T09:00:00Z",
    last_sync_completed: "2026-06-12T09:01:30Z",
    last_error: "",
    docs_emitted: 1284,
  },
};

export const jiraInstance: ConnectorInstanceStatus = {
  instance: {
    id: "inst-jira-1",
    connector_id: "jira",
    display_name: "Eng Jira",
    config: { base_url: "https://acme.atlassian.net" },
    status: "error",
    created: "2026-06-02T08:00:00Z",
    updated: "2026-06-12T08:30:00Z",
  },
  sync: {
    phase: "full",
    last_sync_started: "2026-06-12T08:25:00Z",
    last_sync_completed: "",
    last_error: "token expired — re-authenticate",
    docs_emitted: 0,
  },
};

export const connectorsMock: ConnectorInstanceStatus[] = [
  gmailInstance,
  jiraInstance,
];

export const createdInstance: ConnectorInstance = {
  id: "inst-new-1",
  connector_id: "gmail",
  display_name: "Work Gmail",
  config: { user_email: "alice@example.com" },
  status: "pending",
  created: "2026-06-12T10:00:00Z",
  updated: "2026-06-12T10:00:00Z",
};

// The RAW gateway wire shape for the connector endpoints (protojson:
// lowerCamelCase field names, int64 rendered as a STRING, config as a base64
// `configJson` blob, lastSyncCompleted null when never completed). The api
// client normalizes this into the snake_case fixtures above; the API tests feed
// the wire form and assert the normalized result — so they verify against what
// the gateway actually returns, not a hand-matched mock.
function b64(o: unknown): string {
  return btoa(JSON.stringify(o));
}

export const connectorsWire: unknown[] = [
  {
    instance: {
      id: "inst-gmail-1",
      connectorId: "gmail",
      displayName: "Work Gmail",
      configJson: b64({ user_email: "alice@example.com" }),
      status: "active",
      created: "2026-06-01T08:00:00Z",
      updated: "2026-06-12T09:00:00Z",
    },
    sync: {
      phase: "incremental",
      lastSyncStarted: "2026-06-12T09:00:00Z",
      lastSyncCompleted: "2026-06-12T09:01:30Z",
      lastError: "",
      docsEmitted: "1284",
    },
  },
  {
    instance: {
      id: "inst-jira-1",
      connectorId: "jira",
      displayName: "Eng Jira",
      configJson: b64({ base_url: "https://acme.atlassian.net" }),
      status: "error",
      created: "2026-06-02T08:00:00Z",
      updated: "2026-06-12T08:30:00Z",
    },
    sync: {
      phase: "full",
      lastSyncStarted: "2026-06-12T08:25:00Z",
      lastSyncCompleted: null,
      lastError: "token expired — re-authenticate",
      docsEmitted: "0",
    },
  },
];

export const createdInstanceWire: unknown = {
  id: "inst-new-1",
  connectorId: "gmail",
  displayName: "Work Gmail",
  configJson: b64({ user_email: "alice@example.com" }),
  status: "pending",
  created: "2026-06-12T10:00:00Z",
  updated: "2026-06-12T10:00:00Z",
};

/** Build a JSON Response with the given body and status. */
export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}
