/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../lib/wordReview", async () => {
  const actual = await vi.importActual<typeof import("../lib/wordReview")>("../lib/wordReview");
  return {
    ...actual,
    fetchWords: vi.fn(),
    deleteWord: vi.fn(),
    reviewWord: vi.fn(),
    startResearchWord: vi.fn(),
    confirmResearchWord: vi.fn(),
    startAutoAddWords: vi.fn(),
    fetchAutoAddStatus: vi.fn(),
  };
});

vi.mock("../lib/quizCheck", async () => {
  const actual = await vi.importActual<typeof import("../lib/quizCheck")>("../lib/quizCheck");
  return { ...actual, checkQuizAnswer: vi.fn() };
});

import { WordReview } from "./WordReview";
import {
  deleteWord,
  fetchAutoAddStatus,
  fetchWords,
  reviewWord,
  startResearchWord,
  confirmResearchWord,
  startAutoAddWords,
  type WordReviewItem,
} from "../lib/wordReview";
import { formatAbsoluteDateTime } from "../lib/time";
import { checkQuizAnswer } from "../lib/quizCheck";

// WordReview.tsx checks for an in-flight auto-add job on every mount (so
// reopening the page resumes watching one — see the mount effect), so every
// test needs some default here or that unconditional call would reject
// against an unmocked vi.fn(). Individual tests below override it.
const testNow = Date.UTC(2026, 8, 20, 12);

