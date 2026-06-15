import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// Mock the backend seam the same way the personalization test does: stub the
// functions the Settings page imports directly, keeping the real helpers.
const getIndexStatus = vi.fn();
const reindexConnector = vi.fn();

vi.mock("./backend", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./backend")>();
  return {
    ...actual,
    getIndexStatus: () => getIndexStatus(),
    reindexConnector: (...a: unknown[]) => reindexConnector(...a),
    // Stub the rest of the Settings page's backend deps so they don't reach out.
    getMe: vi.fn().mockResolvedValue({ email: "alice@test", tenantId: "t-123456" }),
    getPreferences: vi
      .fn()
      .mockResolvedValue({ profile: actual.defaultProfile(), sampleCount: 0 }),
  };
});

// The connector client (used by the rest of the Settings page).
vi.mock("../api", () => ({
  ConnectorClient: class {
    listConnectors = vi.fn().mockResolvedValue([]);
    deleteConnector = vi.fn().mockResolvedValue(undefined);
  },
}));

import { Settings } from "./Settings";

const STATUS = {
  indexed: 850,
  emitted: 1000,
  backlog: 150,
  syncing: true,
  connectors: [
    {
      id: "inst-gmail-1",
      connector_id: "gmail",
      display_name: "Gmail",
      phase: "FULL_SYNC",
      docs_emitted: 1000,
      last_sync_completed: "",
      last_error: "",
    },
  ],
};

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

beforeEach(() => {
  getIndexStatus.mockResolvedValue(STATUS);
  reindexConnector.mockResolvedValue(undefined);
});

function renderSettings() {
  render(<Settings onBack={() => {}} onSignOut={() => {}} />);
}

describe("Settings — Indexing", () => {
  it("loads the index status and renders indexed/emitted, backlog, and a connector row", async () => {
    renderSettings();

    expect(getIndexStatus).toHaveBeenCalled();

    // Headline: "850 of 1,000 documents indexed".
    const live = await screen.findByLabelText("Indexing status");
    expect(live.textContent).toMatch(/850\s+of\s+1,000\s+documents indexed/);

    // Backlog surfaced when > 0.
    expect(screen.getByText(/150 still indexing/i)).toBeTruthy();

    // Syncing indicator + a progress bar reflecting 85%.
    expect(screen.getByText(/Syncing/i)).toBeTruthy();
    expect(screen.getByRole("progressbar").getAttribute("aria-valuenow")).toBe("85");

    // The connector row + its phase ("Indexing…" for FULL_SYNC).
    expect(screen.getByText("Gmail")).toBeTruthy();
    expect(screen.getByText("Indexing…")).toBeTruthy();
  });

  it("re-index button confirms then calls reindexConnector with the connector id", async () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    renderSettings();

    const btn = await screen.findByRole("button", { name: "Re-index Gmail" });
    fireEvent.click(btn);

    expect(confirmSpy).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(reindexConnector).toHaveBeenCalledWith("inst-gmail-1"));

    // A brief acknowledgement appears.
    expect(await screen.findByText(/Re-indexing started/i)).toBeTruthy();

    confirmSpy.mockRestore();
  });

  it("does not re-index when the confirmation is declined", async () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderSettings();

    const btn = await screen.findByRole("button", { name: "Re-index Gmail" });
    fireEvent.click(btn);

    expect(confirmSpy).toHaveBeenCalledTimes(1);
    expect(reindexConnector).not.toHaveBeenCalled();

    confirmSpy.mockRestore();
  });

  it("renders an unknown indexed count as a dash, not -1", async () => {
    getIndexStatus.mockResolvedValue({ ...STATUS, indexed: -1, backlog: 0 });
    renderSettings();

    const live = await screen.findByLabelText("Indexing status");
    await waitFor(() => expect(live.textContent).toMatch(/—\s+of\s+1,000/));
    expect(live.textContent).not.toMatch(/-1/);
  });
});
