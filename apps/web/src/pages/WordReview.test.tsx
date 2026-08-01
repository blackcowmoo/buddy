/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("../lib/wordReview", async () => {
  const actual = await vi.importActual<typeof import("../lib/wordReview")>("../lib/wordReview");
  return { ...actual, fetchWords: vi.fn(), deleteWord: vi.fn(), reviewWord: vi.fn() };
});

import { WordReview } from "./WordReview";
import { deleteWord, fetchWords, reviewWord, type WordReviewItem } from "../lib/wordReview";
import { formatAbsoluteDateTime } from "../lib/time";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.restoreAllMocks();
});

const dueWord: WordReviewItem = {
  id: "w1",
  word: "ecstatic",
  meaning: "매우 행복한",
  example: "She was ecstatic.",
  stage: 0,
  reviewCount: 0,
  nextReviewAt: Math.floor(Date.now() / 1000) - 3600, // 1 hour ago: due
  status: "verified",
};

const idiomWord: WordReviewItem = {
  id: "w-idiom",
  word: "do one's best",
  meaning: "최선을 다하다",
  example: "I will do my best to finish the project on time.",
  stage: 0,
  reviewCount: 0,
  nextReviewAt: Math.floor(Date.now() / 1000) - 3600, // 1 hour ago: due
  status: "verified",
};

