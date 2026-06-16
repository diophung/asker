import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { SearchBox } from "./SearchBox";

afterEach(cleanup);

describe("SearchBox — Search button", () => {
  it("submits the current query when the Search button is clicked", () => {
    const onSubmit = vi.fn();
    render(
      <SearchBox
        value="roadmap"
        onChange={() => {}}
        onSubmit={onSubmit}
        suggestions={[]}
        variant="home"
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Search" }));
    expect(onSubmit).toHaveBeenCalledWith("roadmap");
  });

  it("does not submit an empty/whitespace query", () => {
    const onSubmit = vi.fn();
    render(
      <SearchBox
        value="   "
        onChange={() => {}}
        onSubmit={onSubmit}
        suggestions={[]}
        variant="home"
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Search" }));
    expect(onSubmit).not.toHaveBeenCalled();
  });
});

// --- Voice input (Web Speech API) -------------------------------------------

interface FakeResultItem {
  readonly isFinal: boolean;
  readonly 0: { transcript: string };
}
interface FakeEvent {
  resultIndex: number;
  results: ArrayLike<FakeResultItem>;
}

let lastRec: FakeRecognition | undefined;
function captureRec(rec: FakeRecognition) {
  lastRec = rec;
}

class FakeRecognition {
  lang = "";
  interimResults = false;
  continuous = false;
  maxAlternatives = 1;
  onresult: ((e: FakeEvent) => void) | null = null;
  onerror: ((e: { error: string }) => void) | null = null;
  onend: (() => void) | null = null;
  start(): void {
    captureRec(this);
  }
  stop(): void {
    this.onend?.();
  }
  abort(): void {}
}

function installSpeechRecognition() {
  (window as unknown as { SpeechRecognition?: unknown }).SpeechRecognition =
    FakeRecognition;
}
function uninstallSpeechRecognition() {
  delete (window as unknown as { SpeechRecognition?: unknown }).SpeechRecognition;
  lastRec = undefined;
}

function resultEvent(transcript: string, isFinal: boolean): FakeEvent {
  return {
    resultIndex: 0,
    results: { length: 1, 0: { isFinal, 0: { transcript } } },
  };
}

describe("SearchBox — voice input", () => {
  afterEach(uninstallSpeechRecognition);

  it("hides the mic when the browser has no Speech API", () => {
    render(
      <SearchBox
        value=""
        onChange={() => {}}
        onSubmit={() => {}}
        suggestions={[]}
        variant="home"
      />,
    );
    expect(screen.queryByRole("button", { name: /voice/i })).toBeNull();
  });

  it("dictation streams into the box and auto-submits on the final phrase", () => {
    installSpeechRecognition();
    const onChange = vi.fn();
    const onSubmit = vi.fn();
    render(
      <SearchBox
        value=""
        onChange={onChange}
        onSubmit={onSubmit}
        suggestions={[]}
        variant="home"
      />,
    );

    // Start listening.
    fireEvent.click(screen.getByRole("button", { name: "Search by voice" }));
    expect(lastRec).toBeDefined();

    // Interim words stream into the box.
    act(() => lastRec?.onresult?.(resultEvent("what's on my", false)));
    expect(onChange).toHaveBeenLastCalledWith("what's on my");

    // Final phrase + end -> auto-submit.
    act(() => lastRec?.onresult?.(resultEvent("what's on my calendar", true)));
    act(() => lastRec?.onend?.());
    expect(onSubmit).toHaveBeenCalledWith("what's on my calendar");
  });
});
