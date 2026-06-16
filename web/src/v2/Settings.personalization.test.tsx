import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// Mock the backend seam: the Settings page imports these directly, so we stub
// them per the existing vi.fn() test style (see connectors/*.test.tsx).
const getPreferences = vi.fn();
const savePreferences = vi.fn();
const resetLearning = vi.fn();
const exportPersonalization = vi.fn();

vi.mock("./backend", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./backend")>();
  return {
    ...actual,
    // Keep the real defaultProfile() so the component renders sensibly.
    getPreferences: () => getPreferences(),
    savePreferences: (...a: unknown[]) => savePreferences(...a),
    resetLearning: () => resetLearning(),
    exportPersonalization: () => exportPersonalization(),
    // Stub the rest of the Settings page's backend deps so they don't reach out.
    getMe: vi.fn().mockResolvedValue({ email: "alice@test", tenantId: "t-123456" }),
  };
});

// The connector client (used by the rest of the Settings page).
vi.mock("../api", () => ({
  ConnectorClient: class {
    listConnectors = vi.fn().mockResolvedValue([]);
    deleteConnector = vi.fn().mockResolvedValue(undefined);
  },
}));

import { defaultProfile } from "./backend";
import { Settings } from "./Settings";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

beforeEach(() => {
  getPreferences.mockResolvedValue({
    profile: defaultProfile(),
    sampleCount: 7,
  });
  savePreferences.mockResolvedValue(2);
  resetLearning.mockResolvedValue(3);
  exportPersonalization.mockResolvedValue({ profile: defaultProfile() });
});

function renderSettings() {
  render(<Settings onBack={() => {}} onSignOut={() => {}} />);
}

describe("Settings — Personalization", () => {
  it("loads preferences on mount and renders the controls + sample count", async () => {
    renderSettings();

    expect(getPreferences).toHaveBeenCalledTimes(1);

    // The whole spread of preference controls renders.
    expect(await screen.findByText("Priority sources")).toBeTruthy();
    expect(screen.getByText("Important people")).toBeTruthy();
    expect(screen.getByText("Topics & projects")).toBeTruthy();
    expect(screen.getByText("Muted people")).toBeTruthy();
    expect(screen.getByText("Working hours")).toBeTruthy();
    expect(screen.getByLabelText("Attention sensitivity")).toBeTruthy();
    expect(screen.getByLabelText("Recency vs. importance")).toBeTruthy();
    expect(screen.getByLabelText("Novelty vs. familiarity")).toBeTruthy();
    expect(screen.getByLabelText("Pause learning")).toBeTruthy();
    expect(screen.getByLabelText("Timezone")).toBeTruthy();

    // "Learned from N interactions" reflects the sample count.
    expect(screen.getByTestId("sample-count").textContent).toMatch(
      /Learned from 7 interactions/i,
    );
  });

  it("changing a slider persists via savePreferences", async () => {
    renderSettings();
    const slider = await screen.findByLabelText("Recency vs. importance");

    fireEvent.change(slider, { target: { value: "0.9" } });

    await waitFor(() => expect(savePreferences).toHaveBeenCalledTimes(1));
    const saved = savePreferences.mock.calls[0][0] as ReturnType<
      typeof defaultProfile
    >;
    expect(saved.recency_vs_importance).toBeCloseTo(0.9);
  });

  it("toggling pause learning persists the flag", async () => {
    renderSettings();
    const toggle = await screen.findByLabelText("Pause learning");

    fireEvent.click(toggle);

    await waitFor(() => expect(savePreferences).toHaveBeenCalledTimes(1));
    const saved = savePreferences.mock.calls[0][0] as ReturnType<
      typeof defaultProfile
    >;
    expect(saved.learning_paused).toBe(true);
  });

  it("adding an important person persists the new list", async () => {
    renderSettings();
    const input = await screen.findByLabelText("Add an important person");

    fireEvent.change(input, { target: { value: "alice@example.com" } });
    fireEvent.keyDown(input, { key: "Enter" });

    await waitFor(() => expect(savePreferences).toHaveBeenCalledTimes(1));
    const saved = savePreferences.mock.calls[0][0] as ReturnType<
      typeof defaultProfile
    >;
    expect(saved.important_people).toContain("alice@example.com");
  });

  it("reset action calls resetLearning after confirming", async () => {
    renderSettings();

    const open = await screen.findByRole("button", {
      name: /Reset what you.?ve learned about me/i,
    });
    fireEvent.click(open);

    const confirm = await screen.findByRole("button", { name: "Reset learning" });
    fireEvent.click(confirm);

    await waitFor(() => expect(resetLearning).toHaveBeenCalledTimes(1));
    expect(await screen.findByText(/Learning reset\./i)).toBeTruthy();
  });
});
