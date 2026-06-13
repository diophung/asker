import { describe, expect, it } from "vitest";
import {
  formatTimestamp,
  isMediaType,
  isTimedMediaType,
  modalityLabel,
} from "./media";

describe("formatTimestamp", () => {
  it("formats sub-minute offsets as M:SS with zero-padded seconds", () => {
    expect(formatTimestamp(0)).toBe("0:00");
    expect(formatTimestamp(5000)).toBe("0:05");
    expect(formatTimestamp(59000)).toBe("0:59");
  });

  it("formats minute offsets as M:SS", () => {
    expect(formatTimestamp(60000)).toBe("1:00");
    expect(formatTimestamp(83000)).toBe("1:23");
    expect(formatTimestamp(12 * 60000 + 34000)).toBe("12:34");
  });

  it("formats hour-plus offsets as H:MM:SS", () => {
    expect(formatTimestamp(3600000)).toBe("1:00:00");
    expect(formatTimestamp(3723000)).toBe("1:02:03");
    expect(formatTimestamp(10 * 3600000 + 5 * 60000 + 9000)).toBe("10:05:09");
  });

  it("floors sub-second milliseconds", () => {
    expect(formatTimestamp(5999)).toBe("0:05");
    expect(formatTimestamp(1500)).toBe("0:01");
  });

  it("clamps negative and non-finite inputs to 0:00", () => {
    expect(formatTimestamp(-1000)).toBe("0:00");
    expect(formatTimestamp(Number.NaN)).toBe("0:00");
    expect(formatTimestamp(Number.POSITIVE_INFINITY)).toBe("0:00");
  });
});

describe("modalityLabel", () => {
  it("labels known modalities", () => {
    expect(modalityLabel("transcript")).toBe("transcript");
    expect(modalityLabel("caption")).toBe("caption");
    expect(modalityLabel("ocr")).toBe("OCR");
    expect(modalityLabel("visual")).toBe("visual");
  });

  it("passes through unknown modalities as-is", () => {
    expect(modalityLabel("something-new")).toBe("something-new");
    expect(modalityLabel("")).toBe("");
  });
});

describe("media type predicates", () => {
  it("recognizes media types", () => {
    expect(isMediaType("IMAGE")).toBe(true);
    expect(isMediaType("VIDEO")).toBe(true);
    expect(isMediaType("AUDIO")).toBe(true);
    expect(isMediaType("EMAIL")).toBe(false);
    expect(isMediaType("FILE")).toBe(false);
  });

  it("recognizes timed media types", () => {
    expect(isTimedMediaType("VIDEO")).toBe(true);
    expect(isTimedMediaType("AUDIO")).toBe(true);
    expect(isTimedMediaType("IMAGE")).toBe(false);
    expect(isTimedMediaType("EMAIL")).toBe(false);
  });
});
