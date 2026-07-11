/**
 * @vitest-environment jsdom
 */
import { beforeEach, describe, expect, it } from "vitest";
import { MAX_EXTRA_RATES, NATIVE_RATE, loadExtraRates, saveExtraRates } from "./ttsSettings";

beforeEach(() => {
  localStorage.clear();
});

describe("loadExtraRates", () => {
  it("returns the default speeds when nothing is stored", () => {
    expect(loadExtraRates()).toEqual([0.5, 0.8]);
  });

  it("returns a previously saved list", () => {
    saveExtraRates([0.6, 1.4]);
    expect(loadExtraRates()).toEqual([0.6, 1.4]);
  });

  it("falls back to defaults on corrupt JSON", () => {
    localStorage.setItem("buddy.tts.extraRates", "{not json");
    expect(loadExtraRates()).toEqual([0.5, 0.8]);
  });

  it("falls back to defaults when the stored value isn't an array", () => {
    localStorage.setItem("buddy.tts.extraRates", JSON.stringify({ rate: 0.5 }));
    expect(loadExtraRates()).toEqual([0.5, 0.8]);
  });

  it("drops the native rate, out-of-range values, and duplicates", () => {
    localStorage.setItem(
      "buddy.tts.extraRates",
      JSON.stringify([NATIVE_RATE, 0.5, 0.5, 10, -1, "0.7"]),
    );
    expect(loadExtraRates()).toEqual([0.5]);
  });

  it("caps the stored list at MAX_EXTRA_RATES", () => {
    localStorage.setItem("buddy.tts.extraRates", JSON.stringify([0.5, 0.6, 0.7]));
    const rates = loadExtraRates();
    expect(rates).toHaveLength(MAX_EXTRA_RATES);
    expect(rates).toEqual([0.5, 0.6]);
  });

  it("allows an empty list (all extra speeds removed)", () => {
    saveExtraRates([]);
    expect(loadExtraRates()).toEqual([]);
  });
});

describe("saveExtraRates", () => {
  it("dedupes, filters, and caps before persisting", () => {
    saveExtraRates([0.5, 0.5, NATIVE_RATE, 1.9, 1.9, 0.1]);
    expect(loadExtraRates()).toEqual([0.5, 1.9]);
  });
});
