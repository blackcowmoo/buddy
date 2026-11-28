/** @vitest-environment jsdom */
import { afterEach, expect, it, vi } from "vitest";
import { canRepeatQuestion, deleteNuanceLesson, drawNuanceLesson, dueQuestions, fetchNuanceLesson, fetchNuanceLessons, nextReview, nuanceOptions, practiceNuance, retryNuanceLesson, startNuanceReview } from "./nuance";
import { nuanceFixture } from "./nuance.testData";

afterEach(() => vi.restoreAllMocks());
it("uses account API endpoints and sends the progress revision, never a client verdict", async () => {
  const lesson = nuanceFixture();
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue({ ok: true, json: async () => lesson } as Response);
  expect(await drawNuanceLesson()).toEqual(lesson);
  expect(fetch).toHaveBeenLastCalledWith("api/nuance/draw", expect.objectContaining({ method: "POST" }));
  await startNuanceReview();
  expect(fetch).toHaveBeenLastCalledWith("api/nuance/review", expect.objectContaining({ method: "POST" }));
  await fetchNuanceLesson("a/b");
  expect(fetch).toHaveBeenLastCalledWith("api/nuance/a%2Fb", undefined);
  const action = { kind: "answer" as const, revision: 3, questionId: "q0", selected: "cheap" };
  await practiceNuance("a/b", action);
  expect(fetch).toHaveBeenLastCalledWith("api/nuance/a%2Fb/practice", expect.objectContaining({ body: JSON.stringify(action) }));
  await retryNuanceLesson("a/b");
  expect(fetch).toHaveBeenLastCalledWith("api/nuance/a%2Fb/retry", expect.objectContaining({ method: "POST" }));
  expect(await deleteNuanceLesson("a/b")).toBe(true);
  expect(fetch).toHaveBeenLastCalledWith("api/nuance/a%2Fb", { method: "DELETE" });
});
it.each(["http", "network", "json"])("keeps %s failures distinct from an empty history", async (failure) => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
    if (failure === "network") throw new Error("offline");
    return { ok: failure !== "http", json: async () => { throw new Error("invalid"); } } as unknown as Response;
  });
  expect(await fetchNuanceLessons()).toBeNull();
  expect(await fetchNuanceLesson("a")).toBeNull();
  expect(await practiceNuance("a", { kind: "start", revision: 1 })).toBeNull();
});
it("counts due questions at the scheduled boundary and keeps option order stable until retry", () => {
  const l = nuanceFixture();
  expect(dueQuestions(l, 100)).toBe(5);
  l.state.progress.q0 = { stage: 1, attempts: 1, correct: 1, lastReviewedAt: 100, nextReviewAt: 200 };
  expect(dueQuestions(l, 199)).toBe(4);
  expect(dueQuestions(l, 200)).toBe(5);
  expect(nextReview(l)).toBe(0);
  const options = nuanceOptions(l, "q0", 0);
  expect(nuanceOptions(structuredClone(l), "q0", 0)).toEqual(options);
  expect(nuanceOptions(l, "q0", 1)).toEqual([...options].reverse());
  expect(l.content!.words.map((w) => w.word)).toEqual(["cheap", "inexpensive"]);
});
// Repeating a correct context only softens a stage the answer would have
// earned, so the shared rule lives here: stage 0 going in (first attempt, or a
// miss that already reset progress) and a wrong answer both withhold it.
it("offers repeat only for a correct answer that had progress to hold back", () => {
  const l = nuanceFixture();
  expect(canRepeatQuestion(l)).toBe(false); // nothing graded yet
  l.state.feedback = { questionId: "q0", selected: "cheap", correct: false, previousStage: 3 };
  expect(canRepeatQuestion(l)).toBe(false);
  l.state.feedback = { questionId: "q0", selected: "cheap", correct: true };
  expect(canRepeatQuestion(l)).toBe(false); // first-ever attempt
  l.state.feedback = { questionId: "q0", selected: "cheap", correct: true, previousStage: 0 };
  expect(canRepeatQuestion(l)).toBe(false); // progress already reset by a miss
  l.state.feedback = { questionId: "q0", selected: "cheap", correct: true, previousStage: 2 };
  expect(canRepeatQuestion(l)).toBe(true);
});
it("sends the requested word pair only when the learner chose one", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue({ ok: true, json: async () => null } as Response);
  await drawNuanceLesson();
  expect(JSON.parse(String(fetch.mock.lastCall?.[1]?.body))).toEqual({});
  await drawNuanceLesson(["cheap", "inexpensive"]);
  expect(fetch).toHaveBeenLastCalledWith("api/nuance/draw", expect.objectContaining({ method: "POST" }));
  expect(JSON.parse(String(fetch.mock.lastCall?.[1]?.body))).toEqual({ words: ["cheap", "inexpensive"] });
});
