import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ConnectorClient } from "../api";
import {
  connectorsMock,
  createdInstance,
} from "../mocks/connectorsMock";
import { ConnectorsPage } from "./ConnectorsPage";

afterEach(cleanup);

/** A fake ConnectorClient with vi.fn() methods, typed as the real interface. */
function fakeClient(
  overrides: Partial<Record<keyof ConnectorClient, unknown>> = {},
): {
  client: ConnectorClient;
  listConnectors: ReturnType<typeof vi.fn>;
  createConnector: ReturnType<typeof vi.fn>;
  putConnectorToken: ReturnType<typeof vi.fn>;
  deleteConnector: ReturnType<typeof vi.fn>;
  startOAuth: ReturnType<typeof vi.fn>;
} {
  const listConnectors = vi.fn().mockResolvedValue(connectorsMock);
  const createConnector = vi.fn().mockResolvedValue(createdInstance);
  const putConnectorToken = vi.fn().mockResolvedValue(undefined);
  const deleteConnector = vi.fn().mockResolvedValue(undefined);
  const startOAuth = vi
    .fn()
    .mockResolvedValue("https://accounts.example/authorize?x=1");
  const client = {
    listConnectors,
    createConnector,
    putConnectorToken,
    deleteConnector,
    startOAuth,
    ...overrides,
  } as unknown as ConnectorClient;
  return {
    client,
    listConnectors,
    createConnector,
    putConnectorToken,
    deleteConnector,
    startOAuth,
  };
}

/** Reset the URL between tests so the ?oauth= banner logic starts clean. */
beforeEach(() => {
  window.history.replaceState(null, "", "/");
});

describe("ConnectorsPage", () => {
  it("renders the catalog grouped by category", async () => {
    const { client } = fakeClient();
    render(<ConnectorsPage client={client} />);

    expect(screen.getByText("Email")).not.toBeNull();
    expect(screen.getByText("Files")).not.toBeNull();
    // Catalog buttons for known types.
    expect(
      screen.getByRole("button", { name: /Gmail Connect/ }),
    ).not.toBeNull();

    await waitFor(() =>
      expect(screen.getByText("Work Gmail")).not.toBeNull(),
    );
  });

  it("lists the tenant's existing instances with status and errors", async () => {
    const { client } = fakeClient();
    render(<ConnectorsPage client={client} />);

    await waitFor(() =>
      expect(screen.getByText("Eng Jira")).not.toBeNull(),
    );
    expect(screen.getByText("error")).not.toBeNull();
    expect(
      screen.getAllByRole("alert").some((el) =>
        el.textContent?.includes("token expired"),
      ),
    ).toBe(true);
  });

  it("creates a connector then stores its token with the right shape", async () => {
    const { client, createConnector, putConnectorToken, listConnectors } =
      fakeClient();
    render(<ConnectorsPage client={client} />);
    await waitFor(() => expect(listConnectors).toHaveBeenCalled());

    // Open the Gmail connect form.
    fireEvent.click(screen.getByRole("button", { name: /Gmail Connect/ }));
    fireEvent.change(screen.getByLabelText(/^User email/), {
      target: { value: "alice@example.com" },
    });
    fireEvent.change(screen.getByLabelText(/^Access token/), {
      target: { value: "tok-xyz" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Connect" }));

    await waitFor(() => expect(createConnector).toHaveBeenCalledTimes(1));
    expect(createConnector).toHaveBeenCalledWith("gmail", "Gmail", {
      user_email: "alice@example.com",
    });
    expect(putConnectorToken).toHaveBeenCalledWith(
      createdInstance.id,
      "tok-xyz",
    );
  });

  it("offers the provider OAuth button after creating an OAuth connector without a token", async () => {
    const { client, createConnector, putConnectorToken, startOAuth } =
      fakeClient();
    // jsdom's window.location.assign is not configurable; make startOAuth
    // never resolve so the click can't trigger a real top-level navigation.
    // The actual assign() navigation is covered by ConnectForm's onNavigate
    // unit test; here we only assert the page wires startOAuth(id).
    startOAuth.mockReturnValue(new Promise<string>(() => {}));
    render(<ConnectorsPage client={client} />);

    fireEvent.click(screen.getByRole("button", { name: /Gmail Connect/ }));
    fireEvent.change(screen.getByLabelText(/^User email/), {
      target: { value: "alice@example.com" },
    });
    // Leave the access token blank -> OAuth sign-in path.
    fireEvent.click(screen.getByRole("button", { name: "Connect" }));

    await waitFor(() => expect(createConnector).toHaveBeenCalledTimes(1));
    // No manual token was pasted, so putConnectorToken must not be called.
    expect(putConnectorToken).not.toHaveBeenCalled();

    const oauthBtn = await screen.findByRole("button", {
      name: /Connect with Google/,
    });
    fireEvent.click(oauthBtn);

    await waitFor(() =>
      expect(startOAuth).toHaveBeenCalledWith(createdInstance.id),
    );
  });

  it("deletes an instance after confirmation", async () => {
    const { client, deleteConnector } = fakeClient();
    render(<ConnectorsPage client={client} />);
    await waitFor(() => expect(screen.getByText("Work Gmail")).not.toBeNull());

    // Two instances -> two Delete buttons; act on the first.
    fireEvent.click(screen.getAllByRole("button", { name: "Delete" })[0]);
    fireEvent.click(screen.getByRole("button", { name: "Confirm" }));

    await waitFor(() =>
      expect(deleteConnector).toHaveBeenCalledWith("inst-gmail-1"),
    );
  });

  it("shows an error state when listing fails", async () => {
    const { client } = fakeClient({
      listConnectors: vi.fn().mockRejectedValue(new Error("gateway down")),
    });
    render(<ConnectorsPage client={client} />);

    await waitFor(() =>
      expect(screen.getByText(/could not load connectors/i)).not.toBeNull(),
    );
    expect(screen.getByText("gateway down")).not.toBeNull();
  });

  describe("OAuth result banner", () => {
    it("shows a success banner and clears ?oauth=connected from the URL", async () => {
      window.history.replaceState(null, "", "/connectors?oauth=connected");
      const { client } = fakeClient();
      render(<ConnectorsPage client={client} />);

      await waitFor(() =>
        expect(screen.getByRole("status").textContent).toContain(
          "Account connected",
        ),
      );
      // Param is stripped so a reload won't re-show the banner.
      expect(window.location.search).toBe("");

      // Dismiss hides the banner.
      fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
      expect(screen.queryByRole("status")).toBeNull();
    });

    it("shows an error banner for ?oauth=error", async () => {
      window.history.replaceState(null, "", "/connectors?oauth=error");
      const { client } = fakeClient();
      render(<ConnectorsPage client={client} />);

      await waitFor(() =>
        expect(
          screen
            .getAllByRole("alert")
            .some((el) => el.textContent?.includes("Could not connect")),
        ).toBe(true),
      );
      expect(window.location.search).toBe("");
    });

    it("preserves unrelated query params when clearing ?oauth", async () => {
      window.history.replaceState(null, "", "/connectors?tab=files&oauth=connected");
      const { client } = fakeClient();
      render(<ConnectorsPage client={client} />);

      await waitFor(() => expect(screen.getByRole("status")).not.toBeNull());
      expect(window.location.search).toBe("?tab=files");
    });
  });
});
