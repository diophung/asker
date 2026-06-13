// Static catalog of connector types the UI can offer to "Connect".
//
// The gateway's connector registry is the source of truth at runtime, but the
// management UI needs human labels, grouping, and per-connector config-field
// hints to render a useful Connect form. These mirror each connector's config
// schema (see connectors/<id>/<id>.go) and are intentionally hardcoded: the
// dev gateway does not expose a discovery endpoint yet (hub work).

/** One config field the Connect form should render for a connector type. */
export interface ConnectorField {
  /** Config object key sent in createConnector(...config). */
  key: string;
  label: string;
  placeholder?: string;
  /** Marks the field required client-side (the gateway re-validates). */
  required?: boolean;
}

/** A connector type that can be instantiated from the catalog. */
export interface ConnectorType {
  /** Registry id sent as connector_id. */
  id: string;
  displayName: string;
  category: string;
  /** Config fields for the Connect form (may be empty). */
  fields: ConnectorField[];
  /**
   * True when the connector authenticates with an AuthToken/OAuth2 credential.
   * For dev, the form shows a "paste a token" field that is stored via
   * putConnectorToken after the instance is created.
   */
  needsToken: boolean;
}

const baseUrl = (placeholder: string): ConnectorField => ({
  key: "base_url",
  label: "Base URL",
  placeholder,
});

/**
 * Known connector types, grouped by category in display order. Field hints
 * track the Go connectors' config schemas; base_url is optional everywhere it
 * appears (it overrides the API endpoint for dev/CI fakes).
 */
export const CONNECTOR_CATALOG: ReadonlyArray<ConnectorType> = [
  {
    id: "gmail",
    displayName: "Gmail",
    category: "Email",
    needsToken: true,
    fields: [
      {
        key: "user_email",
        label: "User email",
        placeholder: "you@example.com",
        required: true,
      },
      baseUrl("https://gmail.googleapis.com"),
    ],
  },
  {
    id: "outlook-mail",
    displayName: "Outlook Mail",
    category: "Email",
    needsToken: true,
    fields: [
      {
        key: "user_principal_name",
        label: "User principal name",
        placeholder: "you@example.com",
        required: true,
      },
      baseUrl("https://graph.microsoft.com"),
    ],
  },
  {
    id: "gdrive",
    displayName: "Google Drive",
    category: "Files",
    needsToken: true,
    fields: [baseUrl("https://www.googleapis.com")],
  },
  {
    id: "s3",
    displayName: "Amazon S3",
    category: "Files",
    needsToken: false,
    fields: [
      {
        key: "endpoint",
        label: "Endpoint",
        placeholder: "s3.amazonaws.com",
        required: true,
      },
      {
        key: "bucket",
        label: "Bucket",
        placeholder: "my-bucket",
        required: true,
      },
      { key: "prefix", label: "Prefix", placeholder: "docs/" },
      { key: "access_key_id", label: "Access key ID" },
      { key: "secret_access_key", label: "Secret access key" },
    ],
  },
  {
    id: "upload",
    displayName: "File upload",
    category: "Files",
    needsToken: false,
    fields: [],
  },
  {
    id: "gcal",
    displayName: "Google Calendar",
    category: "Calendar",
    needsToken: true,
    fields: [
      {
        key: "calendar_id",
        label: "Calendar ID",
        placeholder: "primary",
      },
      baseUrl("https://www.googleapis.com/calendar/v3"),
    ],
  },
  {
    id: "outlook-cal",
    displayName: "Outlook Calendar",
    category: "Calendar",
    needsToken: true,
    fields: [
      {
        key: "user_principal_name",
        label: "User principal name",
        placeholder: "you@example.com",
        required: true,
      },
      baseUrl("https://graph.microsoft.com"),
    ],
  },
  {
    id: "ical",
    displayName: "iCal feed",
    category: "Calendar",
    needsToken: false,
    fields: [
      {
        key: "feed_url",
        label: "Feed URL",
        placeholder: "https://example.com/calendar.ics",
        required: true,
      },
    ],
  },
  {
    id: "slack",
    displayName: "Slack",
    category: "Chat",
    needsToken: true,
    fields: [baseUrl("https://slack.com/api")],
  },
  {
    id: "whatsapp-export",
    displayName: "WhatsApp export",
    category: "Chat",
    needsToken: false,
    fields: [],
  },
  {
    id: "confluence",
    displayName: "Confluence",
    category: "Knowledge",
    needsToken: true,
    fields: [
      {
        key: "base_url",
        label: "Base URL",
        placeholder: "https://your-domain.atlassian.net/wiki",
        required: true,
      },
      { key: "space_key", label: "Space key", placeholder: "ENG" },
    ],
  },
  {
    id: "jira",
    displayName: "Jira",
    category: "Knowledge",
    needsToken: true,
    fields: [
      {
        key: "base_url",
        label: "Base URL",
        placeholder: "https://your-domain.atlassian.net",
        required: true,
      },
      {
        key: "project_keys",
        label: "Project keys (comma-separated)",
        placeholder: "ENG, OPS",
      },
    ],
  },
];

/** Connector types grouped by category, preserving catalog order. */
export function catalogByCategory(): Array<{
  category: string;
  types: ConnectorType[];
}> {
  const groups: Array<{ category: string; types: ConnectorType[] }> = [];
  for (const type of CONNECTOR_CATALOG) {
    let group = groups.find((g) => g.category === type.category);
    if (group === undefined) {
      group = { category: type.category, types: [] };
      groups.push(group);
    }
    group.types.push(type);
  }
  return groups;
}

const labelById = new Map(CONNECTOR_CATALOG.map((t) => [t.id, t.displayName]));

/** Human label for a connector id; falls back to the raw id. */
export function connectorTypeLabel(id: string): string {
  return labelById.get(id) ?? id;
}

/**
 * Build the config object for createConnector from raw string form values.
 * Comma-separated list fields (key ends in "_keys") become string arrays;
 * blank values are dropped so the gateway sees an absent key, not "".
 */
export function buildConfig(
  type: ConnectorType,
  values: Record<string, string>,
): Record<string, unknown> {
  const config: Record<string, unknown> = {};
  for (const field of type.fields) {
    const raw = (values[field.key] ?? "").trim();
    if (raw === "") {
      continue;
    }
    if (field.key.endsWith("_keys")) {
      const list = raw
        .split(",")
        .map((s) => s.trim())
        .filter((s) => s !== "");
      if (list.length > 0) {
        config[field.key] = list;
      }
      continue;
    }
    config[field.key] = raw;
  }
  return config;
}
