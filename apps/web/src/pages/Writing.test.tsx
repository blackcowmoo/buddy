/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen, within, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../lib/writing", () => ({
  fetchWritingPrompts: vi.fn(),
  fetchWritingPrompt: vi.fn(),
  drawWritingPrompt: vi.fn(),
  checkWriting: vi.fn(),
  recheckWriting: vi.fn(),
  deleteWritingPrompt: vi.fn(),
}));
vi.mock("../lib/wordSearch", () => ({ suggestWords: vi.fn() }));
vi.mock("../lib/wordReview", () => ({ saveWord: vi.fn() }));

import { Writing } from "./Writing";
import "../styles.css";
import {
  checkWriting,
  deleteWritingPrompt,
  drawWritingPrompt,
  fetchWritingPrompt,
  fetchWritingPrompts,
  recheckWriting,
  type WritingPrompt,
} from "../lib/writing";
import { suggestWords } from "../lib/wordSearch";
import { formatDateDivider } from "../lib/time";

const oldPrompt = { id: "p1", korean: "어제 영화를 봤어요.", status: "done" as const, createdAt: 1700000000 };
const newPrompt = { id: "p2", korean: "오늘은 책을 읽어요.", status: "done" as const, createdAt: 1700000100 };

// The persisted grade shape the server returns once the detached check job
// finishes: the stored answer plus corrected/issues on the prompt itself.
const graded = (over: Partial<WritingPrompt> = {}): WritingPrompt => ({
  ...oldPrompt,
  answer: "I watched movie yesterday.",
  checkStatus: "done",
  corrected: "I watched a movie yesterday.",
  issues: [
    {
      type: "grammar",
      span: "watched movie",
      suggestion: "watched a movie",
      explanation: "Countable nouns need an article.",
      explanationTranslation: "셀 수 있는 명사 앞에는 관사가 필요해요.",
    },
  ],
  ...over,
});

// Real-timer tests: poll until the DOM condition holds.
async function flushUntil(finder: () => unknown) {
  await waitFor(finder);
}

// Fake-timer tests must not use userEvent (its internal delays never
// elapse on their own) — see App.test.tsx's identical tick loop: every
// advance needs act(), and the loop's exit condition is the real one.
async function tickUntil(check: () => boolean, maxTicks = 50) {
  for (let i = 0; i < maxTicks; i++) {
    if (check()) return;
    await act(() => vi.advanceTimersByTimeAsync(0));
  }
  throw new Error(`tickUntil: condition still false after ${maxTicks} ticks`);
}

