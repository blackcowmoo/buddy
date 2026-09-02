/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
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
import { deleteWritingPrompt, drawWritingPrompt, fetchWritingPrompt, fetchWritingPrompts } from "../lib/writing";
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
  it("groups prompts by calendar day with the same date dividers as articles", async () => {
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
    expect(screen.getByText(formatDateDivider(firstDay)).compareDocumentPosition(screen.getByText(formatDateDivider(secondDay))) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("starts on a list and opens the selected problem as a detail view", async () => {
    const user = userEvent.setup();
    render(<Writing />);

    expect(await screen.findByRole("button", { name: /어제 영화를 봤어요/ })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /어제 영화를 봤어요/ }));

    expect(await screen.findByRole("heading", { name: "오늘의 한 문장" })).toBeInTheDocument();
    expect(screen.getByPlaceholderText("영어로 한 문장을 써보세요")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "← 목록으로" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /새 문제 만들기/ })).not.toBeInTheDocument();
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
