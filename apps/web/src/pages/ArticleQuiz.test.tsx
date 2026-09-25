/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

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

vi.mock("../lib/wordSearch", async () => {
  const actual = await vi.importActual<typeof import("../lib/wordSearch")>("../lib/wordSearch");
  return { ...actual, checkDefinedWordStatus: vi.fn(), defineWord: vi.fn() };
});

vi.mock("../lib/wordReview", async () => {
  const actual = await vi.importActual<typeof import("../lib/wordReview")>("../lib/wordReview");
  return { ...actual, saveWord: vi.fn() };
});

import { ArticleQuiz, shouldCenterWordLookup } from "./ArticleQuiz";
import {
  answerArticle,
  articleAudioURL,
  deleteArticleInstance,
  drawArticle,
  fetchArticleInstance,
  fetchArticleInstances,
  type ArticleDraw,
  type ArticleInstance,
} from "../lib/articles";
import { formatAbsoluteDate, formatDateDivider } from "../lib/time";
import { checkDefinedWordStatus, defineWord } from "../lib/wordSearch";
import { saveWord } from "../lib/wordReview";

// jsdom doesn't implement HTMLMediaElement.play() — stub it so handleRead's
// el.play() resolves instead of throwing "not implemented", the same reason
// kokoro.test.ts stubs the global Audio constructor for its own (unrelated,
// client-side chat read-aloud) tests.
beforeEach(() => {
  HTMLMediaElement.prototype.play = vi.fn().mockResolvedValue(undefined);
  HTMLMediaElement.prototype.pause = vi.fn();
  vi.mocked(checkDefinedWordStatus).mockResolvedValue(null);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  localStorage.clear();
});

const sampleDraw: ArticleDraw = {
  id: "i1",
  source: "BBC",
  title: "Scientists make discovery",
  summary: "Scientists announced a new discovery today.",
  translation: "과학자들이 오늘 새로운 발견을 발표했습니다.",
  subQuestions: [
    { prompt: "어떤 내용이었나요?", options: ["정확한 해석", "틀린 해석"] },
    { prompt: "언제 일어났나요?", options: ["오늘", "어제"] },
  ],
  publishedAt: 1710494400,
  status: "done",
};

const pendingDraw: ArticleDraw = { ...sampleDraw, summary: "", subQuestions: [], status: "pending" };

const articleHistory = (count: number): ArticleInstance[] => Array.from({ length: count }, (_, i) => ({
  id: `i${i}`,
  source: "BBC",
  title: `Story ${i}`,
  summary: "s",
  answered: false,
  correct: false,
  createdAt: 1700000000 + i,
  publishedAt: 0,
  status: "done",
}));

// The reading view now splits the summary into per-word tappable buttons
// (see ArticleQuiz.tsx's word-lookup feature), so the full summary is no
// longer one element's own text-node child the way testing-library's default
// getByText(string) expects (it only reads a node's direct text children,
// not descendants) — match by the whole .article-summary element's
// textContent instead, which still equals the summary verbatim since
// tokenizing only ever re-renders the same characters across more elements.
const articleSummaryMatcher = (text: string) => (_content: string, element: Element | null) =>
  element?.classList.contains("article-summary") === true && element.textContent === text;

