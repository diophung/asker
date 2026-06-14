// Vitest global setup (jsdom).
//
// jsdom does not implement URL.createObjectURL / URL.revokeObjectURL, which the
// media thumbnail code relies on. Provide minimal shims so components that
// create/revoke object URLs run under test. Individual tests may still
// vi.spyOn these to assert calls; vi.restoreAllMocks() restores these shims.
let objectUrlSeq = 0;

if (typeof URL.createObjectURL !== "function") {
  URL.createObjectURL = () => `blob:test-${objectUrlSeq++}`;
}
if (typeof URL.revokeObjectURL !== "function") {
  URL.revokeObjectURL = () => {};
}
