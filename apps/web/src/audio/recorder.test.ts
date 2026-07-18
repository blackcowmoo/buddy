import { beforeEach, describe, expect, it, vi } from "vitest";
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

const runMock = vi.fn();
const newMock = vi.fn();

vi.mock("@ricky0123/vad-web", () => ({
  NonRealTimeVAD: { new: (...args: unknown[]) => newMock(...args) },
}));

async function* segments(...chunks: Float32Array[]) {
  for (const audio of chunks) yield { audio, start: 0, end: 0 };
}

describe("trimSilence", () => {
  beforeEach(() => {
    vi.resetModules();
    runMock.mockReset();
    newMock.mockReset();
    newMock.mockImplementation(async () => ({ run: runMock }));
  });

  it("skips loading the VAD model entirely for empty audio", async () => {
    const { trimSilence } = await import("./recorder");
    const audio = new Float32Array(0);
    const out = await trimSilence(audio);
    expect(out).toBe(audio);
    expect(newMock).not.toHaveBeenCalled();
  });

  it("concatenates the detected speech segments, dropping silence between them", async () => {
    runMock.mockReturnValue(segments(new Float32Array([1, 2]), new Float32Array([3, 4, 5])));
    const { trimSilence } = await import("./recorder");
    const out = await trimSilence(new Float32Array([0, 1, 2, 0, 0, 3, 4, 5, 0]));
    expect(Array.from(out)).toEqual([1, 2, 3, 4, 5]);
  });

  it("falls back to the original audio when no speech is detected", async () => {
    runMock.mockReturnValue(segments());
    const { trimSilence } = await import("./recorder");
    const audio = new Float32Array([0, 0, 0]);
    const out = await trimSilence(audio);
    expect(out).toBe(audio);
  });

  it("falls back to the original audio when the VAD model fails to load, and retries next time", async () => {
    newMock.mockRejectedValueOnce(new Error("model fetch failed"));
    const { trimSilence } = await import("./recorder");
    const audio = new Float32Array([1, 2, 3]);
    const out = await trimSilence(audio);
    expect(out).toBe(audio);

    runMock.mockReturnValue(segments(new Float32Array([1, 2, 3])));
    await trimSilence(audio);
    expect(newMock).toHaveBeenCalledTimes(2); // failed load isn't cached forever
  });

  it("loads the VAD model once and reuses it across calls", async () => {
    runMock.mockReturnValue(segments(new Float32Array([1])));
    const { trimSilence } = await import("./recorder");
    await trimSilence(new Float32Array([1]));
    runMock.mockReturnValue(segments(new Float32Array([1])));
    await trimSilence(new Float32Array([1]));
    expect(newMock).toHaveBeenCalledTimes(1);
  });
});
