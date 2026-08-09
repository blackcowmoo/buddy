/**
 * @vitest-environment jsdom
 */
import { beforeEach, describe, expect, it } from "vitest";
import { NATIVE_RATE, isValidRate, loadPlaybackRate, savePlaybackRate } from "./ttsSettings";

beforeEach(() => {
  localStorage.clear();
});

describe("loadPlaybackRate", () => {
  it("defaults to the native rate when nothing is stored", () => {
    expect(loadPlaybackRate()).toBe(NATIVE_RATE);
  });

  it("returns a previously saved rate", () => {
    savePlaybackRate(0.7);
    expect(loadPlaybackRate()).toBe(0.7);
  });

  it("falls back to the default on corrupt JSON", () => {
    localStorage.setItem("buddy.tts.playbackRate", "{not json");
    expect(loadPlaybackRate()).toBe(NATIVE_RATE);
  });

  it("falls back to the default for an out-of-range stored value", () => {
    localStorage.setItem("buddy.tts.playbackRate", JSON.stringify(10));
    expect(loadPlaybackRate()).toBe(NATIVE_RATE);
  });
});

describe("savePlaybackRate", () => {
  it("persists a valid rate", () => {
    savePlaybackRate(1.25);
    expect(loadPlaybackRate()).toBe(1.25);
  });

  it("falls back to the default instead of persisting an out-of-range value", () => {
    savePlaybackRate(10);
    expect(loadPlaybackRate()).toBe(NATIVE_RATE);
  });
});

describe("isValidRate", () => {
  it("accepts values within [0.5, 2]", () => {
    expect(isValidRate(0.5)).toBe(true);
    expect(isValidRate(2)).toBe(true);
    expect(isValidRate(1)).toBe(true);
  });

  it("rejects out-of-range or non-numeric values", () => {
    expect(isValidRate(0.4)).toBe(false);
    expect(isValidRate(2.1)).toBe(false);
    expect(isValidRate("1")).toBe(false);
    expect(isValidRate(NaN)).toBe(false);
  });
});
