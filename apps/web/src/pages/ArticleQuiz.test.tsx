/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("../lib/articles", async () => {
  const actual = await vi.importActual<typeof import("../lib/articles")>("../lib/articles");
  return {
    ...actual,
    fetchArticleInstances: vi.fn(),
    fetchArticleInstance: vi.fn(),
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
  fetchArticleInstance,
  fetchArticleInstances,
  type ArticleDraw,
} from "../lib/articles";
import { formatAbsoluteDate, formatDateDivider } from "../lib/time";
import { KokoroSpeaker } from "../tts/kokoro";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.useRealTimers();
});

const sampleDraw: ArticleDraw = {
  id: "i1",
  source: "BBC",
  title: "Scientists make discovery",
  summary: "Scientists announced a new discovery today.",
  choices: ["정확한 해석", "틀린 해석 1", "틀린 해석 2", "틀린 해석 3"],
  publishedAt: 1710494400,
  status: "done",
};

const pendingDraw: ArticleDraw = { ...sampleDraw, summary: "", choices: [], status: "pending" };

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
      { id: "i1", source: "BBC", title: "Old story", summary: "s", answered: true, correct: true, createdAt: 1700000000, publishedAt: 0, status: "done" as const },
    ]);
    render(<ArticleQuiz />);
    expect(await screen.findByText("[BBC] Old story")).toBeInTheDocument();
    expect(screen.getByText(/정답/)).toBeInTheDocument();
  });

  it("groups past attempts from the same day under a single date divider", async () => {
    const morning = 1700000000;
    const laterSameDay = morning + 3600;
    vi.mocked(fetchArticleInstances).mockResolvedValue([
      { id: "i1", source: "BBC", title: "first", summary: "s", answered: false, correct: false, createdAt: morning, publishedAt: 0, status: "done" as const },
      { id: "i2", source: "NPR", title: "second", summary: "s", answered: false, correct: false, createdAt: laterSameDay, publishedAt: 0, status: "done" as const },
    ]);
    render(<ArticleQuiz />);

    await screen.findByText("[BBC] first");
    expect(screen.getAllByText(formatDateDivider(morning))).toHaveLength(1);
  });

  it("asks for confirmation, deletes, and removes the row on confirmed success", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([
      { id: "i1", source: "BBC", title: "Old story", summary: "s", answered: false, correct: false, createdAt: 1700000000, publishedAt: 0, status: "done" as const },
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
    expect(screen.getByText(formatAbsoluteDate(new Date(sampleDraw.publishedAt * 1000)))).toBeInTheDocument();
  });

  it("omits the date line when publishedAt is unknown", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: { ...sampleDraw, publishedAt: 0 } });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));

    expect(await screen.findByText(sampleDraw.summary)).toBeInTheDocument();
    expect(screen.queryByText(formatAbsoluteDate(new Date(sampleDraw.publishedAt * 1000)))).not.toBeInTheDocument();
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

  it("shows a generating hint for a pending draw, then the summary once polling reports done", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: pendingDraw });
    vi.mocked(fetchArticleInstance).mockResolvedValue(sampleDraw);
    const user = userEvent.setup({ delay: null });
    render(<ArticleQuiz />);
    const drawButton = await screen.findByRole("button", { name: "새 아티클 뽑기" });

    // Fake timers only from here: pollDraw's setTimeout must be one vi
    // tracks, so it needs to be scheduled (by the click below) after this,
    // not before — an already-real-scheduled timer wouldn't be affected by
    // advanceTimersByTimeAsync later. shouldAdvanceTime keeps React's own
    // internal (also setTimeout-based) scheduler unstuck, since it isn't
    // ever explicitly advanced below — only the poll interval is.
    vi.useFakeTimers({ shouldAdvanceTime: true });
    await user.click(drawButton);
    expect(screen.getByText(/아티클을 요약하고 문제를 만드는 중이에요/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "문제풀기" })).not.toBeInTheDocument();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });

    expect(fetchArticleInstance).toHaveBeenCalledWith(pendingDraw.id);
    expect(screen.getByText(sampleDraw.summary)).toBeInTheDocument();
  });

  it("resumes polling a still-generating draw reopened from the list", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([
      { id: "i1", source: "BBC", title: "Scientists make discovery", summary: "", answered: false, correct: false, createdAt: 1700000000, publishedAt: 0, status: "pending" },
    ]);
    vi.mocked(fetchArticleInstance).mockResolvedValue(pendingDraw);
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    expect(await screen.findByText("생성 중")).toBeInTheDocument();
    await user.click(screen.getByText("[BBC] Scientists make discovery"));

    expect(fetchArticleInstance).toHaveBeenCalledWith("i1");
    expect(await screen.findByText(/아티클을 요약하고 문제를 만드는 중이에요/)).toBeInTheDocument();
  });

  it("reopens a finished past attempt into the reading view", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([
      { id: "i1", source: "BBC", title: "Old story", summary: "s", answered: true, correct: true, createdAt: 1700000000, publishedAt: 0, status: "done" as const },
    ]);
    vi.mocked(fetchArticleInstance).mockResolvedValue(sampleDraw);
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByText("[BBC] Old story"));

    expect(fetchArticleInstance).toHaveBeenCalledWith("i1");
    expect(await screen.findByText(sampleDraw.summary)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "문제풀기" })).toBeInTheDocument();
  });

  it("reads the summary aloud through a real speaker instance when 읽어주기 is tapped", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "🔊 읽어주기" }));

    // Regression guard: the speaker ref must actually be constructed (see the
    // mount effect that assigns speakerRef.current), otherwise handleRead's
    // `if (!sp) return` bails out silently and the button does nothing.
    const speakerInstance = vi.mocked(KokoroSpeaker).mock.instances[0];
    await vi.waitFor(() => expect(speakerInstance.speak).toHaveBeenCalledWith(sampleDraw.summary));
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
