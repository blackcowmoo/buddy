import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { WordReviewItem, WordReviewQuestion } from "./wordReview";
import { buildReviewQueue, currentQuestion, reshuffleRecognitionChoices, type QuizItem } from "./wordReviewQuiz";

function word(id: string, changes: Partial<WordReviewItem> = {}): WordReviewItem {
  return {
    id, word: id, meaning: `${id}의 뜻`, example: `This is ${id}.`,
    status: "verified", researchStatus: "confirmed", stage: 0, reviewCount: 0, nextReviewAt: 100,
    reviewQuestion: { version: 3, prompt: "This is ___.", answers: [id] },
    ...changes,
  };
}

beforeEach(() => { vi.spyOn(Math, "random").mockReturnValue(0); });
afterEach(() => { vi.restoreAllMocks(); });

describe("review question compatibility", () => {
  it.each([
    undefined,
    // Version 0 is the "no real question yet" legacy marker — never solvable.
    { version: 0, prompt: "This is ___.", answers: ["none"] },
    { version: 3, prompt: "This is ___." },
    { version: 3, prompt: "This is ___.", answers: [] },
    { version: 3, prompt: "This is ___ and ___.", answers: ["one"] },
    { version: 3, prompt: "This is ___.", answers: [" "] },
  ])("defers an incomplete question: %j", (question) => {
    expect(currentQuestion(word("example", { reviewQuestion: question as WordReviewQuestion | undefined }))).toBeNull();
  });

  // The server keeps serving an older-contract question while it regenerates
  // one under the current contract in the background, so any complete older
  // question must stay answerable — a version bump never interrupts the
  // question the learner is solving.
  it.each([
    { version: 1, prompt: "This is ___.", answers: ["old"] },
    { version: 2, prompt: "He ___ home.", answers: ["goes"] },
    { version: 3, prompt: "They ___ their ___ yesterday.", answers: ["did", "best"] },
  ])("keeps an older complete question answerable during an upgrade: %j", (question) => {
    expect(currentQuestion(word("example", { reviewQuestion: question }))).toBe(question);
  });

  it("keeps complete grammatical answers and supports multiple blanks and future versions", () => {
    const question = { version: 4, prompt: "They ___ their ___ yesterday.", answers: ["did", "best"] };
    expect(currentQuestion(word("do one's best", { reviewQuestion: question }))).toBe(question);
  });
});

describe("review queue", () => {
  it("includes only due, confirmed words with solvable questions without mutating the source", () => {
    const source = [
      word("due"), word("overdue", { nextReviewAt: 99 }), word("future", { nextReviewAt: 101 }),
      word("pending", { status: "pending" }), word("rejected", { status: "rejected" }),
      word("unconfirmed", { researchStatus: "done" }), word("legacy", { reviewQuestion: undefined }),
      // An older-version question awaiting its background upgrade is due too.
      word("upgrading", { reviewQuestion: { version: 2, prompt: "He ___ home.", answers: ["goes"] } }),
    ];
    const original = structuredClone(source);
    const queue = buildReviewQueue(source, 100);
    expect(queue.map(({ word }) => word.id).sort()).toEqual(["due", "overdue", "upgrading"]);
    expect(queue.every((item) => item.mode === "recall")).toBe(true);
    expect(source).toEqual(original);
  });

  it("draws eight recognition options from confirmed vocabulary, including words not yet due", () => {
    const source = Array.from({ length: 8 }, (_, index) => word(`word-${index}`, { nextReviewAt: index ? 200 : 100 }));
    source.push(word("excluded", { researchStatus: "done" }));
    for (const meaningStatus of ["pending", "done", "failed"] as const) {
      source.push(word(meaningStatus, { meaningStatus }));
    }
    const queue = buildReviewQueue(source, 100);
    expect(queue).toHaveLength(1);
    const item = queue[0];
    expect(item.mode).toBe("recognition");
    if (item.mode !== "recognition") throw new Error("Expected recognition options");
    expect(item.choices).toHaveLength(8);
    expect([...item.choices].sort()).toEqual(source.slice(0, 8).map(({ meaning }) => meaning).sort());
  });

  it("withholds meanings under cleanup or awaiting a decision until explicitly confirmed", () => {
    const source = ["pending", "done", "failed", "confirmed"].map((status) => word(status, { meaningStatus: status as WordReviewItem["meaningStatus"] }));
    expect(buildReviewQueue(source, 100).map((item) => item.word.id)).toEqual(["confirmed"]);
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
