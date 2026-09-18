/** @vitest-environment jsdom */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { loadNuanceAnswers, nuanceLessons, saveNuanceAnswers } from "./nuance";

beforeEach(() => localStorage.clear());
afterEach(() => { vi.restoreAllMocks(); localStorage.clear(); });

describe("nuance progress", () => {
  it("starts empty and round-trips answers", () => {
    expect(loadNuanceAnswers()).toEqual({});
    expect(saveNuanceAnswers({ "price-quality": "cheap" })).toBe(true);
    expect(loadNuanceAnswers()).toEqual({ "price-quality": "cheap" });
  });

  it.each(["{invalid", "null", "[]", "42"])("ignores malformed progress: %s", (raw) => {
    localStorage.setItem("buddy.nuance.answers.v1", raw);
    expect(loadNuanceAnswers()).toEqual({});
  });

  it("drops stale question IDs and invalid choices but preserves valid answers", () => {
    localStorage.setItem("buddy.nuance.answers.v1", JSON.stringify({
      obsolete: "cheap", "price-quality": "curious", "price-recommend": "inexpensive", "child-wonder": 42,
    }));
    expect(loadNuanceAnswers()).toEqual({ "price-recommend": "inexpensive" });
  });

  it("handles denied storage reads and writes", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("denied"); });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("denied"); });
    expect(loadNuanceAnswers()).toEqual({});
    expect(saveNuanceAnswers({ "price-quality": "cheap" })).toBe(false);
  });
});

it("keeps curriculum IDs unique and gives every word a translated example and an answerable context", () => {
  expect(new Set(nuanceLessons.map((lesson) => lesson.id)).size).toBe(nuanceLessons.length);
  const questions = nuanceLessons.flatMap((lesson) => lesson.questions);
  expect(new Set(questions.map((q) => q.id)).size).toBe(questions.length);
  for (const lesson of nuanceLessons) {
    const words = lesson.words.map((w) => w.word);
    expect(new Set(words).size).toBe(words.length);
    for (const word of lesson.words) {
      expect(word.example).toContain(word.word);
      expect(word.translation).toMatch(/[가-힣]/);
      expect(lesson.questions.some((q) => q.answer === word.word)).toBe(true);
    }
    for (const q of lesson.questions) {
      expect(words).toContain(q.answer);
      expect(q.sentence.split("____")).toHaveLength(2);
      expect(q.translation).toMatch(/[가-힣]/);
      for (const word of words) expect(q.explanation).toContain(word);
    }
  }
});
