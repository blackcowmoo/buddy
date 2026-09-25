/** @vitest-environment jsdom */
import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { WordNuance } from "./WordNuance";
import * as api from "../lib/nuance";
import { nuanceFixture } from "../lib/nuance.testData";

vi.mock("../lib/nuance", async (original) => ({
  ...await original<typeof api>(),
  fetchNuanceLessons: vi.fn(), fetchNuanceLesson: vi.fn(), drawNuanceLesson: vi.fn(),
  retryNuanceLesson: vi.fn(), practiceNuance: vi.fn(), deleteNuanceLesson: vi.fn(),
}));
let lesson: api.NuanceLesson;
beforeEach(() => {
  vi.resetAllMocks();
  lesson = nuanceFixture();
  window.history.replaceState(null, "", "/nuance");
  vi.mocked(api.fetchNuanceLessons).mockImplementation(async () => [structuredClone(lesson)]);
  vi.mocked(api.fetchNuanceLesson).mockImplementation(async () => structuredClone(lesson));
});
afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); });
async function open() {
  render(<WordNuance />);
  fireEvent.click(await screen.findByRole("button", { name: "cheap / inexpensive 열기" }));
  await screen.findByRole("button", { name: "상황 연습" });
}
function respond(update: (l: api.NuanceLesson) => void) {
  const next = structuredClone(lesson); next.revision++; update(next);
  vi.mocked(api.practiceNuance).mockImplementationOnce(async () => { lesson = next; return structuredClone(next); });
  return next;
}
async function start() {
  respond((l) => { l.state.queue = ["q0", "q1"]; });
  fireEvent.click(screen.getByRole("button", { name: "상황 연습" }));
  await screen.findByRole("region", { name: /상황 0/ });
}
function answer(correct: boolean, questionId = "q0") {
  const next = respond((l) => {
    const q = l.content!.questions.find((q) => q.id === questionId)!;
    const selected = correct ? q.answer : l.content!.words.find((w) => w.word !== q.answer)!.word;
    l.state.feedback = { questionId, selected, correct };
    l.state.progress[questionId] = { stage: correct ? 1 : 0, attempts: 1, correct: correct ? 1 : 0, lastReviewedAt: 1700000000, nextReviewAt: correct ? 4102444800 : 0 };
  });
  fireEvent.click(screen.getByRole("button", { name: next.state.feedback!.selected }));
  fireEvent.click(screen.getByRole("button", { name: "답안 확인" }));
}
it("uses a dedicated stacked card layout for long history titles", async () => {
  render(<WordNuance />);
  const historyItem = await screen.findByRole("button", { name: "cheap / inexpensive 열기" });
  expect(historyItem).toHaveClass("nuance-list-item");
  expect(within(historyItem).getByText("cheap / inexpensive")).toHaveClass("title");
});
it("lists generated history, omits search and dictionary links, and opens the comparison", async () => {
  await open();
  expect(screen.queryByRole("searchbox")).not.toBeInTheDocument();
  expect(screen.queryByRole("link", { name: /사전/ })).not.toBeInTheDocument();
  expect(screen.getByText("cheap도 중립적으로 쓸 수 있어요.")).toBeInTheDocument();
  expect(screen.queryByText(/문제와 풀이 진행은 계정에 저장/)).not.toBeInTheDocument();
  expect(new URLSearchParams(window.location.search).get("lesson")).toBe(lesson.id);
  fireEvent.click(screen.getByRole("button", { name: "← 목록으로" }));
  expect(await screen.findByRole("button", { name: "＋ 새 문제 만들기" })).toBeInTheDocument();
  expect(screen.getByText("같은 뜻, 미묘하게 다른 느낌")).toBeInTheDocument();
  expect(screen.getByText(/서로 바꿔 써도 기본 뜻이 통하는 단어들을 비교해요/)).toBeInTheDocument();
  expect(screen.queryByText(/의도에 더 잘 맞는 표현을 놓친 문맥은 다시 풀고/)).not.toBeInTheDocument();
});
it("hides the translation and explanation until the server grades, then saves the forced-guess choice", async () => {
  await open(); await start();
  expect(screen.queryByText(lesson.content!.questions[0].translation)).not.toBeInTheDocument();
  const before = within(screen.getByRole("group", { name: "표현 선택" })).getAllByRole("button").map((b) => b.textContent);
  answer(true);
  expect(await screen.findByText("의도에 맞는 표현이에요")).toBeInTheDocument();
  expect(screen.getByText(lesson.content!.questions[0].translation)).toBeInTheDocument();
  expect(within(screen.getByRole("group", { name: "표현 선택" })).getAllByRole("button").map((b) => b.textContent)).toEqual(before);
  expect(api.practiceNuance).toHaveBeenLastCalledWith("lesson-1", { kind: "answer", revision: 2, questionId: "q0", selected: "cheap" });
  respond((l) => { l.state.feedback = undefined; l.state.queue = ["q1"]; });
  fireEvent.click(screen.getByRole("button", { name: "맞혔지만 다시 복습" }));
  await screen.findByRole("region", { name: /상황 1/ });
  expect(api.practiceNuance).toHaveBeenLastCalledWith("lesson-1", { kind: "next", revision: 3, repeat: true });
  expect(screen.getByRole("button", { name: "답안 확인" })).toBeDisabled();
});
it("allows revising a choice before checking it like the other quizzes", async () => {
  await open(); await start();
  const submit = screen.getByRole("button", { name: "답안 확인" });
  const cheap = screen.getByRole("button", { name: "cheap" });
  const inexpensive = screen.getByRole("button", { name: "inexpensive" });
  expect(submit).toBeDisabled();
  fireEvent.click(inexpensive);
  expect(inexpensive).toHaveAttribute("aria-pressed", "true");
  expect(inexpensive).toHaveClass("selected");
  fireEvent.click(cheap);
  expect(inexpensive).toHaveAttribute("aria-pressed", "false");
  expect(cheap).toHaveAttribute("aria-pressed", "true");
  expect(submit).toBeEnabled();
  expect(api.practiceNuance).toHaveBeenCalledTimes(1); // start only
  expect(screen.queryByRole("status")).not.toBeInTheDocument();
  expect(screen.queryByText(lesson.content!.questions[0].translation)).not.toBeInTheDocument();

  answer(true);
  await screen.findByText("의도에 맞는 표현이에요");
  expect(screen.getByRole("button", { name: "cheap" })).toHaveClass("correct");
  expect(screen.getByRole("button", { name: "cheap" })).toBeDisabled();
  expect(screen.queryByRole("button", { name: "답안 확인" })).not.toBeInTheDocument();
});
it("clears the draft when the same missed question is immediately repeated", async () => {
  await open(); await start();
  answer(false);
  await screen.findByText("이 문제는 잠시 뒤 다시 나와요.");
  expect(screen.getByRole("button", { name: "inexpensive" })).toHaveClass("incorrect");
  respond((l) => { l.state.feedback = undefined; l.state.queue = ["q0"]; });
  fireEvent.click(screen.getByRole("button", { name: "다음 문맥" }));

  expect(await screen.findByRole("button", { name: "답안 확인" })).toBeDisabled();
  for (const choice of within(screen.getByRole("group", { name: "표현 선택" })).getAllByRole("button")) {
    expect(choice).toHaveAttribute("aria-pressed", "false");
    expect(choice).not.toHaveClass("selected");
  }
});
it("repeats a missed context after another question with changed option positions", async () => {
  await open(); await start();
  const before = within(screen.getByRole("group", { name: "표현 선택" })).getAllByRole("button").map((b) => b.textContent);
  answer(false); await screen.findByText("이 문제는 잠시 뒤 다시 나와요.");
  respond((l) => { l.state.feedback = undefined; l.state.queue = ["q1", "q0"]; });
  fireEvent.click(screen.getByRole("button", { name: "다음 문맥" }));
  await screen.findByRole("region", { name: /상황 1/ });
  answer(true, "q1"); await screen.findByText("의도에 맞는 표현이에요");
  respond((l) => { l.state.feedback = undefined; l.state.queue = ["q0"]; });
  fireEvent.click(screen.getByRole("button", { name: "다음 문맥" }));
  await screen.findByRole("region", { name: /상황 0/ });
  expect(within(screen.getByRole("group", { name: "표현 선택" })).getAllByRole("button").map((b) => b.textContent)).toEqual([...before].reverse());
  expect(screen.queryByText(lesson.content!.questions[0].translation)).not.toBeInTheDocument();
});
it("resumes the saved reveal after remount with no localStorage dependency", async () => {
  await open(); await start(); answer(false);
  await screen.findByText("이 문제는 잠시 뒤 다시 나와요.");
  cleanup();
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("disabled"); });
  render(<WordNuance />);
  expect(await screen.findByText("이 문제는 잠시 뒤 다시 나와요.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "cheap" })).toBeDisabled();
});
it("keeps a failed answer on screen and restores a committed answer after a lost response", async () => {
  await open(); await start();
  vi.mocked(api.practiceNuance).mockResolvedValueOnce(null);
  fireEvent.click(screen.getByRole("button", { name: "cheap" }));
  fireEvent.click(screen.getByRole("button", { name: "답안 확인" }));
  await screen.findByRole("alert");
  expect(screen.queryByText("의도에 맞는 표현이에요")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "cheap" })).toBeEnabled();
  const saved = structuredClone(lesson); saved.revision++;
  saved.state.feedback = { questionId: "q0", selected: "cheap", correct: true };
  saved.state.progress.q0 = { stage: 1, attempts: 1, correct: 1, lastReviewedAt: 1700000000, nextReviewAt: 4102444800 };
  vi.mocked(api.practiceNuance).mockResolvedValueOnce(null);
  vi.mocked(api.fetchNuanceLesson).mockResolvedValueOnce(saved);
  fireEvent.click(screen.getByRole("button", { name: "cheap" }));
  fireEvent.click(screen.getByRole("button", { name: "답안 확인" }));
  expect(await screen.findByText("의도에 맞는 표현이에요")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "cheap" })).toBeDisabled();
});
it("starts generation immediately and polls persisted status while back in the list", async () => {
  vi.useFakeTimers();
  const pending: api.NuanceLesson = { ...nuanceFixture(), content: undefined, status: "pending", revision: 0 };
  vi.mocked(api.fetchNuanceLessons).mockResolvedValueOnce([]);
  vi.mocked(api.drawNuanceLesson).mockResolvedValueOnce(pending);
  await act(async () => { render(<WordNuance />); });
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "＋ 새 문제 만들기" })); });
  expect(screen.getByText("새 비교 묶음을 만들고 있어요")).toBeInTheDocument();
  vi.mocked(api.fetchNuanceLessons).mockResolvedValueOnce([pending]);
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "← 목록으로" })); });
  expect(screen.getByRole("button", { name: "새 비교 묶음을 만드는 중… 열기" })).toBeInTheDocument();
  vi.mocked(api.fetchNuanceLessons).mockResolvedValueOnce([lesson]);
  await act(async () => { await vi.advanceTimersByTimeAsync(2500); });
  expect(screen.getByRole("button", { name: "cheap / inexpensive 열기" })).toBeInTheDocument();
});
it("shows generation failure and allows retry of the same lesson", async () => {
  lesson.status = "failed"; lesson.content = undefined;
  render(<WordNuance />);
  fireEvent.click(await screen.findByRole("button", { name: "문제 생성 실패 열기" }));
  const retry = await screen.findByRole("button", { name: "생성 다시 시도" });
  vi.mocked(api.retryNuanceLesson).mockResolvedValueOnce({ ...lesson, status: "pending" });
  fireEvent.click(retry);
  expect(await screen.findByText("새 비교 묶음을 만들고 있어요")).toBeInTheDocument();
  expect(api.retryNuanceLesson).toHaveBeenCalledWith("lesson-1");
});
it("distinguishes load failure from empty history and surfaces deletion failures", async () => {
  vi.mocked(api.fetchNuanceLessons).mockResolvedValueOnce(null);
  render(<WordNuance />);
  await screen.findByRole("alert");
  expect(screen.queryByText(/아직 만든 문제가 없어요/)).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "목록 다시 불러오기" }));
  const del = await screen.findByRole("button", { name: "cheap / inexpensive 삭제" });
  vi.spyOn(window, "confirm").mockReturnValue(true);
  vi.mocked(api.deleteNuanceLesson).mockResolvedValueOnce(false);
  fireEvent.click(del);
  expect(await screen.findByText("삭제하지 못했어요. 다시 시도해 주세요.")).toBeInTheDocument();
  expect(del).toBeInTheDocument();
  vi.mocked(api.deleteNuanceLesson).mockResolvedValueOnce(true);
  fireEvent.click(del);
  await waitFor(() => expect(screen.queryByRole("button", { name: "cheap / inexpensive 삭제" })).not.toBeInTheDocument());
});
it("sends list review to the separate all-problem review page", async () => {
  render(<WordNuance />);
  const review = await screen.findByRole("link", { name: "복습 시작" });
  expect(review).toHaveAttribute("href", "/nuance-review");
  expect(api.practiceNuance).not.toHaveBeenCalled();
});
it("locks answer submission while saving so a double tap sends one attempt", async () => {
  await open(); await start();
  let resolve!: (value: api.NuanceLesson | null) => void;
  vi.mocked(api.practiceNuance).mockReturnValueOnce(new Promise((done) => { resolve = done; }));
  const choice = screen.getByRole("button", { name: "cheap" });
  fireEvent.click(choice);
  const submit = screen.getByRole("button", { name: "답안 확인" });
  fireEvent.click(submit); fireEvent.click(submit);
  expect(api.practiceNuance).toHaveBeenCalledTimes(2); // start, answer
  expect(choice).toBeDisabled();
  await act(async () => { resolve(null); });
  expect(choice).toBeEnabled();
});
it("follows browser history back and forward to a saved lesson", async () => {
  await open();
  await act(async () => { window.history.replaceState(null, "", "/nuance"); window.dispatchEvent(new PopStateEvent("popstate")); });
  expect(screen.getByRole("button", { name: "＋ 새 문제 만들기" })).toBeInTheDocument();
  await act(async () => { window.history.replaceState(null, "", "/nuance?lesson=lesson-1"); window.dispatchEvent(new PopStateEvent("popstate")); });
  expect(screen.getByRole("button", { name: "상황 연습" })).toBeInTheDocument();
});