describe("ArticleQuiz page — list view", () => {
  it("shows a welcoming introduction with the shared hamburger navigation", () => {
    vi.mocked(fetchArticleInstances).mockReturnValue(new Promise(() => {}));
    render(<ArticleQuiz />);
    expect(screen.getByRole("button", { name: "메뉴" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "새로운 이야기를 읽어 봐요" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "홈으로" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "대화로 돌아가기" })).not.toBeInTheDocument();
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

  it("starts at the introduction and draw action, followed by history from newest to oldest", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([
      { id: "old", source: "BBC", title: "Old story", summary: "s", answered: false, correct: false, createdAt: 1700000000, publishedAt: 0, status: "done" as const },
      { id: "new", source: "NPR", title: "New story", summary: "s", answered: false, correct: false, createdAt: 1700003600, publishedAt: 0, status: "done" as const },
    ]);
    render(<ArticleQuiz />);
    const page = screen.getByRole("main");
    Object.defineProperty(page, "scrollHeight", { value: 1600 });

    const oldRow = (await screen.findByText("[BBC] Old story")).closest(".session-row")!;
    const newRow = screen.getByText("[NPR] New story").closest(".session-row")!;
    const intro = screen.getByRole("heading", { name: "새로운 이야기를 읽어 봐요" });
    const drawButton = screen.getByRole("button", { name: "새 아티클 뽑기" });
    const history = screen.getByRole("heading", { name: "나의 아티클 기록" });
    expect(intro.compareDocumentPosition(drawButton) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(drawButton.compareDocumentPosition(history) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(history.compareDocumentPosition(newRow) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(newRow.compareDocumentPosition(oldRow) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByText("총 2개 · 최근 기록부터")).toBeInTheDocument();
    expect(page.scrollTop).toBe(0);
  });

  it("appends older attempts near the bottom without moving the current scroll position", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue(articleHistory(22));
    render(<ArticleQuiz />);

    await screen.findByText("[BBC] Story 2");
    expect(screen.queryByText("[BBC] Story 0")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "이전 아티클 더 보기" })).toBeInTheDocument();

    const page = screen.getByRole("main");
    Object.defineProperties(page, {
      scrollHeight: { value: 1600 },
      clientHeight: { value: 600 },
    });
    fireEvent.scroll(page, { target: { scrollTop: 0 } });
    expect(screen.queryByText("[BBC] Story 0")).not.toBeInTheDocument();
    fireEvent.scroll(page, { target: { scrollTop: 919 } });
    expect(screen.queryByText("[BBC] Story 0")).not.toBeInTheDocument();
    fireEvent.scroll(page, { target: { scrollTop: 920 } });

    expect(await screen.findByText("[BBC] Story 0")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "이전 아티클 더 보기" })).not.toBeInTheDocument();
    expect(page.scrollTop).toBe(920);
    const titles = [...page.querySelectorAll(".article-instance-item .title")].map((title) => title.textContent);
    expect(titles).toEqual(Array.from({ length: 22 }, (_, i) => `[BBC] Story ${21 - i}`));
    expect(screen.getAllByText(formatDateDivider(1700000000))).toHaveLength(1);
  });

  it("offers a keyboard-accessible way to reveal older attempts in batches", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue(articleHistory(45));
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    const more = await screen.findByRole("button", { name: "이전 아티클 더 보기" });
    expect(screen.getAllByRole("button", { name: "아티클 퀴즈 삭제" })).toHaveLength(20);
    expect(screen.getByText("총 45개 · 최근 기록부터")).toBeInTheDocument();
    more.focus();
    await user.keyboard("{Enter}");
    expect(screen.getAllByRole("button", { name: "아티클 퀴즈 삭제" })).toHaveLength(40);
    expect(more).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(screen.getAllByRole("button", { name: "아티클 퀴즈 삭제" })).toHaveLength(45);
    expect(screen.queryByRole("button", { name: "이전 아티클 더 보기" })).not.toBeInTheDocument();
  });

  it("opens a past article and returns to the list at the top of each view", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue(articleHistory(22));
    vi.mocked(fetchArticleInstance).mockResolvedValue(sampleDraw);
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await screen.findByText("[BBC] Story 21");
    const page = screen.getByRole("main");
    page.scrollTop = 800;
    await user.click(screen.getByText("[BBC] Story 2").closest("button")!);
    expect(fetchArticleInstance).toHaveBeenCalledWith("i2");
    expect(await screen.findByText(articleSummaryMatcher(sampleDraw.summary))).toBeInTheDocument();
    expect(page.scrollTop).toBe(0);

    page.scrollTop = 500;
    await user.click(screen.getByRole("button", { name: "← 목록으로" }));
    expect(await screen.findByRole("heading", { name: "나의 아티클 기록" })).toBeInTheDocument();
    expect(page.scrollTop).toBe(0);
    expect(screen.queryByText("[BBC] Story 0")).not.toBeInTheDocument();
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
    expect(screen.getByText("총 0개 · 최근 기록부터")).toBeInTheDocument();
  });
});