const futureWord: WordReviewItem = {
  id: "w2",
  word: "elated",
  meaning: "신이 난",
  example: "He was elated.",
  stage: 1,
  reviewCount: 1,
  nextReviewAt: Math.floor(Date.now() / 1000) + 48 * 3600, // in 2 days: not due
  status: "verified",
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

// Three other verified words distinct from dueWord, so a recognition-mode
// question about dueWord has enough distractor meanings (see
// minRecognitionDistractors in WordReview.tsx).
const otherVerifiedWords: WordReviewItem[] = [
  { id: "w3", word: "gloomy", meaning: "우울한", example: "a gloomy day", stage: 0, reviewCount: 0, nextReviewAt: Math.floor(Date.now() / 1000) + 999999, status: "verified" },
  { id: "w4", word: "jaded", meaning: "지친", example: "a jaded look", stage: 0, reviewCount: 0, nextReviewAt: Math.floor(Date.now() / 1000) + 999999, status: "verified" },
  { id: "w5", word: "content", meaning: "만족하는", example: "feeling content", stage: 0, reviewCount: 0, nextReviewAt: Math.floor(Date.now() / 1000) + 999999, status: "verified" },
];

describe("WordReview page", () => {
  it("links back to the chat page with a relative href", () => {
    vi.mocked(fetchWords).mockReturnValue(new Promise(() => {}));
    render(<WordReview />);
    expect(screen.getByRole("link", { name: "대화로 돌아가기" })).toHaveAttribute("href", ".");
  });

  it("shows a loading hint before the fetch resolves", () => {
    vi.mocked(fetchWords).mockReturnValue(new Promise(() => {}));
    render(<WordReview />);
    expect(screen.getByText("불러오는 중…")).toBeInTheDocument();
  });

  it("shows an error hint when the fetch fails", async () => {
    vi.mocked(fetchWords).mockResolvedValue(null);
    render(<WordReview />);
    expect(await screen.findByText(/단어 목록을 불러오지 못했습니다/)).toBeInTheDocument();
  });

  it("shows an empty-state hint when there are no tracked words", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [], dueCount: 0 });
    render(<WordReview />);
    expect(await screen.findByText(/아직 학습 중인 단어가 없어요/)).toBeInTheDocument();
    expect(screen.getByText("지금 복습할 단어가 없어요.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "복습 시작" })).not.toBeInTheDocument();
  });

  it("shows the due count and a start button when words are due", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [dueWord], dueCount: 1 });
    render(<WordReview />);
    expect(await screen.findByText("복습할 단어 1개가 있어요.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "복습 시작" })).toBeInTheDocument();
  });

  it("lists each tracked word with its meaning and next review time", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [futureWord], dueCount: 0 });
    render(<WordReview />);
    expect(await screen.findByText("elated")).toBeInTheDocument();
    expect(screen.getByText("신이 난")).toBeInTheDocument();
    const expectedNextReview = formatAbsoluteDateTime(futureWord.nextReviewAt);
    expect(screen.getByText((content) => content.includes(expectedNextReview))).toBeInTheDocument();
  });

  // A word never leaves review rotation, however many times it's been
  // answered correctly — it just gets a next-review date far out instead of
  // a fixed daily/weekly ceiling, so a growing word list doesn't pile more
  // and more reviews onto the same short cap (see wordreview's schedule
  // doc). The list view has no separate "완료" state to show instead.
  it("still shows a next-review date for a well-known word with a far-future schedule, not a terminal state", async () => {
    const wellKnownWord = { ...futureWord, stage: 9, nextReviewAt: Math.floor(Date.now() / 1000) + 200 * 86400 };
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

  it("shows a 확인 중 section for words still being verified", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [pendingWord], dueCount: 0 });
    render(<WordReview />);

    expect(await screen.findByText("확인 중")).toBeInTheDocument();
    expect(screen.getByText("wistful")).toBeInTheDocument();
    expect(screen.getByText("확인 중…")).toBeInTheDocument();
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
    vi.mocked(fetchWords).mockResolvedValue({ words: [dueWord], dueCount: 1 });
    vi.mocked(reviewWord).mockResolvedValue({ ...dueWord, stage: 1, reviewCount: 1 });
    const user = userEvent.setup();
    render(<WordReview />);

    await user.click(await screen.findByRole("button", { name: "복습 시작" }));

    // The prompt shows the meaning and the example with the target word
    // masked out, not given away.
    expect(await screen.findByText("매우 행복한")).toBeInTheDocument();
    expect(screen.getByText("She was ____.")).toBeInTheDocument();

    await user.type(screen.getByRole("textbox", { name: "정답 입력" }), "ecstatic");
    await user.click(screen.getByRole("button", { name: "확인" }));

    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
    expect(reviewWord).toHaveBeenCalledWith("w1", true);

    await user.click(screen.getByRole("button", { name: "결과 보기" }));
    expect(await screen.findByText("1개 중 1개 맞혔어요!")).toBeInTheDocument();
  });

  it("masks each significant word of a phrase when the example inflects it rather than showing it unmasked", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [idiomWord], dueCount: 1 });
    render(<WordReview />);

    await userEvent.setup().click(await screen.findByRole("button", { name: "복습 시작" }));

    // "do one's best" never appears verbatim in the example (it's
    // "do my best"), so the exact-match mask would leave it fully shown.
    expect(await screen.findByText("최선을 다하다")).toBeInTheDocument();
    expect(screen.getByText("I will ____ my ____ to finish the project on time.")).toBeInTheDocument();
  });

  it("counts a wrong answer as incorrect and still advances", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [dueWord], dueCount: 1 });
    vi.mocked(reviewWord).mockResolvedValue({ ...dueWord, stage: 0, reviewCount: 1 });
    const user = userEvent.setup();
    render(<WordReview />);

    await user.click(await screen.findByRole("button", { name: "복습 시작" }));
    await user.type(await screen.findByRole("textbox", { name: "정답 입력" }), "wrong answer");
    await user.click(screen.getByRole("button", { name: "확인" }));

    expect(await screen.findByText(/아쉬워요\. 정답: ecstatic/)).toBeInTheDocument();
    expect(reviewWord).toHaveBeenCalledWith("w1", false);

    await user.click(screen.getByRole("button", { name: "결과 보기" }));
    expect(await screen.findByText("1개 중 0개 맞혔어요!")).toBeInTheDocument();
  });

  // With only one verified word tracked (dueWord alone, no others to pull
  // distractors from), a review session must always fall back to recall
  // mode — recognition mode needs minRecognitionDistractors other verified
  // meanings, which don't exist here. Forces Math.random toward
  // "recognition" (0, i.e. < 0.5) to prove the *count* guard, not the coin
  // flip, is what's preventing it.
  it("falls back to recall mode when there aren't enough other verified words for a multiple-choice question", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0);
    vi.mocked(fetchWords).mockResolvedValue({ words: [dueWord], dueCount: 1 });
    const user = userEvent.setup();
    render(<WordReview />);

    await user.click(await screen.findByRole("button", { name: "복습 시작" }));

    expect(await screen.findByRole("textbox", { name: "정답 입력" })).toBeInTheDocument();
  });

  // Recognition mode: word + example given, learner picks the meaning from
  // multiple choice — the "읽기" half of the reading/writing pair the user
  // asked for, alongside the existing recall ("쓰기") mode above.
  it("runs a recognition-mode question: shows the word, offers multiple-choice meanings, and checks a correct pick", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0); // forces recognition mode when eligible
    vi.mocked(fetchWords).mockResolvedValue({ words: [dueWord, ...otherVerifiedWords], dueCount: 1 });
    vi.mocked(reviewWord).mockResolvedValue({ ...dueWord, stage: 1, reviewCount: 1 });
    const user = userEvent.setup();
    render(<WordReview />);

    await user.click(await screen.findByRole("button", { name: "복습 시작" }));

    // The word (not the meaning) and its plain, un-masked example are given
    // — nothing to hide, since recognizing the word is what's being tested.
    expect(await screen.findByText("ecstatic")).toBeInTheDocument();
    expect(screen.getByText("She was ecstatic.")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "매우 행복한" }));

    expect(await screen.findByText("정답이에요!")).toBeInTheDocument();
    expect(reviewWord).toHaveBeenCalledWith("w1", true);
  });

  it("marks a recognition-mode question incorrect when the wrong meaning is picked", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0);
    vi.mocked(fetchWords).mockResolvedValue({ words: [dueWord, ...otherVerifiedWords], dueCount: 1 });
    vi.mocked(reviewWord).mockResolvedValue({ ...dueWord, stage: 0, reviewCount: 1 });
    const user = userEvent.setup();
    render(<WordReview />);

    await user.click(await screen.findByRole("button", { name: "복습 시작" }));
    await screen.findByText("ecstatic");
    await user.click(screen.getByRole("button", { name: "우울한" })); // a distractor, not the correct meaning

    expect(await screen.findByText(/아쉬워요\. 정답: 매우 행복한/)).toBeInTheDocument();
    expect(reviewWord).toHaveBeenCalledWith("w1", false);
  });

  it("returns to the list from the quiz view", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [dueWord], dueCount: 1 });
    const user = userEvent.setup();
    render(<WordReview />);

    await user.click(await screen.findByRole("button", { name: "복습 시작" }));
    await user.click(await screen.findByRole("button", { name: "← 목록으로" }));

    expect(await screen.findByRole("button", { name: "복습 시작" })).toBeInTheDocument();
  });
});
