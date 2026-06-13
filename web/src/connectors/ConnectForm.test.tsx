import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CONNECTOR_CATALOG, type ConnectorType } from "./catalog";
import { ConnectForm } from "./ConnectForm";

afterEach(cleanup);

const gmail = CONNECTOR_CATALOG.find((t) => t.id === "gmail") as ConnectorType;
const ical = CONNECTOR_CATALOG.find((t) => t.id === "ical") as ConnectorType;

/** Flush microtasks so a resolved/rejected onSubmit promise settles. */
const flush = () => new Promise((r) => setTimeout(r, 0));

describe("ConnectForm", () => {
  it("submits display name, built config, and token for a token connector", async () => {
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    render(<ConnectForm type={gmail} onSubmit={onSubmit} onCancel={() => {}} />);

    fireEvent.change(screen.getByLabelText("Display name"), {
      target: { value: "My Gmail" },
    });
    fireEvent.change(screen.getByLabelText(/^User email/), {
      target: { value: "alice@example.com" },
    });
    fireEvent.change(screen.getByLabelText(/^Access token/), {
      target: { value: "tok-123" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Connect" }));

    await flush();
    expect(onSubmit).toHaveBeenCalledTimes(1);
    expect(onSubmit).toHaveBeenCalledWith(
      "My Gmail",
      { user_email: "alice@example.com" },
      "tok-123",
    );
  });

  it("omits the token field for a token-free connector", () => {
    render(<ConnectForm type={ical} onSubmit={vi.fn()} onCancel={() => {}} />);
    expect(screen.queryByLabelText(/^Access token/)).toBeNull();
    expect(screen.getByLabelText(/^Feed URL/)).not.toBeNull();
  });

  it("surfaces a submit error from the handler", async () => {
    const onSubmit = vi.fn().mockRejectedValue(new Error("create failed"));
    render(<ConnectForm type={ical} onSubmit={onSubmit} onCancel={() => {}} />);

    fireEvent.change(screen.getByLabelText(/^Feed URL/), {
      target: { value: "https://x.test/c.ics" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Connect" }));

    await flush();
    expect(screen.getByRole("alert").textContent).toContain("create failed");
  });

  it("calls onCancel without submitting", () => {
    const onCancel = vi.fn();
    const onSubmit = vi.fn();
    render(<ConnectForm type={ical} onSubmit={onSubmit} onCancel={onCancel} />);

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onCancel).toHaveBeenCalledTimes(1);
    expect(onSubmit).not.toHaveBeenCalled();
  });
});