describe("ArticleQuiz page — draw / reading / quiz / result flow", () => {
  it("shows the reading view with the English summary after a successful draw", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));

    expect(await screen.findByText(articleSummaryMatcher(sampleDraw.summary))).toBeInTheDocument();
    expect(screen.getByText("[BBC] Scientists make discovery")).toBeInTheDocument();
    expect(screen.getByText(formatAbsoluteDate(new Date(sampleDraw.publishedAt * 1000)))).toBeInTheDocument();
  });

  it("omits the date line when publishedAt is unknown", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: { ...sampleDraw, publishedAt: 0 } });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));

    expect(await screen.findByText(articleSummaryMatcher(sampleDraw.summary))).toBeInTheDocument();
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

    expect(await screen.findByRole("alert")).toHaveTextContent("아티클을 가져오지 못했어요. 연결 상태를 확인한 뒤 ‘새 아티클 뽑기’를 다시 눌러 주세요.");
    const drawButton = screen.getByRole("button", { name: "새 아티클 뽑기" });
    const history = screen.getByRole("heading", { name: "나의 아티클 기록" });
    expect(drawButton.compareDocumentPosition(screen.getByRole("alert")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole("alert").compareDocumentPosition(history) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("reveals the quiz sub-questions only after tapping 문제풀기, alongside the English original", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    expect(screen.queryByText("틀린 해석")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "문제풀기" }));
    expect(screen.getByText("틀린 해석")).toBeInTheDocument();
    expect(screen.getByText("오늘")).toBeInTheDocument();
    expect(screen.getByText(articleSummaryMatcher(sampleDraw.summary))).toBeInTheDocument();
    expect(screen.queryByText(sampleDraw.translation)).not.toBeInTheDocument();
  });

  it("only enables submission once every sub-question has a pick, then submits all selections at once", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    vi.mocked(answerArticle).mockResolvedValue({
      correct: true,
      score: 2,
      total: 2,
      translation: sampleDraw.translation,
      subQuestions: [
        {
          prompt: sampleDraw.subQuestions[0].prompt,
          options: sampleDraw.subQuestions[0].options,
          correctOptionIndex: 0,
          selectedOptionIndex: 0,
          correct: true,
          explanation: "원문의 의미를 정확히 반영하기 때문입니다.",
        },
        {
          prompt: sampleDraw.subQuestions[1].prompt,
          options: sampleDraw.subQuestions[1].options,
          correctOptionIndex: 0,
          selectedOptionIndex: 0,
          correct: true,
          explanation: "원문에 명시되어 있습니다.",
        },
      ],
    });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "문제풀기" }));

    expect(screen.getByRole("button", { name: "답안 확인" })).toBeDisabled();

    const wrong = screen.getByRole("button", { name: "틀린 해석" });
    await user.click(wrong);
    expect(wrong).toHaveAttribute("aria-pressed", "true");
    await user.click(screen.getByRole("button", { name: "정확한 해석" }));
    expect(wrong).toHaveAttribute("aria-pressed", "false");
    expect(screen.getByRole("button", { name: "정확한 해석" })).toHaveAttribute("aria-pressed", "true");
    expect(answerArticle).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "답안 확인" })).toBeDisabled();

    await user.click(screen.getByRole("button", { name: "오늘" }));
    expect(screen.getByRole("button", { name: "답안 확인" })).not.toBeDisabled();

    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(answerArticle).toHaveBeenCalledWith("i1", [0, 0]);
    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
    expect(screen.getByText(sampleDraw.translation)).toBeInTheDocument();
    expect(screen.getByText("원문의 의미를 정확히 반영하기 때문입니다.")).toBeInTheDocument();
    expect(screen.getByText("원문에 명시되어 있습니다.")).toBeInTheDocument();
    expect(screen.getByText(articleSummaryMatcher(sampleDraw.summary))).toBeInTheDocument();
  });

  it("locks choices and submission until the submitted answers finish grading", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    let resolveAnswer!: (result: null) => void;
    vi.mocked(answerArticle).mockReturnValue(new Promise((resolve) => { resolveAnswer = resolve; }));
    const user = userEvent.setup();
    render(<ArticleQuiz />);
    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "문제풀기" }));
    await user.click(screen.getByRole("button", { name: "정확한 해석" }));
    await user.click(screen.getByRole("button", { name: "오늘" }));
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(screen.getByRole("button", { name: "채점 중…" })).toBeDisabled();
    for (const question of sampleDraw.subQuestions) {
      for (const button of within(screen.getByRole("group", { name: question.prompt })).getAllByRole("button")) {
        expect(button).toBeDisabled();
      }
    }
    expect(answerArticle).toHaveBeenCalledExactlyOnceWith("i1", [0, 0]);

    await act(async () => { resolveAnswer(null); });
    expect(screen.getByRole("button", { name: "답안 확인" })).toBeEnabled();
    const choice = screen.getByRole("button", { name: "정확한 해석" });
    expect(choice).toBeEnabled();
    expect(choice).toHaveAttribute("aria-pressed", "true");
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
    expect(screen.getByText(articleSummaryMatcher(sampleDraw.summary))).toBeInTheDocument();
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

  it("opens a past article with the keyboard and hides the list introduction", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([
      { id: "i1", source: "BBC", title: "Old story", summary: "s", answered: true, correct: true, createdAt: 1700000000, publishedAt: 0, status: "done" as const },
    ]);
    vi.mocked(fetchArticleInstance).mockResolvedValue(sampleDraw);
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    const article = await screen.findByRole("button", { name: /\[BBC\] Old story/ });
    article.focus();
    await user.keyboard("{Enter}");

    expect(fetchArticleInstance).toHaveBeenCalledWith("i1");
    expect(await screen.findByText(articleSummaryMatcher(sampleDraw.summary))).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "문제풀기" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "새로운 이야기를 읽어 봐요" })).not.toBeInTheDocument();
  });

  it("points a plain <audio> element at the server-generated read-aloud URL and plays it when 읽어주기 is tapped", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    const audioSession = { type: "auto" };
    vi.stubGlobal("navigator", { ...navigator, audioSession });
    const user = userEvent.setup();
    const { container } = render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "🔊 읽어주기" }));

    // Regression guard: generation now happens server-side, once per shared
    // article (see lib/articles.ts's articleAudioURL) — this is just a plain
    // <audio src> pointed at it and played, the same shape as
    // Recordings.tsx's playback, not the old client-side kokoro.ts pipeline.
    const audioEl = container.querySelector("audio");
    expect(audioEl).not.toBeNull();
    expect(audioEl?.src).toContain(articleAudioURL(sampleDraw.id));
    expect(vi.mocked(HTMLMediaElement.prototype.play)).toHaveBeenCalled();

    // Regression guard: requesting the "ambient" audio session type must run
    // synchronously in the same click as play(), before the network request
    // for the audio even starts — see handleRead's doc comment — so
    // read-aloud mixes with (never pauses) music already playing in another
    // app.
    expect(audioSession.type).toBe("ambient");
  });

  it("plays read-aloud at the playback rate configured in the hamburger menu's global setting", async () => {
    localStorage.setItem("buddy.tts.playbackRate", JSON.stringify(0.7));
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    const user = userEvent.setup();
    const { container } = render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "🔊 읽어주기" }));

    const audioEl = container.querySelector("audio") as HTMLAudioElement;
    expect(audioEl.playbackRate).toBe(0.7);
  });

  it("shows 불러오는 중… while buffering, then 재생 중… once the audio element actually starts playing", async () => {
    // Regression guard: showing "재생 중…" before playback has actually
    // started is misleading — the label is driven by the <audio> element's
    // own waiting/playing events, not assumed the instant the button is
    // tapped, so it never claims audio is playing when it's still loading/
    // generating server-side.
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    const user = userEvent.setup();
    const { container } = render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "🔊 읽어주기" }));

    expect(await screen.findByRole("button", { name: "불러오는 중…" })).toBeInTheDocument();

    const audioEl = container.querySelector("audio")!;
    await act(async () => audioEl.dispatchEvent(new Event("playing")));

    expect(await screen.findByRole("button", { name: "재생 중…" })).toBeInTheDocument();

    await act(async () => audioEl.dispatchEvent(new Event("ended")));
    expect(await screen.findByRole("button", { name: "🔊 읽어주기" })).toBeInTheDocument();
  });

  it("cancels read-aloud while it is buffering or playing", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    const user = userEvent.setup();
    const { container } = render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "🔊 읽어주기" }));
    const audioEl = container.querySelector("audio") as HTMLAudioElement;

    expect(screen.getByRole("button", { name: "취소" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "취소" }));

    expect(vi.mocked(HTMLMediaElement.prototype.pause)).toHaveBeenCalled();
    expect(audioEl.currentTime).toBe(0);
    expect(await screen.findByRole("button", { name: "🔊 읽어주기" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "취소" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "🔊 읽어주기" }));
    await act(async () => audioEl.dispatchEvent(new Event("playing")));
    await user.click(screen.getByRole("button", { name: "취소" }));

    expect(vi.mocked(HTMLMediaElement.prototype.pause)).toHaveBeenCalledTimes(2);
    expect(await screen.findByRole("button", { name: "🔊 읽어주기" })).toBeInTheDocument();
  });

  it("resets the read-aloud label to idle after leaving mid-playback and reopening", async () => {
    // Regression guard: navigating back to the list while "재생 중…" is
    // showing does stop the actual audio (the reading view's <audio>
    // element unmounts with it), but that unmount never fires the element's
    // own onEnded/onError — nothing reset the tts label state itself, so
    // reopening any draw afterward showed a stale "재생 중…" even though
    // nothing was actually playing.
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    const user = userEvent.setup();
    const { container } = render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "🔊 읽어주기" }));
    const audioEl = container.querySelector("audio")!;
    await act(async () => audioEl.dispatchEvent(new Event("playing")));
    expect(await screen.findByRole("button", { name: "재생 중…" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "← 목록으로" }));
    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));

    expect(await screen.findByRole("button", { name: "🔊 읽어주기" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "재생 중…" })).not.toBeInTheDocument();
  });

  it("shows a failure label instead of silently going back to idle when playback fails", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    const user = userEvent.setup();
    const { container } = render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "🔊 읽어주기" }));

    const audioEl = container.querySelector("audio")!;
    await act(async () => audioEl.dispatchEvent(new Event("error")));

    expect(await screen.findByRole("button", { name: "재생 실패, 다시 시도해주세요" })).toBeInTheDocument();
  });

  it("shows a partial-credit incorrect reveal when a sub-question was missed", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    vi.mocked(answerArticle).mockResolvedValue({
      correct: false,
      score: 1,
      total: 2,
      translation: sampleDraw.translation,
      subQuestions: [
        {
          prompt: sampleDraw.subQuestions[0].prompt,
          options: sampleDraw.subQuestions[0].options,
          correctOptionIndex: 0,
          selectedOptionIndex: 1,
          correct: false,
          explanation: "왜냐하면",
        },
        {
          prompt: sampleDraw.subQuestions[1].prompt,
          options: sampleDraw.subQuestions[1].options,
          correctOptionIndex: 0,
          selectedOptionIndex: 0,
          correct: true,
          explanation: "원문에 명시되어 있습니다.",
        },
      ],
    });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "문제풀기" }));
    await user.click(screen.getByRole("button", { name: "틀린 해석" }));
    await user.click(screen.getByRole("button", { name: "오늘" }));
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText("아쉬워요, 1/2 정답이에요.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "틀린 해석" })).toHaveClass("incorrect");
    expect(screen.getByRole("button", { name: "정확한 해석" })).toHaveClass("correct");
    expect(screen.getByRole("button", { name: "오늘" })).toHaveClass("correct");
    expect(screen.getByRole("button", { name: "틀린 해석" })).toBeDisabled();
  });
});

