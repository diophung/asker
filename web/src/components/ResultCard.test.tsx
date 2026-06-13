import { cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ResultCard } from "./ResultCard";
import {
  imageHitMock,
  searchMock,
  videoHitMock,
} from "../mocks/searchMock";

afterEach(cleanup);

describe("ResultCard — text hits (unchanged)", () => {
  it("renders title, badges and snippet without media chrome", () => {
    const textHit = searchMock.hits[0]; // EMAIL
    const { container } = render(<ResultCard hit={textHit} />);

    expect(container.querySelector(".result-title")?.textContent).toBe(
      "Q3 planning notes",
    );
    expect(container.querySelector(".badge-type")?.textContent).toBe("Email");
    expect(container.querySelector(".snippet")).not.toBeNull();
    // No media affordances on a text hit.
    expect(container.querySelector(".result-media")).toBeNull();
    expect(container.querySelector("img.thumb")).toBeNull();
    expect(container.querySelector(".media-jump")).toBeNull();
    expect(container.querySelector(".badge-modality")).toBeNull();
    // Highlights still render safely as <mark>.
    expect(container.querySelectorAll("mark")).toHaveLength(2);
  });
});

describe("ResultCard — IMAGE hits", () => {
  it("renders an <img> sourced via fetchThumbnail and an OCR modality badge", async () => {
    const objectUrl = "blob:mock-image-url";
    const fetchThumbnail = vi.fn(async () => objectUrl);

    const { container } = render(
      <ResultCard hit={imageHitMock} fetchThumbnail={fetchThumbnail} />,
    );

    // Placeholder shown before the fetch resolves; list never blocked.
    expect(container.querySelector(".thumb-placeholder")).not.toBeNull();

    const img = await waitFor(() => {
      const el = container.querySelector("img.thumb");
      if (el === null) {
        throw new Error("img not yet rendered");
      }
      return el as HTMLImageElement;
    });

    expect(fetchThumbnail).toHaveBeenCalledWith(
      imageHitMock.thumbnail_key,
      expect.any(AbortSignal),
    );
    expect(img.getAttribute("src")).toBe(objectUrl);
    expect(img.getAttribute("alt")).toBe(imageHitMock.title);

    // OCR matched -> modality badge; no timestamp link for a still image.
    const badge = container.querySelector(".badge-modality");
    expect(badge?.textContent).toBe("OCR");
    expect(container.querySelector(".media-jump")).toBeNull();
  });

  it("does not crash or render an <img> without a thumbnail fetcher", () => {
    const { container } = render(<ResultCard hit={imageHitMock} />);
    expect(container.querySelector("img.thumb")).toBeNull();
    expect(container.querySelector(".thumb-placeholder")).toBeNull();
  });
});

describe("ResultCard — VIDEO hits", () => {
  it("renders a formatted timestamp deep-link and an ASR transcript badge", async () => {
    const fetchThumbnail = vi.fn(async () => "blob:mock-video-poster");

    const { container } = render(
      <ResultCard hit={videoHitMock} fetchThumbnail={fetchThumbnail} />,
    );

    const jump = container.querySelector(".media-jump");
    expect(jump).not.toBeNull();
    // 83000ms -> 1:23
    expect(jump?.textContent).toContain("1:23");
    expect(jump?.getAttribute("aria-label")).toBe("Jump to 1:23");
    // Deep-link preserves the seek intent as a media fragment.
    expect(jump?.getAttribute("href")).toBe("#t=83");

    expect(container.querySelector(".badge-modality")?.textContent).toBe(
      "Transcript",
    );

    // Poster still loads via fetchThumbnail.
    await waitFor(() =>
      expect(container.querySelector("img.thumb")).not.toBeNull(),
    );
    expect(fetchThumbnail).toHaveBeenCalledWith(
      videoHitMock.thumbnail_key,
      expect.any(AbortSignal),
    );
  });

  it("omits the timestamp link when start_ms is 0", () => {
    const fetchThumbnail = vi.fn(async () => "blob:x");
    const hit = { ...videoHitMock, start_ms: 0 };
    const { container } = render(
      <ResultCard hit={hit} fetchThumbnail={fetchThumbnail} />,
    );
    expect(container.querySelector(".media-jump")).toBeNull();
  });
});
