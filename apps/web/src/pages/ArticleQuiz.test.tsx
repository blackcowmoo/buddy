/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("../lib/articles", async () => {
  const actual = await vi.importActual<typeof import("../lib/articles")>("../lib/articles");
  return {
    ...actual,
    fetchArticleInstances: vi.fn(),
    drawArticle: vi.fn(),
    answerArticle: vi.fn(),
    deleteArticleInstance: vi.fn(),
  };
});

vi.mock("../tts/kokoro", () => ({
  KokoroSpeaker: vi.fn().mockImplementation(function KokoroSpeaker(this: object) {
    return Object.assign(this, {
      loaded: false,
      load: vi.fn().mockResolvedValue(undefined),
      speak: vi.fn().mockResolvedValue(undefined),
    });
  }),
}));

import { ArticleQuiz } from "./ArticleQuiz";
import {
  answerArticle,
  deleteArticleInstance,
  drawArticle,
  fetchArticleInstances,
  type ArticleDraw,
} from "../lib/articles";
import { formatDateDivider } from "../lib/time";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const sampleDraw: ArticleDraw = {
  id: "i1",
  source: "BBC",
  title: "Scientists make discovery",
  summary: "Scientists announced a new discovery today.",
  choices: ["정확한 해석", "틀린 해석 1", "틀린 해석 2", "틀린 해석 3"],
};

describe("ArticleQuiz page — list view", () => {
  it("links back to the chat page with a relative href", () => {
    vi.mocked(fetchArticleInstances).mockReturnValue(new Promise(() => {}));
    render(<ArticleQuiz />);
    expect(screen.getByRole("link", { name: "대화로 돌아가기" })).toHaveAttribute("href", ".");
  });

  it("shows a loading hint before the fetch resolves", () => {
    vi.mocked(fetchArticleInstances).mockReturnValue(new Promise(() => {}));
    render(<ArticleQuiz />);
    expect(screen.getByText("불러오는 중…")).toBeInTheDocument();
  });

  it("shows an empty-state hint when there are no past attempts", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    render(<ArticleQuiz />);
    expect(await screen.findByText("아직 읽은 아티클이 없어요.")).toBeInTheDocument();
  });

  it("shows each past attempt's source, title, and correctness", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([
      { id: "i1", source: "BBC", title: "Old story", summary: "s", answered: true, correct: true, createdAt: 1700000000 },
    ]);
    render(<ArticleQuiz />);
    expect(await screen.findByText("[BBC] Old story")).toBeInTheDocument();
    expect(screen.getByText(/정답/)).toBeInTheDocument();
  });

  it("groups past attempts from the same day under a single date divider", async () => {
    const morning = 1700000000;
    const laterSameDay = morning + 3600;
    vi.mocked(fetchArticleInstances).mockResolvedValue([
      { id: "i1", source: "BBC", title: "first", summary: "s", answered: false, correct: false, createdAt: morning },
      { id: "i2", source: "NPR", title: "second", summary: "s", answered: false, correct: false, createdAt: laterSameDay },
    ]);
    render(<ArticleQuiz />);

    await screen.findByText("[BBC] first");
    expect(screen.getAllByText(formatDateDivider(morning))).toHaveLength(1);
  });

  it("asks for confirmation, deletes, and removes the row on confirmed success", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([
      { id: "i1", source: "BBC", title: "Old story", summary: "s", answered: false, correct: false, createdAt: 1700000000 },
    ]);
    vi.mocked(deleteArticleInstance).mockResolvedValue(true);
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "아티클 퀴즈 삭제" }));

    expect(confirmSpy).toHaveBeenCalled();
    expect(deleteArticleInstance).toHaveBeenCalledWith("i1");
    expect(await screen.findByText("아직 읽은 아티클이 없어요.")).toBeInTheDocument();
  });
});

describe("ArticleQuiz page — draw / reading / quiz / result flow", () => {
  it("shows the reading view with the English summary after a successful draw", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));

    expect(await screen.findByText(sampleDraw.summary)).toBeInTheDocument();
    expect(screen.getByText("[BBC] Scientists make discovery")).toBeInTheDocument();
  });

  it("shows a hint and stays on the list when there's nothing new to draw", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "noMore" });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));

    expect(await screen.findByText("지금은 새로 볼 아티클이 없어요. 나중에 다시 시도해보세요.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "새 아티클 뽑기" })).toBeInTheDocument();
  });

  it("shows an error hint when the draw request fails", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "error" });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));

    expect(await screen.findByText("아티클을 가져오지 못했습니다. 네트워크 문제일 수 있습니다.")).toBeInTheDocument();
  });

  it("reveals the quiz choices only after tapping 문제풀기", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    expect(screen.queryByText("틀린 해석 1")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "문제풀기" }));
    expect(screen.getByText("틀린 해석 1")).toBeInTheDocument();
  });

  it("submits the selected choice and shows the correct reveal", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    vi.mocked(answerArticle).mockResolvedValue({
      correct: true,
      correctIndex: 0,
      translation: "정확한 해석",
      explanation: "원문의 의미를 정확히 반영하기 때문입니다.",
    });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "문제풀기" }));
    await user.click(screen.getByRole("button", { name: "정확한 해석" }));

    expect(answerArticle).toHaveBeenCalledWith("i1", 0);
    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
    expect(screen.getByText("원문의 의미를 정확히 반영하기 때문입니다.")).toBeInTheDocument();
  });

  it("shows an incorrect reveal when the wrong choice was picked", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    vi.mocked(answerArticle).mockResolvedValue({
      correct: false,
      correctIndex: 0,
      translation: "정확한 해석",
      explanation: "왜냐하면",
    });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "문제풀기" }));
    await user.click(screen.getByRole("button", { name: "틀린 해석 1" }));

    expect(await screen.findByText("아쉬워요, 오답이에요.")).toBeInTheDocument();
  });
});
