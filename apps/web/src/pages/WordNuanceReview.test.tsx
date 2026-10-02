/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
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
    { lessonId: second.id, questionId: "q2" },
    { lessonId: first.id, questionId: "q3" },
  ];
  first.state.queue = ["q0", "q1", "q2", "q3"];
  second.state.queue = ["q0", "q1", "q2"];
  first.revision++; second.revision++;
  lessons = [first, second];
  vi.mocked(api.startNuanceReview).mockImplementation(async () => {
    for (const lesson of lessons) lesson.state.queue = [];
    for (const item of items) lessons.find((lesson) => lesson.id === item.lessonId)!.state.queue.push(item.questionId);
    return { items: structuredClone(items), lessons: structuredClone(lessons) };
  });
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

it("draws every question of the review batch across all lessons and shows no word-set explanation header", async () => {
  render(<WordNuanceReview />);
  expect(await screen.findByText("이번 복습 7문제 · 남은 문제 7개")).toBeInTheDocument();
  const review = screen.getByRole("region", { name: "오늘의 뉘앙스 복습" });
  const back = screen.getByRole("link", { name: "학습 목록" });
  expect(back).toHaveAttribute("href", "/nuance");
  expect(back.compareDocumentPosition(review) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(within(review).getByRole("region", { name: /상황 0/ })).toBeInTheDocument();
  expect(screen.getAllByRole("main")).toHaveLength(1);
  expect(screen.getByRole("main")).toContainElement(review);
  expect(screen.getAllByRole("banner")).toHaveLength(1);
  expect(api.startNuanceReview).toHaveBeenCalledOnce();
  expect(screen.queryByText("가격과 품질을 구분해요")).not.toBeInTheDocument();
  expect(screen.queryByText("cheap / inexpensive")).not.toBeInTheDocument();
  expect(screen.getByRole("region", { name: /상황 0/ })).toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "cheap" }));
  fireEvent.click(screen.getByRole("button", { name: "답안 확인" }));
  await screen.findByText("의도에 맞는 표현이에요");
  const progress = within(review).getByRole("group", { name: "복습 진행" });
  expect(within(progress).getByRole("button", { name: "맞혔지만 다시 복습" })).toBeEnabled();
  fireEvent.click(within(progress).getByRole("button", { name: "다음 문맥" }));
  expect(await screen.findByRole("region", { name: /다른 묶음 상황 0/ })).toBeInTheDocument();
  expect(screen.getAllByRole("main")).toHaveLength(1);
  expect(screen.getAllByRole("banner")).toHaveLength(1);
});

it("restores the persisted batch from the URL without drawing another batch", async () => {
  const params = new URLSearchParams();
  for (const item of items) params.append("item", JSON.stringify([item.lessonId, item.questionId]));
  window.history.replaceState(null, "", `/pr/14/nuance-review?${params}`);
  render(<WordNuanceReview />);
  expect(await screen.findByText("이번 복습 7문제 · 남은 문제 7개")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "학습 목록" })).toHaveAttribute("href", "/pr/14/nuance");
  expect(api.fetchNuanceLessons).toHaveBeenCalledOnce();
  expect(api.startNuanceReview).not.toHaveBeenCalled();
});

it("finishes the whole batch in one sitting and redraws every remaining due question", async () => {
  render(<WordNuanceReview />);
  await screen.findByText("이번 복습 7문제 · 남은 문제 7개");
  const answers = ["cheap", "cheap", "inexpensive", "inexpensive", "cheap", "cheap", "inexpensive"];
  for (const [index, answer] of answers.entries()) {
    fireEvent.click(await screen.findByRole("button", { name: answer }));
    fireEvent.click(screen.getByRole("button", { name: "답안 확인" }));
    await screen.findByText("의도에 맞는 표현이에요");
    fireEvent.click(screen.getByRole("button", { name: "다음 문맥" }));
    if (index === answers.length - 1) {
      expect(await screen.findByText("이번 7문제 복습을 마쳤어요")).toBeInTheDocument();
    } else {
      expect(await screen.findByText(`이번 복습 7문제 · 남은 문제 ${answers.length - 1 - index}개`)).toBeInTheDocument();
    }
  }
  // q4 of each lesson was not part of the drawn batch, so the button offers
  // to pull every still-due question into one new batch.
  const redraw = screen.getByRole("button", { name: "지금 나온 문제 모두 복습" });
  fireEvent.click(redraw);
  expect(await screen.findByText("이번 복습 7문제 · 남은 문제 7개")).toBeInTheDocument();
  expect(api.startNuanceReview).toHaveBeenCalledTimes(2);
});
