import { describe, expect, it } from "vitest";
import {
  buildConfig,
  catalogByCategory,
  CONNECTOR_CATALOG,
  connectorTypeLabel,
  type ConnectorType,
  oauthProviderLabel,
} from "./catalog";

describe("CONNECTOR_CATALOG", () => {
  it("lists the known connector ids", () => {
    const ids = CONNECTOR_CATALOG.map((t) => t.id);
    for (const id of [
      "gmail",
      "outlook-mail",
      "gdrive",
      "gcal",
      "outlook-cal",
      "slack",
      "confluence",
      "jira",
      "s3",
      "ical",
      "whatsapp-export",
      "upload",
    ]) {
      expect(ids).toContain(id);
    }
  });

  it("marks OAuth/token connectors needsToken and token-free ones not", () => {
    const byId = new Map(CONNECTOR_CATALOG.map((t) => [t.id, t]));
    expect(byId.get("gmail")?.needsToken).toBe(true);
    expect(byId.get("slack")?.needsToken).toBe(true);
    expect(byId.get("s3")?.needsToken).toBe(false);
    expect(byId.get("ical")?.needsToken).toBe(false);
    expect(byId.get("upload")?.needsToken).toBe(false);
  });

  it("tags OAuth connectors with their provider; token-free ones have none", () => {
    const byId = new Map(CONNECTOR_CATALOG.map((t) => [t.id, t]));
    // gmail/gcal/gdrive -> google
    expect(byId.get("gmail")?.oauthProvider).toBe("google");
    expect(byId.get("gcal")?.oauthProvider).toBe("google");
    expect(byId.get("gdrive")?.oauthProvider).toBe("google");
    // outlook-mail/outlook-cal -> microsoft
    expect(byId.get("outlook-mail")?.oauthProvider).toBe("microsoft");
    expect(byId.get("outlook-cal")?.oauthProvider).toBe("microsoft");
    // slack -> slack
    expect(byId.get("slack")?.oauthProvider).toBe("slack");
    // jira/confluence -> atlassian
    expect(byId.get("jira")?.oauthProvider).toBe("atlassian");
    expect(byId.get("confluence")?.oauthProvider).toBe("atlassian");
    // Non-OAuth / token-free connectors carry no provider.
    expect(byId.get("s3")?.oauthProvider).toBeUndefined();
    expect(byId.get("ical")?.oauthProvider).toBeUndefined();
    expect(byId.get("upload")?.oauthProvider).toBeUndefined();
  });

  it("keeps needsToken true for OAuth connectors (dev paste fallback)", () => {
    for (const t of CONNECTOR_CATALOG) {
      if (t.oauthProvider !== undefined) {
        expect(t.needsToken).toBe(true);
      }
    }
  });

  it("encodes the documented config field hints", () => {
    const byId = new Map(CONNECTOR_CATALOG.map((t) => [t.id, t]));
    expect(byId.get("gmail")?.fields.map((f) => f.key)).toContain("user_email");
    expect(byId.get("s3")?.fields.map((f) => f.key)).toEqual(
      expect.arrayContaining(["endpoint", "bucket"]),
    );
    expect(byId.get("ical")?.fields.map((f) => f.key)).toEqual(["feed_url"]);
    expect(byId.get("confluence")?.fields.map((f) => f.key)).toContain(
      "base_url",
    );
  });
});

describe("catalogByCategory", () => {
  it("groups types under their category, preserving order", () => {
    const groups = catalogByCategory();
    expect(groups.map((g) => g.category)).toEqual([
      "Email",
      "Files",
      "Calendar",
      "Chat",
      "Knowledge",
    ]);
    const email = groups.find((g) => g.category === "Email");
    expect(email?.types.map((t) => t.id)).toEqual(["gmail", "outlook-mail"]);
  });
});

describe("connectorTypeLabel", () => {
  it("returns the display name and passes through unknown ids", () => {
    expect(connectorTypeLabel("gmail")).toBe("Gmail");
    expect(connectorTypeLabel("mystery")).toBe("mystery");
  });
});

describe("oauthProviderLabel", () => {
  it("maps providers to their human labels", () => {
    expect(oauthProviderLabel("google")).toBe("Google");
    expect(oauthProviderLabel("microsoft")).toBe("Microsoft");
    expect(oauthProviderLabel("slack")).toBe("Slack");
    expect(oauthProviderLabel("atlassian")).toBe("Atlassian");
  });
});

describe("buildConfig", () => {
  const jira = CONNECTOR_CATALOG.find((t) => t.id === "jira") as ConnectorType;

  it("drops blank fields so the gateway sees absent keys", () => {
    const config = buildConfig(jira, { base_url: "  ", project_keys: "" });
    expect(config).toEqual({});
  });

  it("trims scalar values and splits *_keys into arrays", () => {
    const config = buildConfig(jira, {
      base_url: "  https://acme.atlassian.net  ",
      project_keys: "ENG, OPS ,",
    });
    expect(config).toEqual({
      base_url: "https://acme.atlassian.net",
      project_keys: ["ENG", "OPS"],
    });
  });

  it("ignores values for keys not in the type's fields", () => {
    const config = buildConfig(jira, {
      base_url: "https://acme.atlassian.net",
      rogue: "ignored",
    });
    expect(config).not.toHaveProperty("rogue");
  });
});
