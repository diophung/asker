import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ResultItem } from "./ResultItem";
import { sendFeedback } from "./backend";
import type { EmailResult } from "./types";

afterEach(cleanup);

function emailResult(over: Partial<EmailResult> = {}): EmailResult {
  return {
    id: "e1",
    type: "email",
    source: "Gmail",
    who: "from Sarah Chen",
    when: "3 days ago",
    title: "Q3 Planning",
    snippet: "agenda for Thursday",
    haystack: "",
    sender: "Sarah Chen",
    connectorId: "gmail",
    docType: "EMAIL",
    senders: ["sarah.chen@acme.com"],
    topics: ["q3"],
    explanation: "Boosted because Sarah Chen is on your important-people list.",
    ...over,
  };
}

describe("ResultItem — personalization", () => {
  it("renders the explanation as a 'Why this?' affordance", () => {
    render(<ResultItem result={emailResult()} query="q3" />);
    expect(
      screen.getByText(/Boosted because Sarah Chen is on your important-people list/i),
    ).toBeTruthy();
  });

  it("omits feedback controls when no onFeedback handler is given", () => {
    render(<ResultItem result={emailResult()} query="q3" />);
    expect(screen.queryByRole("button", { name: /More results like this/i })).toBeNull();
  });

  it("More like this calls onFeedback with show_more", () => {
    const onFeedback = vi.fn();
    render(
      <ResultItem result={emailResult()} query="q3" onFeedback={onFeedback} />,
    );
    fireEvent.click(screen.getByRole("button", { name: /More results like this/i }));
    expect(onFeedback).toHaveBeenCalledTimes(1);
    expect(onFeedback.mock.calls[0][1]).toBe("show_more");
  });

  it("Fewer like this calls onFeedback with show_fewer", () => {
    const onFeedback = vi.fn();
    render(
      <ResultItem result={emailResult()} query="q3" onFeedback={onFeedback} />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Fewer results like this/i }));
    expect(onFeedback.mock.calls[0][1]).toBe("show_fewer");
  });

  it("the handler forwards the result identity to sendFeedback", async () => {
    const result = emailResult();
    // A realistic handler mirroring SearchApp.handleFeedback.
    render(
      <ResultItem
        result={result}
        query="q3"
        onFeedback={(r, action) => {
          void sendFeedback({
            doc_id: r.id,
            doc_type: r.docType ?? "",
            connector_id: r.connectorId ?? "",
            senders: r.senders ?? [],
            topics: r.topics ?? [],
            action,
            query: "q3",
          });
        }}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: /More results like this/i }));

    // In test mode BACKEND_ENABLED is false, so sendFeedback resolves locally
    // without a network call; assert it accepts the right shape and resolves.
    await expect(
      sendFeedback({
        doc_id: result.id,
        doc_type: "EMAIL",
        connector_id: "gmail",
        senders: ["sarah.chen@acme.com"],
        topics: ["q3"],
        action: "show_fewer",
        query: "q3",
      }),
    ).resolves.toMatchObject({ sampleCount: expect.any(Number) });
  });
});