describe("ArticleQuiz page — word lookup while reading", () => {
  it("centers the panel only when the tapped word is too close to a viewport edge", () => {
    expect(shouldCenterWordLookup(100, 320, 390)).toBe(true);
    expect(shouldCenterWordLookup(40, 320, 390)).toBe(false);
    expect(shouldCenterWordLookup(0, 320, 390)).toBe(true);
  });

  it("uses the same anchored panel for responsive placement", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "discovery" }));

    const panel = screen.getByRole("menu");
    const selectedWord = screen.getByRole("button", { name: "discovery" });
    expect(panel).toHaveClass("word-lookup-panel");
    expect(selectedWord).toHaveClass("selected");
    expect(selectedWord).toHaveAttribute("aria-expanded", "true");
    // Placement is controlled by the orientation media query in styles.css:
    // portrait remains below the token, while landscape centers this same
    // panel in the viewport so an edge token cannot clip it.
    expect(panel.closest(".article-word-anchor")).toBeInTheDocument();
    expect(panel.closest(".article-summary")?.tagName).toBe("DIV");
  });

  it("waits for confirmation before looking up a tapped word, then saves it to the vocabulary list", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    vi.mocked(defineWord).mockResolvedValue({
      word: "discovery",
      meaning: "발견",
      example: "Scientists announced a new discovery today.",
    });
    vi.mocked(saveWord).mockResolvedValue({
      id: "w1",
      word: "discovery",
      meaning: "발견",
      example: "Scientists announced a new discovery today.",
      stage: 0,
      reviewCount: 0,
      nextReviewAt: 0,
      status: "pending",
    });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "discovery" }));

    expect(defineWord).not.toHaveBeenCalled();
    expect(await screen.findByRole("button", { name: "찾기" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "찾기" }).closest(".article-word-anchor"))
      .toHaveClass("article-word-anchor");
    expect(screen.getByRole("button", { name: "찾기" }).closest(".article-summary"))
      .toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "찾기" }));

    expect(defineWord).toHaveBeenCalledWith(sampleDraw.id, "discovery", 9);
    expect(await screen.findByText("발견")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "학습하기" }));

    expect(saveWord).toHaveBeenCalledWith({
      word: "discovery",
      meaning: "발견",
      example: "Scientists announced a new discovery today.",
    }, "discovery");
    expect(await screen.findByRole("button", { name: "✓ 확인 중" })).toBeInTheDocument();

    // Reopening the same token uses the successful result immediately, so
    // there is no second confirmation or lookup request.
    await user.click(screen.getByRole("button", { name: "단어 뜻 닫기" }));
    await user.click(screen.getByRole("button", { name: "discovery" }));
    expect(screen.queryByRole("button", { name: "찾기" })).not.toBeInTheDocument();
    expect(screen.getByText("발견")).toBeInTheDocument();
    expect(defineWord).toHaveBeenCalledTimes(1);
  });

  it("shows a server-cached lookup immediately without the find button", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    vi.mocked(checkDefinedWordStatus).mockResolvedValue({
      status: "done",
      result: {
        word: "discovery",
        meaning: "발견",
        example: "Scientists announced a new discovery today.",
      },
    });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "discovery" }));

    expect(await screen.findByText("발견")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "찾기" })).not.toBeInTheDocument();
    expect(defineWord).not.toHaveBeenCalled();
    expect(checkDefinedWordStatus).toHaveBeenCalledWith(sampleDraw.id, "discovery", 9);

    // A cached result must also populate the searched-words overlay; the
    // popover and the overlay represent the same lookup.
    const searchedWordsButton = screen.getByRole("button", { name: /검색한 단어 1/ });
    await user.click(searchedWordsButton);
    const searchedWordsPanel = screen.getByRole("dialog", { name: "검색한 단어 목록" });
    expect(within(searchedWordsPanel).getByText("발견")).toBeInTheDocument();
    expect(within(searchedWordsPanel).queryByText("뜻을 가져오지 못했어요.")).not.toBeInTheDocument();
  });

  it("resumes a server-pending lookup without asking the learner to search again", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    vi.mocked(checkDefinedWordStatus).mockResolvedValue({ status: "pending" });
    vi.mocked(defineWord).mockResolvedValue({
      word: "discovery",
      meaning: "발견",
      example: "A new discovery.",
    });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "discovery" }));

    expect(await screen.findByText("발견")).toBeInTheDocument();
    expect(defineWord).toHaveBeenCalledWith(sampleDraw.id, "discovery", 9);
    expect(screen.queryByRole("button", { name: "찾기" })).not.toBeInTheDocument();
  });

  it("keeps searched words in a bottom overlay so they can be saved later", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    vi.mocked(defineWord).mockResolvedValue({
      word: "discovery",
      meaning: "발견",
      example: "Scientists announced a new discovery today.",
    });
    vi.mocked(saveWord).mockResolvedValue({
      id: "w1",
      word: "discovery",
      meaning: "발견",
      example: "Scientists announced a new discovery today.",
      stage: 0,
      reviewCount: 0,
      nextReviewAt: 0,
      status: "pending",
    });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "discovery" }));
    expect(screen.queryByRole("button", { name: /검색한 단어/ })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "찾기" }));
    expect(await screen.findByText("발견")).toBeInTheDocument();

    const searchedWordsButton = screen.getByRole("button", { name: /검색한 단어 1/ });
    expect(searchedWordsButton).toBeInTheDocument();
    await user.click(searchedWordsButton);
    expect(screen.getByRole("dialog", { name: "검색한 단어 목록" })).toBeInTheDocument();
    const searchedWordsPanel = screen.getByRole("dialog", { name: "검색한 단어 목록" });
    expect(within(searchedWordsPanel).getByText("discovery")).toBeInTheDocument();
    expect(within(searchedWordsPanel).getAllByText("발견").length).toBeGreaterThan(0);

    await user.click(within(searchedWordsPanel).getAllByRole("button", { name: "학습하기" })[0]);
    expect(saveWord).toHaveBeenCalledWith({
      word: "discovery",
      meaning: "발견",
      example: "Scientists announced a new discovery today.",
    }, "discovery");
    expect(await screen.findByRole("button", { name: "✓ 확인 중" })).toBeInTheDocument();
  });

  it("keeps the same word lookup and search history available while reviewing the answer", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    vi.mocked(defineWord).mockImplementation(async (_articleID, word) => ({
      word,
      meaning: word === "discovery" ? "발견" : "과학자들",
      example: word === "discovery" ? "A new discovery." : "Scientists announced the result.",
    }));
    vi.mocked(answerArticle).mockResolvedValue({
      correct: true,
      score: 2,
      total: 2,
      translation: sampleDraw.translation,
      subQuestions: sampleDraw.subQuestions.map((sub) => ({
        ...sub,
        correctOptionIndex: 0,
        selectedOptionIndex: 0,
        correct: true,
        explanation: "원문에 나온 내용입니다.",
      })),
    });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "discovery" }));
    await user.click(await screen.findByRole("button", { name: "찾기" }));
    expect(await screen.findByText("발견")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "문제풀기" }));
    await user.click(screen.getByRole("button", { name: "정확한 해석" }));
    await user.click(screen.getByRole("button", { name: "오늘" }));
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
    const historyButton = screen.getByRole("button", { name: /검색한 단어 1/ });
    await user.click(historyButton);
    const history = screen.getByRole("dialog", { name: "검색한 단어 목록" });
    expect(within(history).getByText("discovery")).toBeInTheDocument();
    expect(within(history).getByText("발견")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "검색한 단어 목록 닫기" }));
    await user.click(screen.getByRole("button", { name: "Scientists" }));
    await user.click(await screen.findByRole("button", { name: "찾기" }));
    expect(await screen.findByText("과학자들")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /검색한 단어 2/ })).toBeInTheDocument();

    expect(JSON.parse(localStorage.getItem("buddy.article.searched-words.i1") ?? "[]"))
      .toEqual(expect.arrayContaining([
        expect.objectContaining({ word: "discovery", result: expect.objectContaining({ meaning: "발견" }) }),
        expect.objectContaining({ word: "Scientists", result: expect.objectContaining({ meaning: "과학자들" }) }),
      ]));
  });

  it("restores an article's searched words after returning to the list", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([{
      id: "i1",
      source: "BBC",
      title: sampleDraw.title,
      summary: sampleDraw.summary,
      answered: false,
      correct: false,
      createdAt: 1710494400,
      publishedAt: sampleDraw.publishedAt,
      status: "done",
    }]);
    vi.mocked(fetchArticleInstance).mockResolvedValue(sampleDraw);
    localStorage.setItem("buddy.article.searched-words.i1", JSON.stringify([{
      key: 9,
      word: "discovery",
      result: { word: "discovery", meaning: "발견", example: "A discovery." },
    }]));
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByText("[BBC] Scientists make discovery"));
    await user.click(screen.getByRole("button", { name: /검색한 단어 1/ }));
    const panel = screen.getByRole("dialog", { name: "검색한 단어 목록" });
    expect(within(panel).getByText("discovery")).toBeInTheDocument();
    expect(within(panel).getAllByText("발견").length).toBeGreaterThan(0);
  });

  it("restarts polling for an unfinished stored lookup after returning to the article", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([{
      id: "i1",
      source: "BBC",
      title: sampleDraw.title,
      summary: sampleDraw.summary,
      answered: false,
      correct: false,
      createdAt: 1710494400,
      publishedAt: sampleDraw.publishedAt,
      status: "done",
    }]);
    vi.mocked(fetchArticleInstance).mockResolvedValue(sampleDraw);
    vi.mocked(defineWord).mockResolvedValue({ word: "discovery", meaning: "발견", example: "A discovery." });
    localStorage.setItem("buddy.article.searched-words.i1", JSON.stringify([{
      key: 9,
      word: "discovery",
      result: null,
    }]));
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByText("[BBC] Scientists make discovery"));
    await vi.waitFor(() => expect(defineWord).toHaveBeenCalledWith(sampleDraw.id, "discovery", 9));
    await user.click(screen.getByRole("button", { name: /검색한 단어 1/ }));

    expect(within(screen.getByRole("dialog", { name: "검색한 단어 목록" })).getByText("발견")).toBeInTheDocument();
  });

  it("shows a failure message when the lookup fails", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    vi.mocked(defineWord).mockResolvedValue(null);
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "discovery" }));
    await user.click(screen.getByRole("button", { name: "찾기" }));

    expect(await screen.findByText("뜻을 가져오지 못했어요.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "단어 뜻 닫기" }));
    await user.click(screen.getByRole("button", { name: "discovery" }));
    expect(screen.getByText("뜻을 가져오지 못했어요.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "찾기" })).not.toBeInTheDocument();
  });

  it("keeps showing a lookup as in progress when reopening its word", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    let resolveLookup!: (result: { word: string; meaning: string; example: string }) => void;
    vi.mocked(defineWord).mockReturnValue(
      new Promise((resolve) => {
        resolveLookup = resolve;
      }),
    );
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    await user.click(screen.getByRole("button", { name: "discovery" }));
    await user.click(screen.getByRole("button", { name: "찾기" }));
    await user.click(screen.getByRole("button", { name: "단어 뜻 닫기" }));
    await user.click(screen.getByRole("button", { name: "discovery" }));

    expect(screen.getByText("찾는 중…")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "찾기" })).not.toBeInTheDocument();

    resolveLookup({ word: "discovery", meaning: "발견", example: "A new discovery." });
    expect(await screen.findByText("발견")).toBeInTheDocument();
  });
});
