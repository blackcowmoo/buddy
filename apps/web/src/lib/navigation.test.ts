import { describe, expect, it } from "vitest";
import { learningDestinationForPage, learningDestinations } from "./navigation";
import type { Page } from "./route";

describe("learning navigation", () => {
  it("keeps a single stable order for every learning destination", () => {
    expect(learningDestinations.map(({ href }) => href)).toEqual([
      "recordings", "instant", "words", "match", "nuance", "article", "writing",
    ]);
  });

  it.each<Page>(["recordings", "instant", "words", "match", "nuance", "article", "writing"])("finds the destination for %s", (page) => {
    expect(learningDestinationForPage(page)?.href).toBe(page);
  });

  it("keeps nuance review within the nuance destination", () => {
    expect(learningDestinationForPage("nuance-review")?.href).toBe("nuance");
  });

  it("does not select a learning destination on the conversation page", () => {
    expect(learningDestinationForPage("chat")).toBeUndefined();
  });
});
