import { describe, expect, it } from "vitest";
import {
  freeTextEntryKey,
  normalizePhraseQuery,
  phraseLookupPosition,
  segmentArticleSummary,
  splitSummaryTokens,
} from "./phraseSearch";

describe("splitSummaryTokens", () => {
  it("keeps word tokens at odd indices so their index is a stable lookup position", () => {
    const tokens = splitSummaryTokens("Scientists announced a new discovery today.");
    expect(tokens[1]).toBe("Scientists");
    expect(tokens[3]).toBe("announced");
    expect(tokens[5]).toBe("a");
    expect(tokens[7]).toBe("new");
    expect(tokens[9]).toBe("discovery");
  });

  it("treats apostrophes as part of a word", () => {
    const tokens = splitSummaryTokens("they don't stop");
    expect(tokens).toContain("don't");
  });
});

describe("normalizePhraseQuery", () => {
  it("trims and collapses internal whitespace", () => {
    expect(normalizePhraseQuery("  in   spite  of ")).toBe("in spite of");
  });

  it("strips edge punctuation but keeps internal punctuation", () => {
    expect(normalizePhraseQuery("“Talk, again!”")).toBe("Talk, again");
  });

  it("preserves case — the learner sees and looks up the text as written", () => {
    expect(normalizePhraseQuery("New York")).toBe("New York");
  });

  it("reduces a punctuation-only query to the empty string", () => {
    expect(normalizePhraseQuery("...")).toBe("");
  });
});

describe("segmentArticleSummary", () => {
  const rejoin = (summary: string, phrases: string[]) =>
    segmentArticleSummary(summary, phrases).map((segment) => segment.text).join("");

  it("reproduces the summary exactly — tokenizing never adds or drops a character", () => {
    const cases = [
      "They said that everything was fine.",
      "“Talk, again!” she urged — again and again.",
      "No phrases here, at all!",
      "",
      "...",
    ];
    for (const summary of cases) {
      expect(rejoin(summary, ["said that", "talk again", "urged"])).toBe(summary);
    }
  });

  it("merges a suggested phrase into one segment keyed at its first word's token index", () => {
    const segments = segmentArticleSummary("They said that everything was fine.", ["said that"]);
    const phrase = segments.find((segment) => segment.type === "phrase");
    expect(phrase).toEqual({ type: "phrase", text: "said that", key: 3 });
    // The merged words must not also appear as individual word buttons.
    expect(segments.filter((segment) => segment.type === "word").map((segment) => segment.text))
      .toEqual(["They", "everything", "was", "fine"]);
  });

  it("swallows punctuation sitting between the phrase's words", () => {
    const segments = segmentArticleSummary("He said he would talk, again tomorrow.", ["talk again"]);
    const phrase = segments.find((segment) => segment.type === "phrase");
    // The tap sends this verbatim substring; the comma stays with the text.
    expect(phrase).toEqual({ type: "phrase", text: "talk, again", key: 9 });
  });

  it("matches case-insensitively", () => {
    const segments = segmentArticleSummary("They SAID that it worked.", ["said that"]);
    expect(segments.some((segment) => segment.type === "phrase" && segment.text === "SAID that")).toBe(true);
  });

  it("prefers the longest phrase at a position", () => {
    const segments = segmentArticleSummary("He agreed to talk again later.", ["talk again", "agreed to talk again"]);
    const phrase = segments.find((segment) => segment.type === "phrase");
    expect(phrase).toEqual({ type: "phrase", text: "agreed to talk again", key: 3 });
  });

  it("never overlaps: the first greedy match consumes its words", () => {
    const segments = segmentArticleSummary("talk again and again", ["talk again", "again and"]);
    const phraseTexts = segments.filter((segment) => segment.type === "phrase").map((segment) => segment.text);
    expect(phraseTexts).toEqual(["talk again"]);
  });

  it("ignores single-word phrases — those stay plain word taps", () => {
    const segments = segmentArticleSummary("The design won prizes.", ["design"]);
    expect(segments.some((segment) => segment.type === "phrase")).toBe(false);
    expect(segments.some((segment) => segment.type === "word" && segment.text === "design")).toBe(true);
  });

  it("ignores phrases whose words can never match text tokens (hyphenated), without breaking the text", () => {
    const summary = "The state of the art design won prizes.";
    const segments = segmentArticleSummary(summary, ["state-of-the-art"]);
    expect(segments.some((segment) => segment.type === "phrase")).toBe(false);
    expect(rejoin(summary, ["state-of-the-art"])).toBe(summary);
  });

  it("handles several phrases in one paragraph", () => {
    const segments = segmentArticleSummary("He said that he would talk again soon.", ["said that", "talk again"]);
    const phraseTexts = segments.filter((segment) => segment.type === "phrase").map((segment) => segment.text);
    expect(phraseTexts).toEqual(["said that", "talk again"]);
  });

  it("with no phrases, every word is its own keyed segment", () => {
    const segments = segmentArticleSummary("Two words.", []);
    const words = segments.filter((segment) => segment.type === "word");
    expect(words).toEqual([
      { type: "word", text: "Two", key: 1 },
      { type: "word", text: "words", key: 3 },
    ]);
  });
});

describe("phraseLookupPosition", () => {
  const summary = "Scientists announced a new discovery today.";

  it("returns the first word's token index when the query occurs in the text", () => {
    expect(phraseLookupPosition(summary, "announced a new discovery")).toBe(3);
    expect(phraseLookupPosition(summary, "new discovery")).toBe(7);
    expect(phraseLookupPosition(summary, "discovery")).toBe(9);
  });

  it("matches case- and punctuation-insensitively, like the segmenter", () => {
    expect(phraseLookupPosition(summary, "  Announced   a NEW discovery. ")).toBe(3);
  });

  it("falls back to a deterministic hash inside the server's accepted range off-text", () => {
    const first = phraseLookupPosition(summary, "strong economic growth");
    const second = phraseLookupPosition(summary, "STRONG   economic growth");
    expect(first).toBe(second);
    expect(first).toBeGreaterThanOrEqual(0);
    expect(first).toBeLessThanOrEqual(100_000);
  });

  it("gives distinct off-text queries distinct positions (almost always)", () => {
    const positions = new Set<string>();
    for (let i = 0; i < 500; i += 1) {
      positions.add(String(phraseLookupPosition("unrelated text.", `phrase number ${i}`)));
    }
    // A handful of collisions in a 100k range would be acceptable; half
    // colliding would mean the hash is broken.
    expect(positions.size).toBeGreaterThan(490);
  });
});

describe("freeTextEntryKey", () => {
  it("is always negative so it can never collide with token-index tap keys", () => {
    expect(freeTextEntryKey("announced a new discovery")).toBeLessThan(0);
    expect(freeTextEntryKey("discovery")).toBeLessThan(0);
  });

  it("is stable for the same normalized query but distinct for different ones", () => {
    expect(freeTextEntryKey("  Talk Again ")).toBe(freeTextEntryKey("talk   again"));
    expect(freeTextEntryKey("talk again")).not.toBe(freeTextEntryKey("talk again soon"));
  });
});
