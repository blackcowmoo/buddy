import { describe, expect, it } from "vitest";
import { floatTo16 } from "./recorder";

describe("floatTo16", () => {
  it("maps 0 to 0", () => {
    expect(floatTo16(new Float32Array([0]))[0]).toBe(0);
  });

  it("maps 1 to the max positive s16 value", () => {
    expect(floatTo16(new Float32Array([1]))[0]).toBe(0x7fff);
  });

  it("maps -1 to the max negative s16 value", () => {
    expect(floatTo16(new Float32Array([-1]))[0]).toBe(-0x8000);
  });

  it("clamps values outside [-1, 1]", () => {
    const out = floatTo16(new Float32Array([2, -2]));
    expect(out[0]).toBe(0x7fff);
    expect(out[1]).toBe(-0x8000);
  });

  it("preserves length and ordering", () => {
    const out = floatTo16(new Float32Array([0.5, -0.5, 0]));
    expect(out.length).toBe(3);
    expect(out[0]).toBeGreaterThan(0);
    expect(out[1]).toBeLessThan(0);
    expect(out[2]).toBe(0);
  });
});
