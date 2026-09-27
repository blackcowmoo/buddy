import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { WordReviewItem, WordReviewQuestion } from "./wordReview";
import { buildReviewQueue, currentQuestion, reshuffleRecognitionChoices, type QuizItem } from "./wordReviewQuiz";

function word(id: string, changes: Partial<WordReviewItem> = {}): WordReviewItem {
  return {
    id, word: id, meaning: `${id}의 뜻`, example: `This is ${id}.`,
    status: "verified", researchStatus: "confirmed", stage: 0, reviewCount: 0, nextReviewAt: 100,
    reviewQuestion: { version: 2, prompt: "This is ___.", answers: [id] },
    ...changes,
  };
}

beforeEach(() => { vi.spyOn(Math, "random").mockReturnValue(0); });
afterEach(() => { vi.restoreAllMocks(); });

describe("review question compatibility", () => {
  it.each([
    undefined,
    { version: 1, prompt: "This is ___.", answers: ["old"] },
    { version: 2, prompt: "This is ___." },
    { version: 2, prompt: "This is ___.", answers: [] },
    { version: 2, prompt: "This is ___ and ___.", answers: ["one"] },
    { version: 2, prompt: "This is ___.", answers: [" "] },
  ])("defers an incomplete or legacy question: %j", (question) => {
    expect(currentQuestion(word("example", { reviewQuestion: question as WordReviewQuestion | undefined }))).toBeNull();
  });

  it("keeps complete grammatical answers and supports multiple blanks and future versions", () => {
    const question = { version: 3, prompt: "They ___ their ___ yesterday.", answers: ["did", "best"] };
    expect(currentQuestion(word("do one's best", { reviewQuestion: question }))).toBe(question);
  });
});

describe("review queue", () => {
  it("includes only due, confirmed words with current questions without mutating the source", () => {
    const source = [
      word("due"), word("overdue", { nextReviewAt: 99 }), word("future", { nextReviewAt: 101 }),
      word("pending", { status: "pending" }), word("rejected", { status: "rejected" }),
      word("unconfirmed", { researchStatus: "done" }), word("legacy", { reviewQuestion: undefined }),
    ];
    const original = structuredClone(source);
    const queue = buildReviewQueue(source, 100);
    expect(queue.map(({ word }) => word.id).sort()).toEqual(["due", "overdue"]);
    expect(queue.every((item) => item.mode === "recall")).toBe(true);
    expect(source).toEqual(original);
  });

  it("draws eight recognition options from confirmed vocabulary, including words not yet due", () => {
    const source = Array.from({ length: 8 }, (_, index) => word(`word-${index}`, { nextReviewAt: index ? 200 : 100 }));
    source.push(word("excluded", { researchStatus: "done" }));
    const queue = buildReviewQueue(source, 100);
    expect(queue).toHaveLength(1);
    const item = queue[0];
    expect(item.mode).toBe("recognition");
    if (item.mode !== "recognition") throw new Error("Expected recognition options");
    expect(item.choices).toHaveLength(8);
    expect([...item.choices].sort()).toEqual(source.slice(0, 8).map(({ meaning }) => meaning).sort());
  });

  it("can still choose recall when enough recognition options exist", () => {
    vi.mocked(Math.random).mockReturnValue(0.75);
    const source = Array.from({ length: 8 }, (_, index) => word(`word-${index}`));
    const queue = buildReviewQueue(source, 100);
    expect(queue).toHaveLength(8);
    expect(queue.every((item) => item.mode === "recall")).toBe(true);
  });
});

describe("recognition retries", () => {
  it("changes an unchanged shuffle without mutating the current question or losing choices", () => {
    vi.mocked(Math.random).mockReturnValue(0.999);
    const item: QuizItem = { word: word("one"), mode: "recognition", choices: ["first", "second", "third"] };
    const retry = reshuffleRecognitionChoices(item);
    expect(retry).toEqual({ ...item, choices: ["second", "first", "third"] });
    expect(item.choices).toEqual(["first", "second", "third"]);
  });

  it("preserves recall and single-option questions", () => {
    const recall: QuizItem = { word: word("one"), mode: "recall" };
    const single: QuizItem = { word: recall.word, mode: "recognition", choices: ["one"] };
    expect(reshuffleRecognitionChoices(recall)).toBe(recall);
    expect(reshuffleRecognitionChoices(single)).toBe(single);
  });
});
