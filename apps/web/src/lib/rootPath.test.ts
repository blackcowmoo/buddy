import { describe, expect, it } from "vitest";
import { prPath } from "./rootPath";

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
