/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
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

import { ArticleQuiz } from "./ArticleQuiz";
import {
  answerArticle,
  articleAudioURL,
  deleteArticleInstance,
  drawArticle,
  fetchArticleInstance,
  fetchArticleInstances,
  type ArticleDraw,
} from "../lib/articles";
import { formatAbsoluteDate, formatDateDivider } from "../lib/time";

// jsdom doesn't implement HTMLMediaElement.play() — stub it so handleRead's
// el.play() resolves instead of throwing "not implemented", the same reason
// kokoro.test.ts stubs the global Audio constructor for its own (unrelated,
// client-side chat read-aloud) tests.
beforeEach(() => {
  HTMLMediaElement.prototype.play = vi.fn().mockResolvedValue(undefined);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.useRealTimers();
  vi.unstubAllGlobals();
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

  it("reveals the quiz choices only after tapping 문제풀기, alongside the English original", async () => {
    vi.mocked(fetchArticleInstances).mockResolvedValue([]);
    vi.mocked(drawArticle).mockResolvedValue({ status: "ok", draw: sampleDraw });
    const user = userEvent.setup();
    render(<ArticleQuiz />);

    await user.click(await screen.findByRole("button", { name: "새 아티클 뽑기" }));
    expect(screen.queryByText("틀린 해석 1")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "문제풀기" }));
    expect(screen.getByText("틀린 해석 1")).toBeInTheDocument();
    expect(screen.getByText(sampleDraw.summary)).toBeInTheDocument();
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
    expect(screen.getByText(sampleDraw.summary)).toBeInTheDocument();
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
    expect(screen.queryByText(/무음 스위치/)).not.toBeInTheDocument();

    const audioEl = container.querySelector("audio")!;
    await act(async () => audioEl.dispatchEvent(new Event("playing")));

    expect(await screen.findByRole("button", { name: "재생 중…" })).toBeInTheDocument();
    expect(await screen.findByText(/무음 스위치/)).toBeInTheDocument();

    await act(async () => audioEl.dispatchEvent(new Event("ended")));
    expect(await screen.findByRole("button", { name: "🔊 읽어주기" })).toBeInTheDocument();
    expect(screen.queryByText(/무음 스위치/)).not.toBeInTheDocument();
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
