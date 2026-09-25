/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import * as api from "../lib/nuance";
import { nuanceFixture } from "../lib/nuance.testData";
import { WordNuanceReview } from "./WordNuanceReview";

vi.mock("../lib/nuance", async (original) => ({
  ...await original<typeof api>(),
  fetchNuanceLessons: vi.fn(), fetchNuanceLesson: vi.fn(), practiceNuance: vi.fn(), startNuanceReview: vi.fn(),
}));

let lessons: api.NuanceLesson[];
let items: api.NuanceReviewItem[];

beforeEach(() => {
  vi.resetAllMocks();
  window.history.replaceState(null, "", "/nuance-review");
  const first = nuanceFixture();
  const second = structuredClone(first);
  second.id = "lesson-2";
  for (const question of second.content!.questions) question.context = `다른 묶음 ${question.context}`;
  items = [
    { lessonId: first.id, questionId: "q0" },
    { lessonId: second.id, questionId: "q0" },
    { lessonId: first.id, questionId: "q1" },
    { lessonId: second.id, questionId: "q1" },
    { lessonId: first.id, questionId: "q2" },
  ];
  first.state.queue = ["q0", "q1", "q2"];
  second.state.queue = ["q0", "q1"];
  first.revision++; second.revision++;
  lessons = [first, second];
  vi.mocked(api.startNuanceReview).mockImplementation(async () => ({ items: structuredClone(items), lessons: structuredClone(lessons) }));
  vi.mocked(api.fetchNuanceLessons).mockImplementation(async () => structuredClone(lessons));
  vi.mocked(api.fetchNuanceLesson).mockImplementation(async (id) => structuredClone(lessons.find((lesson) => lesson.id === id)!));
  vi.mocked(api.practiceNuance).mockImplementation(async (id, action) => {
    const lesson = lessons.find((candidate) => candidate.id === id)!;
    lesson.revision++;
    if (action.kind === "answer") {
      const question = lesson.content!.questions.find((candidate) => candidate.id === action.questionId)!;
      const correct = action.selected === question.answer;
      lesson.state.feedback = { questionId: question.id, selected: action.selected!, correct };
      lesson.state.progress[question.id] = { stage: correct ? 1 : 0, attempts: 1, correct: correct ? 1 : 0, lastReviewedAt: 1700000000, nextReviewAt: correct ? 4102444800 : 0 };
    } else if (action.kind === "next") {
      const feedback = lesson.state.feedback!;
      lesson.state.queue.shift();
      if (!feedback.correct) lesson.state.queue.push(feedback.questionId);
      lesson.state.feedback = undefined;
    }
    return structuredClone(lesson);
  });
});

afterEach(cleanup);

it("draws five questions across all lessons and shows no word-set explanation header", async () => {
  render(<WordNuanceReview />);
  expect(await screen.findByText("이번 복습 5문제 · 남은 문제 5개")).toBeInTheDocument();
  expect(api.startNuanceReview).toHaveBeenCalledOnce();
  expect(screen.queryByText("가격과 품질을 구분해요")).not.toBeInTheDocument();
  expect(screen.queryByText("cheap / inexpensive")).not.toBeInTheDocument();
  expect(screen.getByRole("region", { name: /상황 0/ })).toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "cheap" }));
  fireEvent.click(screen.getByRole("button", { name: "답안 확인" }));
  await screen.findByText("의도에 맞는 표현이에요");
  fireEvent.click(screen.getByRole("button", { name: "다음 문맥" }));
  expect(await screen.findByRole("region", { name: /다른 묶음 상황 0/ })).toBeInTheDocument();
});

it("restores the persisted batch from the URL without drawing another batch", async () => {
  const params = new URLSearchParams();
  for (const item of items) params.append("item", JSON.stringify([item.lessonId, item.questionId]));
  window.history.replaceState(null, "", `/pr/14/nuance-review?${params}`);
  render(<WordNuanceReview />);
  expect(await screen.findByText("이번 복습 5문제 · 남은 문제 5개")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "← 학습 목록" })).toHaveAttribute("href", "/pr/14/nuance");
  expect(api.fetchNuanceLessons).toHaveBeenCalledOnce();
  expect(api.startNuanceReview).not.toHaveBeenCalled();
});
