import { describe, expect, it } from "vitest";
import { safeHttpUrl } from "./safeUrl";

describe("safeHttpUrl", () => {
  it("passes through absolute http(s) URLs unchanged", () => {
    for (const url of [
      "https://mail.google.com/mail/u/0/#all/abc123",
      "http://example.com/a/b?c=d#e",
      "https://example.com:8443/path",
    ]) {
      expect(safeHttpUrl(url)).toBe(url);
    }
  });

  it("rejects script-bearing schemes that would execute in our origin", () => {
    for (const url of [
      "javascript:alert(1)",
      "JavaScript:alert(1)",
      "  javascript:alert(1)  ",
      "java\tscript:alert(1)",
      "data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==",
      "vbscript:msgbox(1)",
      "file:///etc/passwd",
      "blob:https://example.com/uuid",
    ]) {
      expect(safeHttpUrl(url)).toBe("");
    }
  });

  it("rejects relative, scheme-relative and hostless forms", () => {
    // "https:evil" and "https:/evil.com" are the subtle ones: URL() rewrites
    // both into a real host, so a protocol+hostname check alone lets them by.
    for (const url of [
      "//evil.com/x",
      "/local/path",
      "https:evil",
      "https:/evil.com",
      "http://",
    ]) {
      expect(safeHttpUrl(url)).toBe("");
    }
  });

  it("rejects empty and absent values", () => {
    expect(safeHttpUrl("")).toBe("");
    expect(safeHttpUrl("   ")).toBe("");
    expect(safeHttpUrl(undefined)).toBe("");
    expect(safeHttpUrl(null)).toBe("");
  });
});
