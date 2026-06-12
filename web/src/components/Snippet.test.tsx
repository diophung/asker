import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Snippet } from "./Snippet";

afterEach(cleanup);

describe("Snippet", () => {
  it("renders <hi> tokens as <mark> elements", () => {
    const { container } = render(
      <Snippet text="Discussed the <hi>quarterly</hi> roadmap and <hi>planning</hi>." />,
    );
    const marks = container.querySelectorAll("mark");
    expect(marks).toHaveLength(2);
    expect(marks[0].textContent).toBe("quarterly");
    expect(marks[1].textContent).toBe("planning");
    expect(container.textContent).toBe(
      "Discussed the quarterly roadmap and planning.",
    );
  });

  it("escapes HTML outside highlights instead of injecting it", () => {
    const { container } = render(
      <Snippet text={'<script>alert("xss")</script> meets <hi>terms</hi>'} />,
    );
    expect(container.querySelector("script")).toBeNull();
    expect(container.innerHTML).toContain("&lt;script&gt;");
    expect(container.textContent).toContain('<script>alert("xss")</script>');
    expect(container.querySelector("mark")?.textContent).toBe("terms");
  });

  it("escapes HTML inside highlights", () => {
    const { container } = render(
      <Snippet text="<hi><img src=x onerror=alert(1)></hi>" />,
    );
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("mark")?.textContent).toBe(
      "<img src=x onerror=alert(1)>",
    );
  });

  it("leaves stray unbalanced tags as visible literal text", () => {
    const { container } = render(<Snippet text="an unmatched <hi> token" />);
    expect(container.querySelectorAll("mark")).toHaveLength(0);
    expect(container.textContent).toBe("an unmatched <hi> token");
  });

  it("handles highlights spanning newlines", () => {
    const { container } = render(
      <Snippet text={"a <hi>multi\nline</hi> highlight"} />,
    );
    expect(container.querySelector("mark")?.textContent).toBe("multi\nline");
  });

  it("renders an empty snippet without crashing", () => {
    const { container } = render(<Snippet text="" />);
    expect(container.querySelector(".snippet")).not.toBeNull();
    expect(container.textContent).toBe("");
  });
});
