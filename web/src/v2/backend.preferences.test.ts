import { describe, it, expect } from "vitest";
import { normalizeProfile, defaultProfile } from "./backend";

// Regression: the gateway serializes empty Go slices/maps as `null`, so a
// cold-start GET /v1/preferences returns null arrays. The Settings page then
// called .map()/.includes() on null and rendered a blank screen. normalizeProfile
// must coerce every null array/object to a usable default.
describe("normalizeProfile", () => {
  it("coerces a cold-start (null arrays) wire profile into usable arrays", () => {
    const wire = {
      version: 0,
      source_weights: {},
      important_people: null,
      topics: null,
      mute: { people: null, topics: null, sources: null },
      self_emails: null,
      timezone: "",
      working_hours: { start_hour: 9, end_hour: 17 },
      attention_sensitivity: 0.5,
      recency_vs_importance: 0.5,
      novelty_vs_familiarity: 0.15,
      learning_paused: false,
      weights: { semantic: 1, preference: 0.6, behavioral: 0.5, attention: 0.8, fatigue: 0.3 },
    };
    const p = normalizeProfile(wire);
    expect(p.important_people).toEqual([]);
    expect(p.topics).toEqual([]);
    expect(p.self_emails).toEqual([]);
    expect(p.mute.people).toEqual([]);
    expect(p.mute.topics).toEqual([]);
    expect(p.mute.sources).toEqual([]);
    // The exact dereferences the Settings page makes must not throw.
    expect(() => {
      void p.important_people.length;
      void p.mute.sources.includes("EMAIL");
      void p.topics.map((t) => t);
    }).not.toThrow();
  });

  it("falls back to defaults for a null/empty profile", () => {
    const p = normalizeProfile(null);
    expect(p).toEqual(defaultProfile());
  });

  it("preserves provided values", () => {
    const p = normalizeProfile({
      important_people: ["a@x"],
      mute: { sources: ["CHAT_MESSAGE"] },
      attention_sensitivity: 1,
    });
    expect(p.important_people).toEqual(["a@x"]);
    expect(p.mute.sources).toEqual(["CHAT_MESSAGE"]);
    expect(p.mute.people).toEqual([]); // unspecified sub-field still defaults
    expect(p.attention_sensitivity).toBe(1);
  });
});
