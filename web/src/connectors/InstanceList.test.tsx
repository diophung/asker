import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { connectorsMock, gmailInstance } from "../mocks/connectorsMock";
import { InstanceList } from "./InstanceList";

afterEach(cleanup);

const flush = () => new Promise((r) => setTimeout(r, 0));

describe("InstanceList", () => {
  it("renders an empty state when there are no instances", () => {
    render(<InstanceList instances={[]} onDelete={vi.fn()} />);
    expect(screen.getByText(/no connectors yet/i)).not.toBeNull();
  });

  it("renders name, type, status, phase, docs, and last error", () => {
    render(<InstanceList instances={connectorsMock} onDelete={vi.fn()} />);

    // Gmail row: type label, status, phase, doc count.
    expect(screen.getByText("Work Gmail")).not.toBeNull();
    expect(screen.getByText("Gmail")).not.toBeNull();
    expect(screen.getByText("active")).not.toBeNull();
    expect(screen.getByText("incremental")).not.toBeNull();
    expect(screen.getByText("1,284 docs")).not.toBeNull();

    // Jira row: error surfaced as an alert.
    expect(screen.getByText("Eng Jira")).not.toBeNull();
    expect(screen.getByText("error")).not.toBeNull();
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("token expired");
  });

  it("requires confirmation before deleting and calls onDelete with the id", async () => {
    const onDelete = vi.fn().mockResolvedValue(undefined);
    render(<InstanceList instances={[gmailInstance]} onDelete={onDelete} />);

    // First click only asks to confirm; nothing deleted yet.
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onDelete).not.toHaveBeenCalled();
    expect(screen.getByText("Delete?")).not.toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
    await flush();
    expect(onDelete).toHaveBeenCalledWith(gmailInstance.instance.id);
  });

  it("can cancel the delete confirmation", () => {
    const onDelete = vi.fn();
    render(<InstanceList instances={[gmailInstance]} onDelete={onDelete} />);

    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText("Delete?")).toBeNull();
    expect(onDelete).not.toHaveBeenCalled();
  });
});
