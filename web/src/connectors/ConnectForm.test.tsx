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
    const onSubmit = vi.fn().mockResolvedValue("inst-new-1");
    const onDone = vi.fn();
    render(
      <ConnectForm
        type={gmail}
        onSubmit={onSubmit}
        onDone={onDone}
        onCancel={() => {}}
      />,
    );

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
    // A pasted token skips OAuth and finishes the flow immediately.
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  it("omits the token field for a token-free connector", () => {
    render(
      <ConnectForm
        type={ical}
        onSubmit={vi.fn()}
        onDone={() => {}}
        onCancel={() => {}}
      />,
    );
    expect(screen.queryByLabelText(/^Access token/)).toBeNull();
    expect(screen.getByLabelText(/^Feed URL/)).not.toBeNull();
  });

  it("surfaces a submit error from the handler", async () => {
    const onSubmit = vi.fn().mockRejectedValue(new Error("create failed"));
    render(
      <ConnectForm
        type={ical}
        onSubmit={onSubmit}
        onDone={() => {}}
        onCancel={() => {}}
      />,
    );

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
    render(
      <ConnectForm
        type={ical}
        onSubmit={onSubmit}
        onDone={() => {}}
        onCancel={onCancel}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onCancel).toHaveBeenCalledTimes(1);
    expect(onSubmit).not.toHaveBeenCalled();
  });

  describe("OAuth connector flow", () => {
    it("makes the access token optional for an oauthProvider connector", () => {
      render(
        <ConnectForm
          type={gmail}
          onSubmit={vi.fn()}
          onDone={() => {}}
          onCancel={() => {}}
        />,
      );
      const token = screen.getByLabelText(/^Access token/) as HTMLInputElement;
      expect(token.required).toBe(false);
    });

    it("shows a 'Connect with <Provider>' button after create and navigates on click", async () => {
      // Create resolves with the new id; no manual token -> OAuth step.
      const onSubmit = vi.fn().mockResolvedValue("inst-new-1");
      const onStartOAuth = vi
        .fn()
        .mockResolvedValue("https://accounts.example/authorize?x=1");
      const onNavigate = vi.fn();
      const onDone = vi.fn();
      render(
        <ConnectForm
          type={gmail}
          onSubmit={onSubmit}
          onStartOAuth={onStartOAuth}
          onNavigate={onNavigate}
          onDone={onDone}
          onCancel={() => {}}
        />,
      );

      fireEvent.change(screen.getByLabelText(/^User email/), {
        target: { value: "alice@example.com" },
      });
      // Leave the token blank to take the OAuth path.
      fireEvent.click(screen.getByRole("button", { name: "Connect" }));

      await flush();
      // The form must NOT close yet — it offers the provider sign-in step.
      expect(onDone).not.toHaveBeenCalled();
      const connectBtn = await screen.findByRole("button", {
        name: /Connect with Google/,
      });
      expect(connectBtn).not.toBeNull();

      fireEvent.click(connectBtn);
      await flush();
      expect(onStartOAuth).toHaveBeenCalledWith("inst-new-1");
      expect(onNavigate).toHaveBeenCalledWith(
        "https://accounts.example/authorize?x=1",
      );
    });

    it("surfaces an error if startOAuth fails, without navigating", async () => {
      const onSubmit = vi.fn().mockResolvedValue("inst-new-1");
      const onStartOAuth = vi
        .fn()
        .mockRejectedValue(new Error("provider not configured"));
      const onNavigate = vi.fn();
      render(
        <ConnectForm
          type={gmail}
          onSubmit={onSubmit}
          onStartOAuth={onStartOAuth}
          onNavigate={onNavigate}
          onDone={() => {}}
          onCancel={() => {}}
        />,
      );

      fireEvent.change(screen.getByLabelText(/^User email/), {
        target: { value: "alice@example.com" },
      });
      fireEvent.click(screen.getByRole("button", { name: "Connect" }));
      await flush();

      fireEvent.click(
        await screen.findByRole("button", { name: /Connect with Google/ }),
      );
      await flush();
      expect(onNavigate).not.toHaveBeenCalled();
      expect(screen.getByRole("alert").textContent).toContain(
        "provider not configured",
      );
    });
  });
});
