import { afterEach, describe, expect, it, vi } from "vitest";
import { checkQuizAnswer } from "./quizCheck";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("checkQuizAnswer", () => {
  it("posts the question/answer details and returns true when the server confirms it's correct", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve({ correct: true }) });
    vi.stubGlobal("fetch", fetchMock);

    await expect(checkQuizAnswer("He ___ to school.", "goes", ["walks"], "commutes")).resolves.toBe(true);
    expect(fetchMock).toHaveBeenCalledWith("api/quiz/check-answer", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        prompt: "He ___ to school.",
        answer: "goes",
        acceptableAnswers: ["walks"],
        learnerAnswer: "commutes",
      }),
    });
  });

  it("returns false when the server says the answer isn't correct", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve({ correct: false }) }));
    await expect(checkQuizAnswer("p", "a", undefined, "b")).resolves.toBe(false);
  });

  it("returns false (not null/throwing) on a non-ok response — an unconfirmed answer must grade wrong", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false }));
    await expect(checkQuizAnswer("p", "a", undefined, "b")).resolves.toBe(false);
  });

  it("returns false when fetch rejects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(checkQuizAnswer("p", "a", undefined, "b")).resolves.toBe(false);
  });
});
