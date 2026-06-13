import { cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Thumbnail } from "./Thumbnail";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Thumbnail", () => {
  it("shows a placeholder before the fetch resolves, then the image", async () => {
    let resolve!: (url: string) => void;
    const fetchThumbnail = vi.fn(
      () =>
        new Promise<string>((r) => {
          resolve = r;
        }),
    );
    const { container } = render(
      <Thumbnail
        thumbnailKey="k1"
        alt="a preview"
        fetchThumbnail={fetchThumbnail}
      />,
    );

    expect(container.querySelector(".thumb-placeholder")).not.toBeNull();
    expect(container.querySelector("img")).toBeNull();

    resolve("blob:obj-1");
    const img = await waitFor(() => {
      const el = container.querySelector("img");
      if (el === null) throw new Error("not yet");
      return el as HTMLImageElement;
    });
    expect(img.getAttribute("src")).toBe("blob:obj-1");
    expect(img.getAttribute("alt")).toBe("a preview");
  });

  it("revokes the object URL on unmount", async () => {
    const revoke = vi
      .spyOn(URL, "revokeObjectURL")
      .mockImplementation(() => {});
    const fetchThumbnail = vi.fn(async () => "blob:obj-2");

    const { container, unmount } = render(
      <Thumbnail thumbnailKey="k2" alt="x" fetchThumbnail={fetchThumbnail} />,
    );
    await waitFor(() =>
      expect(container.querySelector("img")).not.toBeNull(),
    );

    unmount();
    expect(revoke).toHaveBeenCalledWith("blob:obj-2");
  });

  it("renders an accessible fallback when the fetch fails", async () => {
    const fetchThumbnail = vi.fn(async () => {
      throw new Error("404");
    });
    const { container } = render(
      <Thumbnail thumbnailKey="k3" alt="broken" fetchThumbnail={fetchThumbnail} />,
    );
    await waitFor(() =>
      expect(container.querySelector(".thumb-fallback")).not.toBeNull(),
    );
    const fallback = container.querySelector(".thumb-fallback");
    expect(fallback?.getAttribute("role")).toBe("img");
    expect(fallback?.getAttribute("aria-label")).toBe("broken");
  });
});
