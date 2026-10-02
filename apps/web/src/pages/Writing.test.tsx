/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../lib/writing", () => ({
  fetchWritingPrompts: vi.fn(),
  fetchWritingPrompt: vi.fn(),
  drawWritingPrompt: vi.fn(),
  checkWriting: vi.fn(),
  deleteWritingPrompt: vi.fn(),
}));
vi.mock("../lib/wordSearch", () => ({ suggestWords: vi.fn() }));
vi.mock("../lib/wordReview", () => ({ saveWord: vi.fn() }));

import { Writing } from "./Writing";
import "../styles.css";
import { checkWriting, deleteWritingPrompt, drawWritingPrompt, fetchWritingPrompt, fetchWritingPrompts } from "../lib/writing";
import { suggestWords } from "../lib/wordSearch";
import { formatDateDivider } from "../lib/time";

const oldPrompt = { id: "p1", korean: "어제 영화를 봤어요.", status: "done" as const, createdAt: 1700000000 };
const newPrompt = { id: "p2", korean: "오늘은 책을 읽어요.", status: "done" as const, createdAt: 1700000100 };

beforeEach(() => {
  vi.mocked(fetchWritingPrompts).mockResolvedValue([oldPrompt]);
  vi.mocked(fetchWritingPrompt).mockImplementation(async (id) => id === oldPrompt.id ? oldPrompt : newPrompt);
  vi.mocked(drawWritingPrompt).mockResolvedValue(newPrompt);
  vi.mocked(deleteWritingPrompt).mockResolvedValue(true);
  vi.mocked(suggestWords).mockResolvedValue([]);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Writing page list/detail flow", () => {
  it("groups prompts by calendar day and shows the newest day first", async () => {
    const firstDay = new Date(2024, 0, 1, 12).getTime() / 1000;
    const secondDay = new Date(2024, 0, 2, 12).getTime() / 1000;
    vi.mocked(fetchWritingPrompts).mockResolvedValue([
      { ...oldPrompt, createdAt: secondDay },
      { ...newPrompt, createdAt: firstDay },
    ]);
    render(<Writing />);

    expect(await screen.findByRole("button", { name: /어제 영화를 봤어요/ })).toBeInTheDocument();
    expect(screen.getAllByText(formatDateDivider(firstDay))).toHaveLength(1);
    expect(screen.getAllByText(formatDateDivider(secondDay))).toHaveLength(1);
    expect(screen.getByText(formatDateDivider(secondDay)).compareDocumentPosition(screen.getByText(formatDateDivider(firstDay))) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("starts the list at the top and orders prompts from newest to oldest", async () => {
    vi.mocked(fetchWritingPrompts).mockResolvedValue([oldPrompt, newPrompt]);
    render(<Writing />);

    const oldRow = (await screen.findByText(oldPrompt.korean)).closest(".session-row")!;
    const newRow = screen.getByText(newPrompt.korean).closest(".session-row")!;
    expect(newRow.compareDocumentPosition(oldRow) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole("main").scrollTop).toBe(0);
  });

  it("starts on a list and opens the selected problem as a detail view", async () => {
    const user = userEvent.setup();
    render(<Writing />);

    const history = screen.getByRole("region", { name: "나의 작문 기록" });
    const promptButton = await within(history).findByRole("button", { name: /어제 영화를 봤어요/ });
    expect(screen.getByRole("heading", { name: "한 문장부터 써 볼까요?" })).toBeInTheDocument();
    const actions = screen.getByRole("group", { name: "주요 작업" });
    expect(within(actions).getByRole("button", { name: /새 문제 만들기/ })).toBeEnabled();
    expect(actions.compareDocumentPosition(history) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(promptButton).toHaveClass("session-item");
    expect(promptButton.closest(".session-row")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "작문 문제 삭제" })).toHaveClass("session-delete");
    await user.click(promptButton);

    const detail = await screen.findByRole("region", { name: "오늘의 한 문장" });
    expect(within(detail).getByLabelText("영어 답안")).toBeInTheDocument();
    // The Korean prompt reads with the article page's shared label+quote
    // presentation rather than a bespoke writing-only box.
    const koreanPrompt = within(detail).getByText("어제 영화를 봤어요.");
    expect(koreanPrompt).toHaveClass("translation-quote");
    expect(koreanPrompt.previousElementSibling).toHaveClass("language-label");
    expect(screen.queryByRole("heading", { name: "한 문장부터 써 볼까요?" })).not.toBeInTheDocument();
    const back = screen.getByRole("button", { name: "목록으로" });
    expect(back.compareDocumentPosition(detail) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.queryByRole("button", { name: /새 문제 만들기/ })).not.toBeInTheDocument();
    expect(screen.getAllByRole("main")).toHaveLength(1);
    expect(screen.getByRole("main")).toContainElement(detail);
    expect(screen.getAllByRole("banner")).toHaveLength(1);

    await user.click(back);
    const restoredHistory = await screen.findByRole("region", { name: "나의 작문 기록" });
    expect(within(restoredHistory).getByRole("button", { name: /어제 영화를 봤어요/ })).toBeEnabled();
    expect(screen.queryByRole("region", { name: "오늘의 한 문장" })).not.toBeInTheDocument();
    expect(screen.getAllByRole("main")).toHaveLength(1);
    expect(screen.getAllByRole("banner")).toHaveLength(1);
  });

  it("opens a newly created problem directly in the detail view", async () => {
    const user = userEvent.setup();
    render(<Writing />);

    await screen.findByRole("button", { name: /어제 영화를 봤어요/ });
    await user.click(screen.getByRole("button", { name: /새 문제 만들기/ }));

    expect(await screen.findByText("오늘은 책을 읽어요.")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("영어로 한 문장을 써보세요")).toBeInTheDocument();
    expect(drawWritingPrompt).toHaveBeenCalledOnce();
  });

  it("deletes a problem from the list after confirmation", async () => {
    const user = userEvent.setup();
    vi.spyOn(window, "confirm").mockReturnValue(true);
    render(<Writing />);
    await screen.findByRole("button", { name: /어제 영화를 봤어요/ });
    await user.click(screen.getByRole("button", { name: "작문 문제 삭제" }));
    expect(deleteWritingPrompt).toHaveBeenCalledWith("p1");
    expect(screen.queryByText("어제 영화를 봤어요.")).not.toBeInTheDocument();
  });

  it("offers the same word search while writing an answer", async () => {
    vi.mocked(suggestWords).mockResolvedValue([
      { word: "watched", meaning: "보다의 과거형", example: "I watched a movie yesterday." },
    ]);
    const user = userEvent.setup();
    render(<Writing />);

    await user.click(await screen.findByRole("button", { name: /어제 영화를 봤어요/ }));
    await user.click(screen.getByRole("button", { name: "모르는 단어 찾기" }));
    const searchForm = screen.getByRole("button", { name: "찾기" }).closest("form");
    expect(searchForm).toBeInTheDocument();
    expect(searchForm?.parentElement?.closest("form")).toBeNull();
    await user.type(screen.getByPlaceholderText("예: 화가 나서 참을 수 없는 느낌"), "보다의 과거형");
    await user.click(screen.getByRole("button", { name: "찾기" }));

    expect(suggestWords).toHaveBeenCalledWith("보다의 과거형");
    expect(await screen.findByText("watched")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("영어로 한 문장을 써보세요")).toBeInTheDocument();
  });

  it("centers the word search overlay and places it below the answer tools", async () => {
    const user = userEvent.setup();
    render(<Writing />);

    await user.click(await screen.findByRole("button", { name: /어제 영화를 봤어요/ }));
    await user.click(screen.getByRole("button", { name: "모르는 단어 찾기" }));

    const panel = screen.getByRole("menu");
    expect(panel).toHaveClass("word-search-panel");
    expect(panel).toHaveClass("word-search-panel-below");
    expect(panel).toHaveStyle({ left: "50vw" });
    expect(panel).toHaveStyle({ top: "6px" });
    expect(panel.parentElement).toHaveClass("word-search");
    expect(panel.closest(".writing-answer-tools")).toBeInTheDocument();
    expect(panel.closest(".writing-detail-card")).toBeInTheDocument();
  });
});

it("associates a submitted answer with the selected prompt", async () => {
 const user = userEvent.setup();
 vi.mocked(checkWriting).mockResolvedValue(null);
 render(<Writing />);
 await user.click(await screen.findByRole("button", { name: /어제 영화를 봤어요/ }));
 await user.type(await screen.findByLabelText("영어 답안"), "I watched a movie.");
 await user.click(screen.getByRole("button", { name: "답안 확인" }));
 expect(checkWriting).toHaveBeenCalledWith(oldPrompt.korean, "I watched a movie.", oldPrompt.id);
});

const correction = {
  original: "I meet my friend yesterday.",
  corrected: "I met my friend yesterday.",
  issues: [{
    type: "grammar",
    span: "meet",
    suggestion: "met",
    explanation: "Past tense is required after 'yesterday'.",
    explanationTranslation: "'어제'가 있으므로 과거형 'met'을 써야 해요.",
  }],
};

async function submitAnswer(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("button", { name: /어제 영화를 봤어요/ }));
  await user.type(await screen.findByLabelText("영어 답안"), "I meet my friend yesterday.");
  await user.click(screen.getByRole("button", { name: "답안 확인" }));
}

describe("Writing feedback stage", () => {
  it("replaces the editor with a feedback stage showing the correction and issues", async () => {
    const user = userEvent.setup();
    vi.mocked(checkWriting).mockResolvedValue(correction);
    render(<Writing />);
    await submitAnswer(user);

    const feedback = await screen.findByRole("region", { name: "답안 피드백" });
    expect(screen.getByRole("status")).toHaveTextContent("조금 다듬어 볼까요?");
    expect(within(feedback).getByText("내가 쓴 문장")).toHaveClass("language-label");
    expect(within(feedback).getByText("내가 쓴 문장")).toBeInTheDocument();
    expect(within(feedback).getByText("I meet my friend yesterday.")).toBeInTheDocument();
    expect(within(feedback).getByText("더 자연스러운 문장")).toBeInTheDocument();
    expect(within(feedback).getByText("I met my friend yesterday.")).toBeInTheDocument();
    expect(within(feedback).getByText("문법")).toBeInTheDocument();
    expect(within(feedback).getByText("meet")).toBeInTheDocument();
    expect(within(feedback).getByText("met")).toBeInTheDocument();
    expect(within(feedback).getByText("'어제'가 있으므로 과거형 'met'을 써야 해요.")).toBeInTheDocument();
    // The editor yields the screen to the feedback until the learner edits again.
    expect(screen.queryByLabelText("영어 답안")).not.toBeInTheDocument();
    expect(screen.getByRole("main")).toContainElement(feedback);
  });

  it("celebrates an answer without issues", async () => {
    const user = userEvent.setup();
    vi.mocked(checkWriting).mockResolvedValue({ ...correction, issues: [] });
    render(<Writing />);
    await submitAnswer(user);

    const feedback = await screen.findByRole("region", { name: "답안 피드백" });
    expect(screen.getByRole("status")).toHaveTextContent("아주 좋아요!");
    expect(within(feedback).queryByText("문법")).not.toBeInTheDocument();
  });

  it("returns to the editor from the feedback stage with the draft intact", async () => {
    const user = userEvent.setup();
    vi.mocked(checkWriting).mockResolvedValue(correction);
    render(<Writing />);
    await submitAnswer(user);

    await user.click(await screen.findByRole("button", { name: "다시 수정하기" }));
    expect(screen.queryByRole("region", { name: "답안 피드백" })).not.toBeInTheDocument();
    expect(screen.getByLabelText("영어 답안")).toHaveValue("I meet my friend yesterday.");
  });

  it("draws a new problem from the feedback stage", async () => {
    const user = userEvent.setup();
    vi.mocked(checkWriting).mockResolvedValue(correction);
    render(<Writing />);
    await submitAnswer(user);

    await user.click(await screen.findByRole("button", { name: "다른 문장 받기" }));
    expect(drawWritingPrompt).toHaveBeenCalledOnce();
    expect(await screen.findByText("오늘은 책을 읽어요.")).toBeInTheDocument();
    expect(screen.getByLabelText("영어 답안")).toHaveValue("");
    expect(screen.queryByRole("region", { name: "답안 피드백" })).not.toBeInTheDocument();
  });

  it("reports a failed answer check and keeps the draft for a retry", async () => {
    const user = userEvent.setup();
    vi.mocked(checkWriting).mockResolvedValue(null);
    render(<Writing />);
    await submitAnswer(user);

    expect(await screen.findByRole("alert")).toHaveTextContent("답안을 확인하지 못했어요.");
    expect(screen.queryByRole("region", { name: "답안 피드백" })).not.toBeInTheDocument();
    expect(screen.getByLabelText("영어 답안")).toHaveValue("I meet my friend yesterday.");
  });
});