beforeEach(() => {
  vi.spyOn(Date, "now").mockReturnValue(testNow);
  vi.mocked(fetchAutoAddStatus).mockResolvedValue({ status: "", count: 0 });
  vi.mocked(checkQuizAnswer).mockResolvedValue(false);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

const dueWord: WordReviewItem = {
  id: "w1",
  word: "ecstatic",
  meaning: "매우 행복한",
  example: "She was ecstatic.",
  stage: 0,
  reviewCount: 0,
  lastReviewedAt: Math.floor(testNow / 1000) - 8 * 24 * 3600,
  nextReviewAt: Math.floor(testNow / 1000) - 3600, // 1 hour ago: due
  status: "verified",
  researchStatus: "confirmed",
  reviewQuestion: { version: 2, prompt: "She was ___.", answers: ["ecstatic"] },
};

const idiomWord: WordReviewItem = {
  id: "w-idiom",
  word: "do one's best",
  meaning: "최선을 다하다",
  example: "I will do my best to finish the project on time.",
  stage: 0,
  reviewCount: 0,
  nextReviewAt: Math.floor(testNow / 1000) - 3600, // 1 hour ago: due
  status: "verified",
  researchStatus: "confirmed",
  reviewQuestion: {
    version: 2,
    prompt: "Even though the task was complicated, he promised to ___ his ___ for the sake of the team.",
    answers: ["do", "best"],
  },
};

const optimizeWord: WordReviewItem = {
  id: "w-optimize",
  word: "optimize",
  meaning: "최적화하다",
  example: "We are optimizing the search algorithm for faster results.",
  stage: 0,
  reviewCount: 0,
  nextReviewAt: Math.floor(testNow / 1000) - 3600, // 1 hour ago: due
  status: "verified",
  researchStatus: "confirmed",
  reviewQuestion: { version: 2, prompt: "We are ___ the search algorithm for faster results.", answers: ["optimizing"] },
};

const organizeWord: WordReviewItem = {
  id: "w-organize",
  word: "organize",
  meaning: "조직하다, 체계화하다",
  example: "The criminals planned the attack in a highly organized manner.",
  stage: 0,
  reviewCount: 0,
  nextReviewAt: Math.floor(testNow / 1000) - 3600,
  status: "verified",
  researchStatus: "confirmed",
  reviewQuestion: {
    version: 2,
    prompt: "The criminals planned the attack in a highly ___ manner.",
    answers: ["organized"],
  },
};

const futureWord: WordReviewItem = {
  id: "w2",
  word: "elated",
  meaning: "신이 난",
  example: "He was elated.",
  stage: 1,
  reviewCount: 1,
  nextReviewAt: Math.floor(testNow / 1000) + 48 * 3600, // in 2 days: not due
  status: "verified",
  researchStatus: "confirmed",
};

const pendingWord: WordReviewItem = {
  id: "w-pending",
  word: "wistful",
  meaning: "아쉬워하는",
  example: "a wistful smile",
  stage: 0,
  reviewCount: 0,
  nextReviewAt: 0,
  status: "pending",
};

const rejectedWord: WordReviewItem = {
  id: "w-rejected",
  word: "xyzzy",
  meaning: "존재하지 않는 단어",
  example: "xyzzy the door.",
  stage: 0,
  reviewCount: 0,
  nextReviewAt: 0,
  status: "rejected",
  verifyReason: "실제로 사용되는 영단어가 아니에요",
};

// Seven other verified words distinct from dueWord, so a recognition-mode
// question about dueWord has enough distractor meanings (see
// minRecognitionDistractors in WordReview.tsx).
const otherVerifiedWords: WordReviewItem[] = [
  { id: "w3", word: "gloomy", meaning: "우울한", example: "a gloomy day", stage: 0, reviewCount: 0, nextReviewAt: Math.floor(testNow / 1000) + 999999, status: "verified", researchStatus: "confirmed" },
  { id: "w4", word: "jaded", meaning: "지친", example: "a jaded look", stage: 0, reviewCount: 0, nextReviewAt: Math.floor(testNow / 1000) + 999999, status: "verified", researchStatus: "confirmed" },
  { id: "w5", word: "content", meaning: "만족하는", example: "feeling content", stage: 0, reviewCount: 0, nextReviewAt: Math.floor(testNow / 1000) + 999999, status: "verified", researchStatus: "confirmed" },
  { id: "w6", word: "vivid", meaning: "생생한", example: "a vivid memory", stage: 0, reviewCount: 0, nextReviewAt: Math.floor(testNow / 1000) + 999999, status: "verified", researchStatus: "confirmed" },
  { id: "w7", word: "fragile", meaning: "깨지기 쉬운", example: "a fragile vase", stage: 0, reviewCount: 0, nextReviewAt: Math.floor(testNow / 1000) + 999999, status: "verified", researchStatus: "confirmed" },
  { id: "w8", word: "reliable", meaning: "믿을 수 있는", example: "a reliable source", stage: 0, reviewCount: 0, nextReviewAt: Math.floor(testNow / 1000) + 999999, status: "verified", researchStatus: "confirmed" },
  { id: "w9", word: "concise", meaning: "간결한", example: "a concise answer", stage: 0, reviewCount: 0, nextReviewAt: Math.floor(testNow / 1000) + 999999, status: "verified", researchStatus: "confirmed" },
];

// Mocks fetchWords to return `words` (always dueCount: 1 — every quiz test
// below cares about exactly one due word, even when the list also carries
// extra non-due entries for recognition-mode distractors), renders
// WordReview, and starts the quiz — the shared preamble every quiz test
// below needs before it can get to what it's actually testing.
async function startQuiz(words: WordReviewItem[] = [dueWord]) {
  vi.mocked(fetchWords).mockResolvedValue({ words, dueCount: 1 });
  const user = userEvent.setup();
  render(<WordReview />);
  await user.click(await screen.findByRole("button", { name: "복습 시작" }));
  return user;
}

// jsdom has no layout. Model the page height to exercise page-level
// pagination and verify that loading data never jumps past the introduction.
function mockScrollHeights() {
  vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockReturnValue(1800);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(600);
}

describe("WordReview page", () => {
  it("shows a welcoming introduction with the shared hamburger navigation", () => {
    vi.mocked(fetchWords).mockReturnValue(new Promise(() => {}));
    render(<WordReview />);
    expect(screen.getByRole("button", { name: "메뉴" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "배운 표현을 내 것으로 만들어요" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "홈으로" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "대화로 돌아가기" })).not.toBeInTheDocument();
  });

  it("shows a loading hint before the fetch resolves", () => {
    vi.mocked(fetchWords).mockReturnValue(new Promise(() => {}));
    render(<WordReview />);
    expect(screen.getByText("불러오는 중…")).toBeInTheDocument();
  });

  it("shows an error hint when the fetch fails", async () => {
    vi.mocked(fetchWords).mockResolvedValue(null);
    render(<WordReview />);
    expect(await screen.findByText(/단어 목록을 불러오지 못했어요/)).toBeInTheDocument();
  });

  it("shows an empty-state hint when there are no tracked words", async () => {
    mockScrollHeights();
    vi.mocked(fetchWords).mockResolvedValue({ words: [], dueCount: 0 });
    render(<WordReview />);
    expect(await screen.findByText(/아직 학습 중인 단어가 없어요/)).toBeInTheDocument();
    expect(screen.getByText("지금 복습할 단어가 없어요.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "복습 시작" })).not.toBeInTheDocument();
    // The "복습 시작" slot is taken over by the auto-add button instead of
    // being left empty, offering a second path besides manual 🔎 search.
    expect(screen.getByRole("button", { name: "새 단어 추가로 학습하기" })).toBeInTheDocument();
    expect(screen.getByRole("main").scrollTop).toBe(0);
  });

  it("shows the due count and a start button when words are due", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [dueWord], dueCount: 1 });
    render(<WordReview />);
    expect(await screen.findByText("복습할 단어 1개가 있어요.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "복습 시작" })).toBeInTheDocument();
  });

  it("ignores a due legacy question while its current version is regenerated", async () => {
    const legacy = {
      ...dueWord,
      reviewQuestion: { version: 1, prompt: "She was ___.", answers: ["ecstatic"] },
    };
    vi.mocked(fetchWords).mockResolvedValue({ words: [legacy], dueCount: 0 });
    render(<WordReview />);

    expect(await screen.findByText("복습 문제 1개를 새 버전으로 준비 중이에요.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "복습 문제 준비 중…" })).toBeDisabled();
    expect(screen.queryByRole("button", { name: "복습 시작" })).not.toBeInTheDocument();
  });

  it("places the review action before the grouped word history like other learning pages", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [dueWord, pendingWord, rejectedWord], dueCount: 1 });
    render(<WordReview />);

    const word = await screen.findByText(dueWord.word);
    const dueHint = screen.getByText("복습할 단어 1개가 있어요.");
    const startButton = screen.getByRole("button", { name: "복습 시작" });
    expect(startButton.compareDocumentPosition(word) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(dueHint.nextElementSibling).toBe(startButton);
    for (const [title, item] of [["복습중인 단어", dueWord], ["확정 전 단어", pendingWord], ["제외된 단어", rejectedWord]] as const) {
      const group = within(screen.getByRole("region", { name: title }));
      expect(group.getByText(item.word)).toBeInTheDocument();
      expect(group.getByRole("button", { name: "단어 삭제" })).toBeInTheDocument();
    }
  });

  it("keeps the introduction at the top after words finish loading", async () => {
    mockScrollHeights();
    let resolveWords!: (result: { words: WordReviewItem[]; dueCount: number }) => void;
    vi.mocked(fetchWords).mockReturnValue(new Promise((resolve) => { resolveWords = resolve; }));
    render(<WordReview />);
    const page = screen.getByRole("main");
    expect(page.scrollTop).toBe(0);

    await act(async () => { resolveWords({ words: [dueWord, futureWord], dueCount: 1 }); });

    expect(page.scrollTop).toBe(0);
    expect(page.querySelector(".word-list-scroll")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "복습 시작" })).toBeEnabled();
  });

  it("opens the quiz and the returning list at the top", async () => {
    mockScrollHeights();
    const user = await startQuiz();
    const page = screen.getByRole("main");
    expect(page.scrollTop).toBe(0);
    expect(screen.getByRole("heading", { name: "문장 속 단어 떠올리기" })).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "정답 입력" })).toBeInTheDocument();
    page.scrollTop = 300;

    await user.click(screen.getByRole("button", { name: "← 목록으로" }));

    expect(page.scrollTop).toBe(0);
    expect(screen.getByRole("button", { name: "복습 시작" })).toBeEnabled();
  });

  it("preserves the page position and expanded history when polling adds words", async () => {
    vi.useFakeTimers();
    mockScrollHeights();
    const words = Array.from({ length: 21 }, (_, i) => ({ ...futureWord, id: `w-${i}`, word: `word-${i}` }));
    vi.mocked(fetchWords)
      .mockResolvedValueOnce({ words, dueCount: 0 })
      .mockResolvedValue({ words: [...words, pendingWord], dueCount: 0 });
    await act(async () => { render(<WordReview />); });
    fireEvent.click(screen.getByRole("button", { name: "이전 단어 더 보기" }));
    const history = screen.getByRole("region", { name: "복습중인 단어" });
    expect(within(history).getAllByRole("listitem")).toHaveLength(21);
    const page = screen.getByRole("main");
    page.scrollTop = 300;

    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });

    expect(screen.getByText(pendingWord.word)).toBeInTheDocument();
    expect(within(history).getAllByRole("listitem")).toHaveLength(21);
    expect(page.scrollTop).toBe(300);
  });

  it("lists each tracked word with its meaning and next review time", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [futureWord], dueCount: 0 });
    render(<WordReview />);
    expect(await screen.findByText("elated")).toBeInTheDocument();
    expect(screen.getByText("신이 난")).toBeInTheDocument();
    const expectedNextReview = formatAbsoluteDateTime(futureWord.nextReviewAt);
    expect(screen.getByText((content) => content.includes(expectedNextReview))).toBeInTheDocument();
  });

  it.each(["scroll", "button"])("lists recent reviews first and loads older words using %s", async (method) => {
    mockScrollHeights();
    const manyWords = Array.from({ length: 21 }, (_, i): WordReviewItem => ({
      ...futureWord,
      id: `review-${i}`,
      word: `review-word-${i}`,
      lastReviewedAt: (i + 1) * 1000,
    }));
    vi.mocked(fetchWords).mockResolvedValue({ words: manyWords, dueCount: 0 });
    render(<WordReview />);

    const history = await screen.findByRole("region", { name: "복습중인 단어" });
    expect(within(history).getByText("21개")).toBeInTheDocument();
    expect(screen.queryByText("review-word-0")).not.toBeInTheDocument();
    const visibleWords = within(history).getAllByRole("listitem");
    expect(visibleWords).toHaveLength(20);
    expect(visibleWords[0]).toHaveTextContent("review-word-20");
    expect(visibleWords.at(-1)).toHaveTextContent("review-word-1");

    const page = screen.getByRole("main");
    page.scrollTop = 1200;
    if (method === "scroll") fireEvent.scroll(page);
    else fireEvent.click(screen.getByRole("button", { name: "이전 단어 더 보기" }));

    expect(within(history).getAllByRole("listitem")).toHaveLength(21);
    expect(within(history).getAllByRole("listitem").at(-1)).toHaveTextContent("review-word-0");
    expect(screen.queryByRole("button", { name: "이전 단어 더 보기" })).not.toBeInTheDocument();
    expect(page.scrollTop).toBe(1200);
  });

  // A word never leaves review rotation, however many times it's been
  // answered correctly — it just gets a next-review date far out instead of
  // a fixed daily/weekly ceiling, so a growing word list doesn't pile more
  // and more reviews onto the same short cap (see wordreview's schedule
  // doc). The list view has no separate "완료" state to show instead.
  it("still shows a next-review date for a well-known word with a far-future schedule, not a terminal state", async () => {
    const wellKnownWord = { ...futureWord, stage: 9, nextReviewAt: Math.floor(testNow / 1000) + 200 * 86400 };
    vi.mocked(fetchWords).mockResolvedValue({ words: [wellKnownWord], dueCount: 0 });
    render(<WordReview />);
    const expectedNextReview = formatAbsoluteDateTime(wellKnownWord.nextReviewAt);
    expect(await screen.findByText((content) => content.includes(expectedNextReview))).toBeInTheDocument();
    expect(screen.queryByText("학습 완료")).not.toBeInTheDocument();
  });

  // Rejected words (failed the model-consensus check) must never be dropped
  // silently — they show up with the judge's reason so the learner can
  // review and delete them (see wordreview's package doc).
  it("shows a 제외된 단어 section with the rejection reason for rejected words", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [rejectedWord], dueCount: 0 });
    render(<WordReview />);

    expect(await screen.findByText("제외된 단어")).toBeInTheDocument();
    expect(screen.getByText("xyzzy")).toBeInTheDocument();
    expect(screen.getByText("실제로 사용되는 영단어가 아니에요")).toBeInTheDocument();
  });

  it("offers 확정 without searching and keeps an unconfirmed word out of review", async () => {
    const needsDecision = { ...dueWord, researchStatus: undefined };
    vi.mocked(fetchWords).mockResolvedValue({ words: [needsDecision], dueCount: 0 });
    render(<WordReview />);

    expect(await screen.findByRole("button", { name: "다시 검색" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "확정" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "복습 시작" })).not.toBeInTheDocument();
  });

  it("offers 다시 검색 and 확정 for a directly defined word", async () => {
    const articleWord = { ...dueWord, originalWord: "ecstatic", researchStatus: undefined };
    vi.mocked(fetchWords).mockResolvedValue({ words: [articleWord], dueCount: 0 });
    vi.mocked(startResearchWord).mockResolvedValue({ ...articleWord, researchStatus: "pending" });
    const user = userEvent.setup();
    render(<WordReview />);

    const researchButton = await screen.findByRole("button", { name: "다시 검색" });
    expect(researchButton).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "확정" })).toBeInTheDocument();

    await user.click(researchButton);
    expect(startResearchWord).toHaveBeenCalledWith(articleWord.id);
  });

  it("moves a rejected word into the confirmed review list and hides research controls", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [rejectedWord], dueCount: 0 });
    vi.mocked(confirmResearchWord).mockResolvedValue({
      ...rejectedWord,
      status: "verified",
      verifyReason: undefined,
      researchStatus: "confirmed",
    });
    const user = userEvent.setup();
    render(<WordReview />);

    await user.click(await screen.findByRole("button", { name: "확정" }));

    expect(confirmResearchWord).toHaveBeenCalledWith(rejectedWord.id);
    expect(screen.queryByText("제외된 단어")).not.toBeInTheDocument();
    expect(screen.getByText(rejectedWord.word)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "다시 검색" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "확정" })).not.toBeInTheDocument();
  });

  it("keeps confirmation disabled while a word is still being verified", async () => {
    vi.useFakeTimers();
    const verifiedWord = { ...pendingWord, status: "verified" as const };
    vi.mocked(fetchWords)
      .mockResolvedValueOnce({ words: [pendingWord], dueCount: 0 })
      .mockResolvedValue({ words: [verifiedWord], dueCount: 0 });
    await act(async () => { render(<WordReview />); });

    expect(screen.getByText("확정 전 단어")).toBeInTheDocument();
    expect(screen.getByText("wistful")).toBeInTheDocument();
    expect(screen.getByText("확정 전")).toBeInTheDocument();
    const verificationButton = screen.getByRole("button", { name: "검증 중…" });
    expect(verificationButton).toBeDisabled();
    expect(screen.queryByRole("button", { name: "확정" })).not.toBeInTheDocument();
    fireEvent.click(verificationButton);
    expect(confirmResearchWord).not.toHaveBeenCalled();

    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });

    expect(screen.getByRole("button", { name: "확정" })).toBeEnabled();
  });

  it("keeps decision queues ahead of the paginated review history", async () => {
    const unconfirmedWord = { ...dueWord, id: "w-unconfirmed", word: "uncertain", researchStatus: undefined };
    vi.mocked(fetchWords).mockResolvedValue({
      words: [rejectedWord, pendingWord, unconfirmedWord, dueWord],
      dueCount: 1,
    });
    render(<WordReview />);

    const reviewingTitle = await screen.findByText("복습중인 단어");
    const unconfirmedTitle = screen.getByText("확정 전 단어");
    const rejectedTitle = screen.getByText("제외된 단어");
    const startButton = screen.getByRole("button", { name: "복습 시작" });

    expect(startButton.compareDocumentPosition(unconfirmedTitle) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(unconfirmedTitle.compareDocumentPosition(rejectedTitle) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(rejectedTitle.compareDocumentPosition(reviewingTitle) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByText("uncertain")).toBeInTheDocument();
    expect(screen.getByText("wistful")).toBeInTheDocument();
  });

  it("does not bury decision queues behind a very large review history", async () => {
    const manyReviewedWords = Array.from({ length: 10_000 }, (_, i): WordReviewItem => ({
      ...futureWord,
      id: `review-${i}`,
      word: `review-word-${i}`,
      lastReviewedAt: i,
    }));
    vi.mocked(fetchWords).mockResolvedValue({
      words: [...manyReviewedWords, pendingWord, rejectedWord],
      dueCount: 0,
    });
    render(<WordReview />);

    const unconfirmedSection = await screen.findByRole("region", { name: "확정 전 단어" });
    const rejectedSection = screen.getByRole("region", { name: "제외된 단어" });
    const historySection = screen.getByRole("region", { name: "복습중인 단어" });

    expect(within(unconfirmedSection).getByText(pendingWord.word)).toBeInTheDocument();
    expect(within(rejectedSection).getByText(rejectedWord.word)).toBeInTheDocument();
    expect(unconfirmedSection.compareDocumentPosition(historySection) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(rejectedSection.compareDocumentPosition(historySection) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(historySection).getAllByRole("listitem")).toHaveLength(20);
  });

  it("can delete a rejected word", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [rejectedWord], dueCount: 0 });
    vi.mocked(deleteWord).mockResolvedValue(true);
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const user = userEvent.setup();
    render(<WordReview />);

    await user.click(await screen.findByRole("button", { name: "단어 삭제" }));

    expect(deleteWord).toHaveBeenCalledWith("w-rejected");
    expect(screen.queryByText("제외된 단어")).not.toBeInTheDocument();
  });

  // A pending/rejected word must never count toward dueCount or the quiz,
  // however past-due its (meaningless, pre-verification) nextReviewAt is —
  // only the verified word shows a start button here.
  it("keeps pending/rejected words out of the due count and start button", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [pendingWord, rejectedWord], dueCount: 0 });
    render(<WordReview />);

    expect(await screen.findByText("지금 복습할 단어가 없어요.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "복습 시작" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "새 단어 추가로 학습하기" })).toBeInTheDocument();
  });

  describe("auto-adding new words when the queue is empty", () => {
    it("shows the auto-add button instead of 복습 시작 once dueCount is 0, even with words already tracked", async () => {
      vi.mocked(fetchWords).mockResolvedValue({ words: [futureWord], dueCount: 0 });
      render(<WordReview />);

      expect(await screen.findByRole("button", { name: "새 단어 추가로 학습하기" })).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "복습 시작" })).not.toBeInTheDocument();
    });

    const newWord: WordReviewItem = {
      id: "w-new",
      word: "resilient",
      meaning: "회복력이 있는",
      example: "She stayed resilient.",
      stage: 0,
      reviewCount: 0,
      nextReviewAt: 0,
      status: "pending",
    };

    // startAutoAddWords only ever reserves the background job (see
    // asyncjob.KindWordAutoAdd) — it comes back "pending" immediately, and
    // the actual generation/save is only visible once pollAutoAdd's next
    // tick reads fetchAutoAddStatus() as "done", so these tests drive that
    // poll with fake timers, the same pattern as ArticleQuiz.test.tsx's
    // "generating hint" test.
    it("starts the background job and adds the returned words into the 확정 전 단어 section once it completes", async () => {
      vi.mocked(fetchWords)
        .mockResolvedValueOnce({ words: [], dueCount: 0 }) // initial load
        .mockResolvedValueOnce({ words: [newWord], dueCount: 0 }); // re-fetched once the job completes
      vi.mocked(startAutoAddWords).mockResolvedValue({ status: "pending", count: 0 });
      const user = userEvent.setup({ delay: null });
      render(<WordReview />);
      const button = await screen.findByRole("button", { name: "새 단어 추가로 학습하기" });

      vi.useFakeTimers({ shouldAdvanceTime: true });
      await user.click(button);
      expect(screen.getByRole("button", { name: "새 단어 찾는 중…" })).toBeDisabled();
      expect(screen.getByText(/이 화면을 나갔다 와도 계속 진행돼요/)).toBeInTheDocument();

      vi.mocked(fetchAutoAddStatus).mockResolvedValue({ status: "done", count: 1 });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3000);
      });

      expect(screen.getByText("확정 전 단어")).toBeInTheDocument();
      expect(screen.getByText("resilient")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "새 단어 추가로 학습하기" })).toBeInTheDocument();
    });

    it("shows an error hint immediately when starting the job fails", async () => {
      vi.mocked(fetchWords).mockResolvedValue({ words: [], dueCount: 0 });
      vi.mocked(startAutoAddWords).mockResolvedValue(null);
      const user = userEvent.setup();
      render(<WordReview />);

      await user.click(await screen.findByRole("button", { name: "새 단어 추가로 학습하기" }));

      expect(await screen.findByText("단어를 추가하지 못했어요. 잠시 후 다시 시도해주세요.")).toBeInTheDocument();
    });

    // Shared by the two tests below: starts the background job, then drives
    // pollAutoAdd's next tick to observe fetchAutoAddStatus as `finalStatus`
    // — same fake-timer pattern as the "starts the background job" test
    // above, just without that test's extra mid-flight assertions.
    async function startAutoAddAndAdvance(finalStatus: "done" | "failed") {
      vi.mocked(fetchWords).mockResolvedValue({ words: [], dueCount: 0 });
      vi.mocked(startAutoAddWords).mockResolvedValue({ status: "pending", count: 0 });
      const user = userEvent.setup({ delay: null });
      render(<WordReview />);
      const button = await screen.findByRole("button", { name: "새 단어 추가로 학습하기" });

      vi.useFakeTimers({ shouldAdvanceTime: true });
      await user.click(button);

      vi.mocked(fetchAutoAddStatus).mockResolvedValue({ status: finalStatus, count: 0 });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3000);
      });
    }

    it("shows a not-found hint once the job completes with no suggestions", async () => {
      await startAutoAddAndAdvance("done");

      expect(screen.getByText("추천할 새 단어를 찾지 못했어요. 잠시 후 다시 시도해주세요.")).toBeInTheDocument();
    });

    it("shows an error hint once the job fails in the background", async () => {
      await startAutoAddAndAdvance("failed");

      expect(screen.getByText("단어를 추가하지 못했어요. 잠시 후 다시 시도해주세요.")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "새 단어 추가로 학습하기" })).toBeInTheDocument();
    });

    // The actual point of making this a background job: a learner who
    // pressed the button, navigated away mid-generation, and comes back
    // (a fresh mount, in test terms) must see it's still going instead of
    // the button looking untouched — see the mount effect in WordReview.tsx.
    it("resumes watching a job that was already in progress when the page loads", async () => {
      vi.mocked(fetchWords).mockResolvedValue({ words: [], dueCount: 0 });
      vi.mocked(fetchAutoAddStatus).mockResolvedValue({ status: "pending", count: 0 });
      render(<WordReview />);

      expect(await screen.findByRole("button", { name: "새 단어 찾는 중…" })).toBeDisabled();
      expect(startAutoAddWords).not.toHaveBeenCalled();
    });
  });

  it("deletes a word after confirmation", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [futureWord], dueCount: 0 });
    vi.mocked(deleteWord).mockResolvedValue(true);
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const user = userEvent.setup();
    render(<WordReview />);

    await user.click(await screen.findByRole("button", { name: "단어 삭제" }));

    expect(deleteWord).toHaveBeenCalledWith("w2");
    expect(await screen.findByText(/아직 학습 중인 단어가 없어요/)).toBeInTheDocument();
  });

  it("does not delete when the confirmation is declined", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [futureWord], dueCount: 0 });
    vi.spyOn(window, "confirm").mockReturnValue(false);
    const user = userEvent.setup();
    render(<WordReview />);

    await user.click(await screen.findByRole("button", { name: "단어 삭제" }));

    expect(deleteWord).not.toHaveBeenCalled();
    expect(screen.getByText("elated")).toBeInTheDocument();
  });

  it("runs a review quiz: masks the word in the example, checks the answer, and shows the score", async () => {
    vi.mocked(reviewWord).mockResolvedValue({ ...dueWord, stage: 1, reviewCount: 1 });
    const user = await startQuiz();

    // The prompt shows the meaning and the example with the target word
    // masked out (replaced by an input to type directly into), not given
    // away.
    expect(await screen.findByText("매우 행복한")).toBeInTheDocument();
    expect(screen.getByText("마지막 복습: 8일 전 복습함")).toBeInTheDocument();
    expect(screen.getByText("She was")).toBeInTheDocument();
    expect(screen.getByText(".")).toBeInTheDocument();

    await user.type(screen.getByRole("textbox", { name: "정답 입력" }), "ecstatic");
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
    // The review call is deferred until the learner advances -- not sent
    // the instant the answer is checked -- so "억지로 맞췄어요" has a chance
    // to redirect it to a repeat instead (see the dedicated test below).
    expect(reviewWord).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "결과 보기" }));
    expect(reviewWord).toHaveBeenCalledWith("w1", true, false, 2);
    expect(await screen.findByText("1개 중 1개 맞혔어요!")).toBeInTheDocument();
  });

  it("focuses the first blank as soon as a recall question appears", async () => {
    await startQuiz();

    const input = await screen.findByRole("textbox", { name: "정답 입력" });
    expect(input).toHaveFocus();
  });

  it("supports submitting, advancing, and completing a recall quiz by keyboard", async () => {
    vi.mocked(reviewWord).mockResolvedValue({ ...dueWord, stage: 1 });
    const user = await startQuiz();
    expect(screen.getByRole("button", { name: "답안 확인" })).toBeDisabled();

    await user.keyboard("ecstatic{Enter}");

    expect(screen.getByText("정답이에요!")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "결과 보기" })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("heading", { name: "복습 결과" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "완료" })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("heading", { name: "배운 표현을 내 것으로 만들어요" })).toBeInTheDocument();
    expect(reviewWord).toHaveBeenCalledExactlyOnceWith("w1", true, false, 2);
  });

  it("does not submit an answer while the keyboard is composing text", async () => {
    const user = await startQuiz();
    const input = screen.getByRole("textbox", { name: "정답 입력" });
    await user.type(input, "ecstatic");
    fireEvent.keyDown(input, { key: "Enter", isComposing: true });

    expect(screen.queryByText("정답이에요!")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "답안 확인" })).toBeEnabled();
    expect(reviewWord).not.toHaveBeenCalled();
  });

  it("locks the answer and actions while checking a similar answer", async () => {
    let resolveCheck!: (similar: boolean) => void;
    vi.mocked(checkQuizAnswer).mockReturnValue(new Promise((resolve) => { resolveCheck = resolve; }));
    const user = await startQuiz();
    const input = screen.getByRole("textbox", { name: "정답 입력" });
    await user.type(input, "thrilled{Enter}");

    expect(input).toBeDisabled();
    expect(screen.getByRole("button", { name: "채점 중…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "잘 모르겠어요" })).toBeDisabled();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(checkQuizAnswer).toHaveBeenCalledTimes(1);
    expect(vi.mocked(checkQuizAnswer).mock.calls[0][4]).toEqual({ wordId: expect.any(String) });
    expect(reviewWord).not.toHaveBeenCalled();

    await act(async () => { resolveCheck(true); });
    expect(input).toBeEnabled();
    expect(input).toHaveValue("");
    expect(input).toHaveFocus();
    expect(screen.getByRole("status")).toHaveTextContent("유사한 정답이에요!");
  });

  it("does not show the forced-guess button when a correct answer schedules the word for tomorrow", async () => {
    const user = await startQuiz();
    await user.type(await screen.findByRole("textbox", { name: "정답 입력" }), "ecstatic");
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "😅 억지로 맞춘 것 같아요" })).not.toBeInTheDocument();
  });

  // The "억지로 맞췄어요" (forced-guess) button: even after answering
  // correctly, the learner can flag it as a lucky/forced guess so the word
  // comes back at the same interval instead of the wider one a confident
  // correct answer would earn (see wordreview.nextSchedule's repeat doc
  // server-side).
  it("sends repeat=true and still advances when the forced-guess button is pressed on a correct answer", async () => {
    const establishedWord = { ...dueWord, stage: 1, reviewCount: 1 };
    vi.mocked(reviewWord).mockResolvedValue(establishedWord);
    const user = await startQuiz([establishedWord]);
    await user.type(await screen.findByRole("textbox", { name: "정답 입력" }), "ecstatic");
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
    const forcedBtn = screen.getByRole("button", { name: "😅 억지로 맞춘 것 같아요" });
    await user.click(forcedBtn);

    expect(reviewWord).toHaveBeenCalledWith("w1", true, true, 2);
    expect(await screen.findByText("1개 중 1개 맞혔어요!")).toBeInTheDocument();
  });

  // The forced-guess button only makes sense for a correct answer -- an
  // incorrect one already resets to stage 0 unconditionally (see
  // nextSchedule), so there's nothing for "repeat" to soften.
  it("does not show the forced-guess button after an incorrect answer", async () => {
    vi.mocked(reviewWord).mockResolvedValue({ ...dueWord, stage: 0, reviewCount: 1 });
    const user = await startQuiz();
    await user.type(await screen.findByRole("textbox", { name: "정답 입력" }), "wrong answer");
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText(/아쉬워요\. 정답: ecstatic/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "😅 억지로 맞춘 것 같아요" })).not.toBeInTheDocument();
  });

  it("grows the answer input to fit what's typed, not the length of the hidden answer", async () => {
    const user = await startQuiz();

    const input = screen.getByRole("textbox", { name: "정답 입력" });
    const widthBefore = input.style.width;

    await user.type(input, "ecstatic");
    const widthAfter = input.style.width;

    expect(widthAfter).not.toBe(widthBefore);
    expect(parseInt(widthAfter, 10)).toBeGreaterThan(parseInt(widthBefore, 10));
  });

  it("leaves a context-selected possessive visible and tests only the idiom's lexical words", async () => {
    vi.mocked(reviewWord).mockResolvedValue({ ...idiomWord, stage: 1, reviewCount: 1 });
    const user = await startQuiz([idiomWord]);

    // "his" is selected by the sentence subject, not recoverable from the
    // Korean meaning alone, so it stays visible between the two real targets.
    expect(await screen.findByText("최선을 다하다")).toBeInTheDocument();
    expect(screen.getByText("Even though the task was complicated, he promised to")).toBeInTheDocument();
    expect(screen.getByText("his")).toBeInTheDocument();
    expect(screen.getByText("for the sake of the team.")).toBeInTheDocument();

    const firstBlank = screen.getByRole("textbox", { name: "정답 1 입력" });
    const secondBlank = screen.getByRole("textbox", { name: "정답 2 입력" });
    await user.type(firstBlank, "do");
    await user.type(secondBlank, "best");
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
  });

  // The saved example inflects "optimize" to "optimizing" (drops the
  // trailing "e" before "-ing"), which isn't a literal substring of the
  // dictionary word, so this exercises the stem-matching fallback rather
  // than the exact-match path. The blank sits right after "are", so only
  // the inflected spelling actually fits grammatically -- the dictionary
  // base form must NOT be accepted.
  it("masks the word even when the example spells it as an inflected form (optimize -> optimizing)", async () => {
    vi.mocked(reviewWord).mockResolvedValue({ ...optimizeWord, stage: 1, reviewCount: 1 });
    const user = await startQuiz([optimizeWord]);

    expect(await screen.findByText("최적화하다")).toBeInTheDocument();
    expect(screen.getByText("We are")).toBeInTheDocument();
    expect(screen.getByText("the search algorithm for faster results.")).toBeInTheDocument();
    expect(screen.queryByText(/optimizing/i)).not.toBeInTheDocument();

    await user.type(screen.getByRole("textbox", { name: "정답 입력" }), "optimizing");
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
  });

  it("hides the whole inflected form and requires it instead of leaving its suffix visible", async () => {
    const user = await startQuiz([organizeWord]);

    expect(await screen.findByText("조직하다, 체계화하다")).toBeInTheDocument();
    expect(screen.getByText("The criminals planned the attack in a highly")).toBeInTheDocument();
    expect(screen.getByText("manner.")).toBeInTheDocument();
    expect(screen.queryByText(/^d$/)).not.toBeInTheDocument();

    await user.type(screen.getByRole("textbox", { name: "정답 입력" }), "organize");
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText("아쉬워요. 정답: organized")).toBeInTheDocument();
  });

  it("asks learners to retry a similar answer without referring to tense", async () => {
    vi.mocked(checkQuizAnswer).mockResolvedValue(true);
    const user = await startQuiz();

    await user.type(await screen.findByRole("textbox", { name: "정답 입력" }), "thrilled");
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByRole("status")).toHaveTextContent("유사한 정답이에요! 다시 입력해보세요.");
    expect(screen.queryByText(/시제에 맞춰/)).not.toBeInTheDocument();
  });

  it("rejects the dictionary base form when the sentence grammar requires the inflected spelling", async () => {
    vi.mocked(reviewWord).mockResolvedValue({ ...optimizeWord, stage: 0, reviewCount: 1 });
    const user = await startQuiz([optimizeWord]);

    await user.type(await screen.findByRole("textbox", { name: "정답 입력" }), "optimize");
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText("아쉬워요. 정답: optimizing")).toBeInTheDocument();
  });

  it("marks an incorrect lexical slot in a placeholder idiom incorrect", async () => {
    vi.mocked(reviewWord).mockResolvedValue({ ...idiomWord, stage: 0, reviewCount: 1 });
    const user = await startQuiz([idiomWord]);

    const firstBlank = await screen.findByRole("textbox", { name: "정답 1 입력" });
    const secondBlank = screen.getByRole("textbox", { name: "정답 2 입력" });
    await user.type(firstBlank, "do");
    await user.type(secondBlank, "good");
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText(/아쉬워요\. 정답: do, best/)).toBeInTheDocument();
    expect(firstBlank).toHaveClass("correct");
    expect(secondBlank).toHaveClass("incorrect");
  });

  it("requeues a missed word for a same-session retry instead of ending the session on it", async () => {
    vi.mocked(reviewWord).mockResolvedValue({ ...dueWord, stage: 0, reviewCount: 1 });
    const user = await startQuiz();
    await user.type(await screen.findByRole("textbox", { name: "정답 입력" }), "wrong answer");
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText(/아쉬워요\. 정답: ecstatic/)).toBeInTheDocument();
    expect(reviewWord).toHaveBeenCalledWith("w1", false, false, 2);

    // Missing the only word in the queue doesn't end the session -- it's
    // requeued for a same-day retry, so there's another question to go.
    expect(screen.getByRole("button", { name: "다음 단어" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "다음 단어" }));

    await user.type(await screen.findByRole("textbox", { name: "정답 입력" }), "ecstatic");
    await user.click(screen.getByRole("button", { name: "답안 확인" }));
    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "결과 보기" }));
    expect(await screen.findByText("2개 중 1개 맞혔어요!")).toBeInTheDocument();
  });

  // With only one verified word tracked (dueWord alone, no others to pull
  // distractors from), a review session must always fall back to recall
  // mode — recognition mode needs minRecognitionDistractors other verified
  // meanings, which don't exist here. Forces Math.random toward
  // "recognition" (0, i.e. < 0.5) to prove the *count* guard, not the coin
  // flip, is what's preventing it.
  it("falls back to recall mode when there aren't enough other verified words for a multiple-choice question", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0);
    await startQuiz();

    expect(await screen.findByRole("textbox", { name: "정답 입력" })).toBeInTheDocument();
  });

  // Recognition mode: word + example given, learner picks the meaning from
  // multiple choice — the "읽기" half of the reading/writing pair the user
  // asked for, alongside the existing recall ("쓰기") mode above.
  it("runs a recognition-mode question: shows the word, offers multiple-choice meanings, and checks a correct pick", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0); // forces recognition mode when eligible
    vi.mocked(reviewWord).mockResolvedValue({ ...dueWord, stage: 1, reviewCount: 1 });
    const user = await startQuiz([dueWord, ...otherVerifiedWords]);

    // The word (not the meaning) and its plain, un-masked example are given
    // — nothing to hide, since recognizing the word is what's being tested.
    expect(await screen.findByText("ecstatic")).toBeInTheDocument();
    expect(screen.getByText("She was ecstatic.")).toBeInTheDocument();
    expect(document.querySelectorAll("button.quiz-choice-btn")).toHaveLength(8);
    expect(screen.getByRole("button", { name: "잘 모르겠어요" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "매우 행복한" }));
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
    expect(reviewWord).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "결과 보기" }));
    expect(reviewWord).toHaveBeenCalledWith("w1", true, false, 2);
  });

  it("lets learners revise a meaning before explicitly checking it", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0);
    const user = await startQuiz([dueWord, ...otherVerifiedWords]);
    const choices = within(screen.getByRole("group", { name: "단어의 뜻" }));
    const wrong = choices.getByRole("button", { name: "우울한" });
    const correct = choices.getByRole("button", { name: "매우 행복한" });
    const submit = screen.getByRole("button", { name: "답안 확인" });
    expect(submit).toBeDisabled();

    await user.click(wrong);
    expect(wrong).toHaveAttribute("aria-pressed", "true");
    expect(wrong).toHaveClass("selected");
    expect(submit).toBeEnabled();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(reviewWord).not.toHaveBeenCalled();

    await user.click(correct);
    expect(wrong).toHaveAttribute("aria-pressed", "false");
    expect(wrong).not.toHaveClass("selected");
    expect(correct).toHaveAttribute("aria-pressed", "true");
    expect(correct).not.toHaveClass("correct");
    expect(reviewWord).not.toHaveBeenCalled();
    await user.click(submit);

    expect(screen.getByRole("status")).toHaveTextContent("정답이에요!");
    expect(correct).toHaveClass("correct");
    for (const choice of choices.getAllByRole("button")) expect(choice).toBeDisabled();
    expect(screen.queryByRole("button", { name: "잘 모르겠어요" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "결과 보기" })).toHaveFocus();
  });

  it.each(["recall", "recognition"])("treats revealing an unsubmitted correct %s answer as a miss", async (mode) => {
    vi.spyOn(Math, "random").mockReturnValue(mode === "recall" ? 0.9 : 0);
    vi.mocked(reviewWord).mockResolvedValue({ ...dueWord, stage: 0 });
    const user = await startQuiz([dueWord, ...otherVerifiedWords]);
    if (mode === "recall") await user.type(screen.getByRole("textbox"), "ecstatic");
    else await user.click(screen.getByRole("button", { name: "매우 행복한" }));

    await user.click(screen.getByRole("button", { name: "잘 모르겠어요" }));

    expect(screen.getByRole("status")).toHaveTextContent("아쉬워요. 정답:");
    expect(reviewWord).toHaveBeenCalledExactlyOnceWith("w1", false, false, 2);
    await user.click(screen.getByRole("button", { name: "다음 단어" }));
    expect(reviewWord).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: "답안 확인" })).toBeDisabled();
    if (mode === "recall") expect(screen.getByRole("textbox")).toHaveValue("");
    else for (const button of within(screen.getByRole("group", { name: "단어의 뜻" })).getAllByRole("button")) {
      expect(button).toHaveAttribute("aria-pressed", "false");
    }
  });

  it("marks a recognition-mode question incorrect when the wrong meaning is picked", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0);
    vi.mocked(reviewWord).mockResolvedValue({ ...dueWord, stage: 0, reviewCount: 1 });
    const user = await startQuiz([dueWord, ...otherVerifiedWords]);
    await screen.findByText("ecstatic");
    await user.click(screen.getByRole("button", { name: "우울한" })); // a distractor, not the correct meaning
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText(/아쉬워요\. 정답: 매우 행복한/)).toBeInTheDocument();
    expect(reviewWord).toHaveBeenCalledWith("w1", false, false, 2);
  });

  it("reshuffles recognition choices when a missed word is retried", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0);
    vi.mocked(reviewWord).mockResolvedValue({ ...dueWord, stage: 0, reviewCount: 1 });
    const user = await startQuiz([dueWord, ...otherVerifiedWords]);

    const choiceButtons = () => [...document.querySelectorAll<HTMLButtonElement>(".quiz-choice-btn")];
    const initialChoices = choiceButtons().map((button) => button.textContent);
    await user.click(screen.getByRole("button", { name: "우울한" }));
    await user.click(screen.getByRole("button", { name: "답안 확인" }));
    await user.click(screen.getByRole("button", { name: "다음 단어" }));

    const retryChoices = choiceButtons().map((button) => button.textContent);
    expect(retryChoices).not.toEqual(initialChoices);
    expect(retryChoices.indexOf("매우 행복한")).not.toBe(initialChoices.indexOf("매우 행복한"));
  });

  it("resets the review day when the learner does not know a recognition answer", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0);
    vi.mocked(reviewWord)
      .mockResolvedValueOnce({ ...dueWord, stage: 0, reviewCount: 2 })
      .mockResolvedValueOnce({ ...dueWord, stage: 1, reviewCount: 3 });
    const user = await startQuiz([dueWord, ...otherVerifiedWords]);

    await user.click(screen.getByRole("button", { name: "잘 모르겠어요" }));
    expect(await screen.findByText(/아쉬워요\. 정답: 매우 행복한/)).toBeInTheDocument();
    expect(reviewWord).toHaveBeenCalledWith("w1", false, false, 2);

    // "모르겠어요" resets the word and puts it back at the end of the
    // session for an immediate day-1 retry.
    await user.click(screen.getByRole("button", { name: "다음 단어" }));
    await user.click(screen.getByRole("button", { name: "매우 행복한" }));
    await user.click(screen.getByRole("button", { name: "답안 확인" }));
    await user.click(screen.getByRole("button", { name: "결과 보기" }));
    expect(reviewWord).toHaveBeenLastCalledWith("w1", true, false, 2);
  });

  it("treats 모르겠어요 as an incorrect answer even when the retry is answered correctly", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0);
    vi.mocked(reviewWord)
      .mockResolvedValueOnce({ ...dueWord, stage: 0, reviewCount: 1 })
      .mockResolvedValueOnce({ ...dueWord, stage: 1, reviewCount: 2 });
    const user = await startQuiz([{ ...dueWord, stage: 1, reviewCount: 1 }, ...otherVerifiedWords]);

    await user.click(screen.getByRole("button", { name: "잘 모르겠어요" }));
    expect(await screen.findByText(/아쉬워요\. 정답: 매우 행복한/)).toBeInTheDocument();
    expect(reviewWord).toHaveBeenCalledWith("w1", false, false, 2);

    await user.click(screen.getByRole("button", { name: "다음 단어" }));
    await user.click(screen.getByRole("button", { name: "매우 행복한" }));
    await user.click(screen.getByRole("button", { name: "답안 확인" }));

    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "😅 억지로 맞춘 것 같아요" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "결과 보기" }));
    expect(reviewWord).toHaveBeenLastCalledWith("w1", true, false, 2);
  });

  it("returns to the list from the quiz view", async () => {
    const user = await startQuiz();
    await user.click(await screen.findByRole("button", { name: "← 목록으로" }));

    expect(await screen.findByRole("button", { name: "복습 시작" })).toBeInTheDocument();
  });
});
