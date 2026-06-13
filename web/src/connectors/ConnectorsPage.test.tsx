import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
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
} {
  const listConnectors = vi.fn().mockResolvedValue(connectorsMock);
  const createConnector = vi.fn().mockResolvedValue(createdInstance);
  const putConnectorToken = vi.fn().mockResolvedValue(undefined);
  const deleteConnector = vi.fn().mockResolvedValue(undefined);
  const client = {
    listConnectors,
    createConnector,
    putConnectorToken,
    deleteConnector,
    ...overrides,
  } as unknown as ConnectorClient;
  return {
    client,
    listConnectors,
    createConnector,
    putConnectorToken,
    deleteConnector,
  };
}

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
});