beforeEach(() => {
  // Mock call history persists across tests here (no clearMocks config):
  // assertions like "the stored grade is never re-graded" depend on a
  // clean slate.
  vi.clearAllMocks();
  vi.mocked(fetchWritingPrompts).mockResolvedValue([]);
  vi.mocked(fetchWritingPrompt).mockResolvedValue(null);
  vi.mocked(drawWritingPrompt).mockResolvedValue(null);
  vi.mocked(checkWriting).mockResolvedValue(null);
  vi.mocked(recheckWriting).mockResolvedValue(null);
  vi.mocked(deleteWritingPrompt).mockResolvedValue(true);
  vi.mocked(suggestWords).mockResolvedValue([]);
  vi.spyOn(window, "confirm").mockReturnValue(true);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

// Open a problem from the history list (the row button's accessible name is
// its content, so the click targets the title text itself).
function openRow(korean: string) {
  fireEvent.click(screen.getByText(korean));
}

describe("Writing", () => {
  it("shows an empty state when the learner has no prompts yet", async () => {
    render(<Writing />);
    await flushUntil(() => screen.getByText("아직 만든 작문 문제가 없어요."));
    expect(screen.getByRole("button", { name: "＋ 새 문제 만들기" })).toBeInTheDocument();
  });

  it("lists saved problems grouped by day, newest first", async () => {
    vi.mocked(fetchWritingPrompts).mockResolvedValue([oldPrompt, newPrompt]);
    render(<Writing />);
    await flushUntil(() => screen.getAllByText(newPrompt.korean));

    // Both prompts fall on the same day, so exactly one divider shows.
    expect(screen.getAllByText(formatDateDivider(oldPrompt.createdAt))).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "작문 문제 삭제" })).toHaveLength(2);
    const rows = screen.getAllByText(/어제 영화를 봤어요\.|오늘은 책을 읽어요\./);
    expect(rows[0]).toHaveTextContent(newPrompt.korean);
  });

  it("creates a problem and shows it in the answer stage", async () => {
    const drawn: WritingPrompt = { id: "p3", korean: "내일 공원에 갈 거예요.", status: "done", createdAt: 1700000200 };
    vi.mocked(drawWritingPrompt).mockResolvedValue(drawn);
    render(<Writing />);
    await flushUntil(() => screen.getByText("아직 만든 작문 문제가 없어요."));

    fireEvent.click(screen.getByRole("button", { name: "＋ 새 문제 만들기" }));
    await flushUntil(() => screen.getByText("내일 공원에 갈 거예요."));
    expect(screen.getByPlaceholderText("영어로 한 문장을 써보세요")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "답안 확인" })).toBeInTheDocument();
    // The word search stays beside, never inside, the answer form: a nested
    // form would hijack Enter into submitting the answer.
    expect(screen.getByText("모르는 단어가 있으면 검색해 보세요.")).toBeInTheDocument();
    expect(document.querySelector("form.writing-answer-form .word-search")).toBeNull();
  });

  it("restores the stored answer and finished grade when a problem reopens", async () => {
    vi.mocked(fetchWritingPrompts).mockResolvedValue([graded()]);
    vi.mocked(fetchWritingPrompt).mockResolvedValue(graded());
    render(<Writing />);
    await flushUntil(() => screen.getAllByText(oldPrompt.korean));

    openRow(oldPrompt.korean);

    // The feedback stage comes back on its own — no re-typing, no re-check.
    await flushUntil(() => screen.getByText("더 자연스러운 문장"));
    expect(screen.getByText("I watched movie yesterday.")).toBeInTheDocument();
    expect(screen.getByText("I watched a movie yesterday.")).toBeInTheDocument();
    expect(screen.getByText("셀 수 있는 명사 앞에는 관사가 필요해요.")).toBeInTheDocument();
    expect(vi.mocked(checkWriting)).not.toHaveBeenCalled();
    // A finished check must not be re-graded just by looking at it.
    expect(vi.mocked(fetchWritingPrompt)).toHaveBeenCalledTimes(1);
  });

  it("restores a stored answer back into the editor when the last check failed", async () => {
    const draft = graded({ checkStatus: "failed", corrected: "", issues: [] });
    vi.mocked(fetchWritingPrompts).mockResolvedValue([draft]);
    vi.mocked(fetchWritingPrompt).mockResolvedValue(draft);
    render(<Writing />);
    await flushUntil(() => screen.getAllByText(oldPrompt.korean));

    openRow(oldPrompt.korean);
    await flushUntil(() => screen.getByDisplayValue("I watched movie yesterday."));
    // The failed check explains itself and offers the retry without retyping.
    expect(screen.getByRole("alert")).toHaveTextContent("답안 확인에 실패했어요");
    expect(screen.getByRole("button", { name: "다시 검사" })).toBeInTheDocument();
  });

  it("shows a failed grade as feedback with the correction and issues", async () => {
    const failed: WritingPrompt = {
      ...oldPrompt,
      answer: "I am agree.",
      checkStatus: "done",
      corrected: "I agree.",
      issues: [
        {
          type: "grammar",
          span: "am agree",
          suggestion: "agree",
          explanation: "'Agree' is already a verb.",
          explanationTranslation: "'agree' 자체가 동사라서 'am'이 필요 없어요.",
        },
      ],
    };
    vi.mocked(fetchWritingPrompts).mockResolvedValue([failed]);
    vi.mocked(fetchWritingPrompt).mockResolvedValue(failed);
    render(<Writing />);
    await flushUntil(() => screen.getAllByText(oldPrompt.korean));
    openRow(oldPrompt.korean);

    await flushUntil(() => screen.getByText("더 자연스러운 문장"));
    expect(screen.getByText("I agree.")).toBeInTheDocument();
    expect(screen.getByText("'agree' 자체가 동사라서 'am'이 필요 없어요.")).toBeInTheDocument();
    // The English-only explanation stays hidden when the Korean one exists.
    expect(screen.queryByText("'Agree' is already a verb.")).not.toBeInTheDocument();
  });

  it("shows the Korean explanation once a submitted answer is graded", async () => {
    vi.mocked(drawWritingPrompt).mockResolvedValue(oldPrompt);
    vi.mocked(checkWriting).mockResolvedValue(graded());
    render(<Writing />);
    await flushUntil(() => screen.getByText("아직 만든 작문 문제가 없어요."));
    fireEvent.click(screen.getByRole("button", { name: "＋ 새 문제 만들기" }));
    await flushUntil(() => screen.getByText(oldPrompt.korean));

    fireEvent.change(screen.getByPlaceholderText("영어로 한 문장을 써보세요"), { target: { value: "I watched movie yesterday." } });
    fireEvent.click(screen.getByRole("button", { name: "답안 확인" }));

    await flushUntil(() => screen.getByText("셀 수 있는 명사 앞에는 관사가 필요해요."));
    expect(screen.getByText("I watched a movie yesterday.")).toBeInTheDocument();
    expect(screen.queryByText("Countable nouns need an article.")).not.toBeInTheDocument();
    expect(checkWriting).toHaveBeenCalledWith("p1", "I watched movie yesterday.");
  });

  // The core of the async contract: the submit response is only "pending" —
  // the editor locks with a status message, the detached server-side grade
  // arrives through the poll, and the feedback stage swaps in by itself.
  it("keeps a submitted answer pending and lands the polled grade without staying on the page", async () => {
    vi.useFakeTimers();
    vi.mocked(drawWritingPrompt).mockResolvedValue(oldPrompt);
    vi.mocked(checkWriting).mockResolvedValue({ ...oldPrompt, answer: "I watched movie.", checkStatus: "pending" });
    vi.mocked(fetchWritingPrompt).mockResolvedValue(graded({ answer: "I watched movie." }));

    render(<Writing />);
    await tickUntil(() => screen.queryByText("아직 만든 작문 문제가 없어요.") !== null);
    fireEvent.click(screen.getByRole("button", { name: "＋ 새 문제 만들기" }));
    await tickUntil(() => screen.queryByPlaceholderText("영어로 한 문장을 써보세요") !== null);

    fireEvent.change(screen.getByPlaceholderText("영어로 한 문장을 써보세요"), { target: { value: "I watched movie." } });
    fireEvent.click(screen.getByRole("button", { name: "답안 확인" }));

    // Pending state: the answer is stored, the editor is locked, and the
    // message says the work continues even after leaving.
    await tickUntil(() => screen.queryByText(/답안을 저장했어요/) !== null);
    expect(screen.getByPlaceholderText("영어로 한 문장을 써보세요")).toBeDisabled();
    expect(screen.queryByText("더 자연스러운 문장")).toBeNull();

    // The poll picks the finished grade up on its own.
    await act(() => vi.advanceTimersByTimeAsync(2500));
    await tickUntil(() => screen.queryByText("더 자연스러운 문장") !== null);
    expect(screen.getByText("I watched a movie yesterday.")).toBeInTheDocument();
    // The feedback stage replaces the editor once the grade lands.
    expect(screen.queryByPlaceholderText("영어로 한 문장을 써보세요")).toBeNull();

    // Once graded, the poll stops — no further reads after convergence.
    const reads = vi.mocked(fetchWritingPrompt).mock.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(vi.mocked(fetchWritingPrompt).mock.calls.length).toBe(reads);
  });

  it("keeps the history list badge in step with a submitted check", async () => {
    vi.useFakeTimers();
    vi.mocked(fetchWritingPrompts).mockResolvedValue([oldPrompt]);
    // Open plainly; from the submit onward the check is pending everywhere.
    vi.mocked(fetchWritingPrompt)
      .mockResolvedValueOnce(oldPrompt)
      .mockResolvedValue({ ...oldPrompt, answer: "a", checkStatus: "pending" });
    vi.mocked(checkWriting).mockResolvedValue({ ...oldPrompt, answer: "a", checkStatus: "pending" });

    render(<Writing />);
    await tickUntil(() => screen.queryByText(oldPrompt.korean) !== null);
    openRow(oldPrompt.korean);
    await tickUntil(() => screen.queryByPlaceholderText("영어로 한 문장을 써보세요") !== null);
    fireEvent.change(screen.getByPlaceholderText("영어로 한 문장을 써보세요"), { target: { value: "a" } });
    fireEvent.click(screen.getByRole("button", { name: "답안 확인" }));
    await tickUntil(() => screen.queryByText(/답안을 저장했어요/) !== null);

    fireEvent.click(screen.getByRole("button", { name: "목록으로" }));
    await tickUntil(() => screen.queryByText("검사 중") !== null);
  });

  it("re-checks the stored answer with the re-check button, without retyping", async () => {
    vi.useFakeTimers();
    const failedCheck = graded({ checkStatus: "failed", corrected: "", issues: [] });
    vi.mocked(fetchWritingPrompts).mockResolvedValue([failedCheck]);
    vi.mocked(fetchWritingPrompt).mockResolvedValue(failedCheck);
    vi.mocked(recheckWriting).mockResolvedValue({ ...failedCheck, checkStatus: "pending" });

    render(<Writing />);
    await tickUntil(() => screen.queryByText(oldPrompt.korean) !== null);
    openRow(oldPrompt.korean);
    await tickUntil(() => screen.queryByRole("button", { name: "다시 검사" }) !== null);

    fireEvent.click(screen.getByRole("button", { name: "다시 검사" }));
    // The stored answer is resubmitted as-is, and the view goes pending.
    await tickUntil(() => screen.queryByText(/답안을 저장했어요/) !== null);
    expect(recheckWriting).toHaveBeenCalledWith("p1");
    expect(checkWriting).not.toHaveBeenCalled();

    vi.mocked(fetchWritingPrompt).mockResolvedValue(graded());
    await act(() => vi.advanceTimersByTimeAsync(2500));
    await tickUntil(() => screen.queryByText("더 자연스러운 문장") !== null);
  });

  it("offers the re-check button on the feedback stage too", async () => {
    vi.mocked(fetchWritingPrompts).mockResolvedValue([graded()]);
    vi.mocked(fetchWritingPrompt).mockResolvedValue(graded());
    render(<Writing />);
    await flushUntil(() => screen.getAllByText(oldPrompt.korean));
    openRow(oldPrompt.korean);
    await flushUntil(() => screen.getByText("더 자연스러운 문장"));

    fireEvent.click(screen.getByRole("button", { name: "다시 검사" }));
    expect(recheckWriting).toHaveBeenCalledWith("p1");
  });

  it("reports when submitting cannot be accepted and keeps the draft", async () => {
    vi.mocked(drawWritingPrompt).mockResolvedValue(oldPrompt);
    vi.mocked(checkWriting).mockResolvedValue(null);
    render(<Writing />);
    await flushUntil(() => screen.getByText("아직 만든 작문 문제가 없어요."));
    fireEvent.click(screen.getByRole("button", { name: "＋ 새 문제 만들기" }));
    await flushUntil(() => screen.getByText(oldPrompt.korean));

    fireEvent.change(screen.getByPlaceholderText("영어로 한 문장을 써보세요"), { target: { value: "I watched movie." } });
    fireEvent.click(screen.getByRole("button", { name: "답안 확인" }));
    await flushUntil(() => screen.getByText("답안을 확인하지 못했어요. 연결 상태를 확인한 뒤 다시 시도해 주세요."));
    expect(screen.getByDisplayValue("I watched movie.")).toBeInTheDocument();
  });

  it("opens a saved problem and shows its answer editor", async () => {
    vi.mocked(fetchWritingPrompts).mockResolvedValue([oldPrompt]);
    vi.mocked(fetchWritingPrompt).mockResolvedValue(oldPrompt);
    render(<Writing />);
    await flushUntil(() => screen.getAllByText(oldPrompt.korean));

    openRow(oldPrompt.korean);
    await flushUntil(() => screen.getByPlaceholderText("영어로 한 문장을 써보세요"));
    expect(screen.getByText(oldPrompt.korean)).toBeInTheDocument();
    expect(fetchWritingPrompt).toHaveBeenCalledWith("p1");
  });

  it("offers the word search next to the editor without nesting it in the answer form", async () => {
    vi.mocked(drawWritingPrompt).mockResolvedValue(oldPrompt);
    vi.mocked(suggestWords).mockResolvedValue([
      { word: "movie", meaning: "영화", example: "The movie starts now." },
      { word: "cinema", meaning: "영화관", example: "We met at the cinema." },
    ]);
    render(<Writing />);
    await flushUntil(() => screen.getByText("아직 만든 작문 문제가 없어요."));
    fireEvent.click(screen.getByRole("button", { name: "＋ 새 문제 만들기" }));
    await flushUntil(() => screen.getByText(oldPrompt.korean));

    fireEvent.click(screen.getByRole("button", { name: "모르는 단어 찾기" }));
    const search = await screen.findByPlaceholderText("예: 화가 나서 참을 수 없는 느낌");
    fireEvent.change(search, { target: { value: "영화" } });
    fireEvent.click(screen.getByRole("button", { name: "찾기" }));
    await flushUntil(() => within(screen.getByRole("menu")).getByText("movie"));
    expect(screen.getByText("cinema")).toBeInTheDocument();
    expect(document.querySelector("form.writing-answer-form .word-search")).toBeNull();
  });

  it("reports a failed word search without disturbing the answer", async () => {
    vi.mocked(drawWritingPrompt).mockResolvedValue(oldPrompt);
    vi.mocked(suggestWords).mockResolvedValue(null);
    render(<Writing />);
    await flushUntil(() => screen.getByText("아직 만든 작문 문제가 없어요."));
    fireEvent.click(screen.getByRole("button", { name: "＋ 새 문제 만들기" }));
    await flushUntil(() => screen.getByText(oldPrompt.korean));

    fireEvent.click(screen.getByRole("button", { name: "모르는 단어 찾기" }));
    const search = await screen.findByPlaceholderText("예: 화가 나서 참을 수 없는 느낌");
    fireEvent.change(search, { target: { value: "영화" } });
    fireEvent.click(screen.getByRole("button", { name: "찾기" }));
    await flushUntil(() => screen.getByText("불러오지 못했어요."));
    const answerBox = screen.getByPlaceholderText("영어로 한 문장을 써보세요");
    fireEvent.change(answerBox, { target: { value: "I watched a film." } });
    expect(answerBox).toHaveValue("I watched a film.");
  });

  it("deletes a saved problem from its row", async () => {
    vi.mocked(fetchWritingPrompts).mockResolvedValue([oldPrompt]);
    render(<Writing />);
    await flushUntil(() => screen.getAllByText(oldPrompt.korean));

    fireEvent.click(screen.getByRole("button", { name: "작문 문제 삭제" }));
    await flushUntil(() => screen.getByText("아직 만든 작문 문제가 없어요."));
    expect(deleteWritingPrompt).toHaveBeenCalledWith("p1");
    expect(window.confirm).toHaveBeenCalled();
  });

  it("keeps a row when the delete call fails", async () => {
    vi.mocked(fetchWritingPrompts).mockResolvedValue([oldPrompt]);
    vi.mocked(deleteWritingPrompt).mockResolvedValue(false);
    render(<Writing />);
    await flushUntil(() => screen.getAllByText(oldPrompt.korean));

    fireEvent.click(screen.getByRole("button", { name: "작문 문제 삭제" }));
    await flushUntil(() => screen.getAllByText(oldPrompt.korean));
  });

  it("goes back from a problem to the list", async () => {
    vi.mocked(fetchWritingPrompts).mockResolvedValue([oldPrompt]);
    vi.mocked(fetchWritingPrompt).mockResolvedValue(oldPrompt);
    render(<Writing />);
    await flushUntil(() => screen.getAllByText(oldPrompt.korean));

    openRow(oldPrompt.korean);
    await flushUntil(() => screen.getByPlaceholderText("영어로 한 문장을 써보세요"));
    fireEvent.click(screen.getByRole("button", { name: "목록으로" }));
    await flushUntil(() => screen.getByText(/총 1개/));
  });

  it("offers a retry when a saved problem cannot be loaded", async () => {
    vi.mocked(fetchWritingPrompts).mockResolvedValue([oldPrompt]);
    vi.mocked(fetchWritingPrompt).mockResolvedValue(null);
    render(<Writing />);
    await flushUntil(() => screen.getAllByText(oldPrompt.korean));

    openRow(oldPrompt.korean);
    await flushUntil(() => screen.getByText("문제를 불러오지 못했어요."));
    expect(screen.getByRole("button", { name: "다시 시도" })).toBeInTheDocument();
  });
});
