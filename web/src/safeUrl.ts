/** Scheme gate for values that become an href.
 *
 * Result links originate from third-party providers (a calendar invite's
 * html_link, a Slack permalink, a Drive web_view_link) and are attacker
 * controllable: anyone who can send the user an event or a message controls
 * the string we index. Rendering such a value as an href without a scheme
 * check allows `javascript:`/`data:` URLs to execute in Asker's origin the
 * moment the user clicks a search result, which would expose the bearer
 * token and the whole indexed corpus.
 *
 * The gateway applies the same gate server-side when it derives `source_url`
 * (see sourceURLFromMetadata in services/gateway/search.go). This is the
 * client-side counterpart, so any value reaching an href is checked even when
 * it did not come through that field.
 */

/** Absolute http(s) URLs only. Returns "" for anything else. */
export function safeHttpUrl(raw: string | undefined | null): string {
  if (raw === undefined || raw === null) return "";
  const v = raw.trim();
  if (v === "") return "";
  // Require the explicit authority form. URL() normalizes the abbreviated
  // "https:evil" and "https:/evil.com" into a real host, so a protocol/hostname
  // check alone would accept them; matching "://" up front keeps the value the
  // user sees equal to the origin the browser will navigate to.
  if (!/^https?:\/\//i.test(v)) return "";
  let u: URL;
  try {
    // Parsed without a base, so relative/scheme-relative values ("//evil.com",
    // "/path") fail here rather than silently resolving against our origin.
    u = new URL(v);
  } catch {
    return "";
  }
  if (u.protocol !== "http:" && u.protocol !== "https:") return "";
  if (u.hostname === "") return "";
  return v;
}
