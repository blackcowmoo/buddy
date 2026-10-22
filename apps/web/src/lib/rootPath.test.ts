import { describe, expect, it } from "vitest";
import { currentPrNumber, prPath } from "./rootPath";

describe("prPath", () => {
  it("maps a bare PR number to /pr/<n>/", () => {
    expect(prPath("14")).toBe("/pr/14/");
  });

  it("trims surrounding whitespace", () => {
    expect(prPath("  14  ")).toBe("/pr/14/");
  });

  it("maps an empty input back to root", () => {
    expect(prPath("")).toBe("/");
    expect(prPath("   ")).toBe("/");
  });

  it("rejects non-numeric input", () => {
    expect(prPath("abc")).toBeNull();
    expect(prPath("14x")).toBeNull();
    expect(prPath("-1")).toBeNull();
    expect(prPath("/pr/14")).toBeNull();
  });
});

describe("currentPrNumber", () => {
  it("reads the PR number from the deployment prefix", () => {
    expect(currentPrNumber("/pr/14/")).toBe("14");
    expect(currentPrNumber("/pr/14")).toBe("14");
  });

  it("reads it from any page under the prefix", () => {
    expect(currentPrNumber("/pr/14/words")).toBe("14");
    expect(currentPrNumber("/pr/14/nuance-review")).toBe("14");
  });

  it("returns null on the root deployment", () => {
    expect(currentPrNumber("/")).toBeNull();
    expect(currentPrNumber("/recordings")).toBeNull();
  });

  it("returns null unless the leading /pr/<n> segment is a bare number", () => {
    expect(currentPrNumber("/pr/")).toBeNull();
    expect(currentPrNumber("/pr/14x/")).toBeNull();
    expect(currentPrNumber("/pr/14x")).toBeNull();
    expect(currentPrNumber("/foo/pr/14/")).toBeNull();
  });
});
