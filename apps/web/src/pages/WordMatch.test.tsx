/**
 * @vitest-environment jsdom
 */
import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../lib/wordReview", async () => {
  const actual = await vi.importActual<typeof import("../lib/wordReview")>("../lib/wordReview");
  return { ...actual, fetchWords: vi.fn() };
});

import { WordMatch } from "./WordMatch";
import { fetchWords, type WordReviewItem } from "../lib/wordReview";

function verifiedWord(id: string, word: string, meaning: string): WordReviewItem {
  return { id, word, meaning, example: `An example with ${word}.`, stage: 0, reviewCount: 0, nextReviewAt: 0, status: "verified" };
}

const w1 = verifiedWord("w1", "ecstatic", "매우 행복한");
const w2 = verifiedWord("w2", "gloomy", "우울한");
const w3 = verifiedWord("w3", "jaded", "지친");

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.clearAllMocks();
  vi.restoreAllMocks();
});

// Math.random always returning 0 makes the Fisher-Yates shuffle in
// lib/shuffle.ts fully deterministic: for [w1, w2, w3] dealt as
// word/meaning card pairs, it always produces (by hand-simulation of the
// swap sequence) this exact 6-card layout:
//   0: w2 meaning   1: w3 word   2: w3 meaning
//   3: w1 word      4: w1 meaning   5: w2 word
// so index 1<->2 and 3<->4 are matching pairs sitting next to each other,
// while w2's pair (0 and 5) is split apart — covering both an immediate
// match and a match found after a mismatch elsewhere.
beforeEach(() => {
  vi.spyOn(Math, "random").mockReturnValue(0);
});

function cardButtons() {
  return screen.getAllByRole("button").filter((b) => b.className.includes("word-match-card"));
}

describe("WordMatch page", () => {
  it("shows a welcoming introduction with the shared hamburger navigation", () => {
    vi.mocked(fetchWords).mockReturnValue(new Promise(() => {}));
    render(<WordMatch />);
    expect(screen.getByRole("button", { name: "메뉴" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "단어와 뜻의 짝을 찾아요" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "홈으로" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "대화로 돌아가기" })).not.toBeInTheDocument();
  });

  it("shows a loading hint before the fetch resolves", () => {
    vi.mocked(fetchWords).mockReturnValue(new Promise(() => {}));
    render(<WordMatch />);
    expect(screen.getByText("불러오는 중…")).toBeInTheDocument();
  });

  it("shows an error hint when the fetch fails", async () => {
    vi.mocked(fetchWords).mockResolvedValue(null);
    render(<WordMatch />);
    expect(await screen.findByText(/단어 목록을 불러오지 못했어요/)).toBeInTheDocument();
  });

  it("asks for more saved words instead of dealing a trivial board", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [w1, w2], dueCount: 0 });
    render(<WordMatch />);
    expect(await screen.findByText(/복습 중인 단어가 3개 이상 있어야/)).toBeInTheDocument();
    expect(screen.getByText(/지금은 2개가 준비되어 있어요/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "단어 모으러 가기" })).toHaveAttribute("href", "words");
    expect(screen.queryByRole("button", { name: "카드 뒤집기" })).not.toBeInTheDocument();
  });

  it("deals every word as a face-down word card and a face-down meaning card", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [w1, w2, w3], dueCount: 0 });
    render(<WordMatch />);
    const round = screen.getByRole("region", { name: "이번 게임" });
    const cards = await within(round).findAllByRole("button", { name: "카드 뒤집기" });
    expect(cards).toHaveLength(6);
    const controls = within(round).getByRole("group", { name: "게임 조작" });
    expect(within(controls).getByRole("button", { name: "다시 섞기" })).toBeEnabled();
    expect(controls.compareDocumentPosition(cards[0]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(round).getByRole("status")).toHaveTextContent("시도 0번");
    expect(screen.getAllByRole("main")).toHaveLength(1);
    expect(screen.getByRole("main")).toContainElement(round);
    expect(screen.getAllByRole("banner")).toHaveLength(1);
    for (const { word, meaning } of [w1, w2, w3]) {
      for (const label of [word, meaning]) {
        expect(screen.getByText(label)).not.toBeVisible();
        expect(screen.getByText(label)).toHaveAttribute("aria-hidden", "true");
        expect(screen.queryByRole("button", { name: label })).not.toBeInTheDocument();
      }
    }
  });

  it.each([[1, 2], [2, 1]])("matches a pair when cards %i then %i are flipped", async (first, second) => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [w1, w2, w3], dueCount: 0 });
    render(<WordMatch />);
    await screen.findAllByRole("button", { name: "카드 뒤집기" });
    const cards = cardButtons();

    // Index 1 and 2 are jaded's word/meaning pair (see the layout note above).
    fireEvent.click(cards[first]);
    fireEvent.click(cards[second]);

    expect(screen.getByText("jaded")).toBeVisible();
    expect(screen.getByText("지친")).toBeVisible();
    expect(cards[1].className).toContain("word-match-card-matched");
    expect(cards[2].className).toContain("word-match-card-matched");
    expect(screen.getByText("시도 1번")).toBeInTheDocument();
  });

  it("flips a mismatched pair back face-down after a short delay", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [w1, w2, w3], dueCount: 0 });
    render(<WordMatch />);
    // Load the initial data on real timers first — only the mismatch-flip
    // delay itself needs to be faked, not whatever internal polling
    // testing-library's findBy* does while waiting on the fetchWords promise.
    await screen.findAllByRole("button", { name: "카드 뒤집기" });
    const cards = cardButtons();

    vi.useFakeTimers();
    try {
      // Index 0 (gloomy's meaning) and index 1 (jaded's word) don't match.
      fireEvent.click(cards[0]);
      fireEvent.click(cards[1]);
      expect(screen.getByText("우울한")).toBeVisible();
      expect(screen.getByText("jaded")).toBeVisible();

      await act(() => vi.advanceTimersByTimeAsync(700));
      expect(screen.getByText("우울한")).not.toBeVisible();
      expect(screen.getByText("jaded")).not.toBeVisible();
      expect(screen.getAllByRole("button", { name: "카드 뒤집기" })).toHaveLength(6);
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps every matched word and meaning on the board with the result below it", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [w1, w2, w3], dueCount: 0 });
    render(<WordMatch />);
    await screen.findAllByRole("button", { name: "카드 뒤집기" });
    const cards = cardButtons();

    // jaded (1,2) and ecstatic (3,4) match on the first try each.
    fireEvent.click(cards[1]);
    fireEvent.click(cards[2]);
    fireEvent.click(cards[3]);
    fireEvent.click(cards[4]);
    // gloomy is split across 0 and 5.
    fireEvent.click(cards[0]);
    fireEvent.click(cards[5]);

    const result = screen.getByText(/3쌍을 모두 맞혔어요! 3번 만에, \d+초 걸렸어요\./);
    const completedCards = cardButtons();
    expect(completedCards).toHaveLength(cards.length);
    completedCards.forEach((card, index) => {
      expect(card).toBe(cards[index]);
      expect(card).toBeVisible();
      expect(card).toBeDisabled();
      expect(card.compareDocumentPosition(result) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    });
    for (const { word, meaning } of [w1, w2, w3]) {
      expect(screen.getByRole("button", { name: word })).toHaveTextContent(word);
      expect(screen.getByRole("button", { name: meaning })).toHaveTextContent(meaning);
    }
    expect(screen.queryByRole("button", { name: "카드 뒤집기" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "다시 섞기" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "다시 하기" })).toBeInTheDocument();
  });

  it("blocks a third flip during a mismatch and cancels the old delay when reshuffling", async () => {
    vi.useFakeTimers();
    vi.mocked(fetchWords).mockResolvedValue({ words: [w1, w2, w3], dueCount: 0 });
    await act(async () => { render(<WordMatch />); });
    const cards = cardButtons();
    fireEvent.click(cards[0]);
    fireEvent.click(cards[1]);
    fireEvent.click(cards[3]);
    expect(cards[3]).toHaveAccessibleName("카드 뒤집기");
    expect(screen.getByText("시도 1번")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "다시 섞기" }));
    const restartedCards = cardButtons();
    fireEvent.click(restartedCards[1]);
    expect(restartedCards[1]).toHaveAccessibleName("jaded");
    await act(() => vi.advanceTimersByTimeAsync(700));
    expect(restartedCards[1]).toHaveAccessibleName("jaded");
    expect(screen.getByText("시도 0번")).toBeInTheDocument();

    fireEvent.click(restartedCards[2]);
    expect(restartedCards[1]).toBeDisabled();
    expect(restartedCards[2]).toBeDisabled();
    expect(screen.getByText("시도 1번")).toBeInTheDocument();
  });

  it("keeps long label elements in place while revealing and concealing their content", async () => {
    const longWord = "pneumonoultramicroscopicsilicovolcanoconiosis".repeat(3);
    const longMeaning = "공백없이아주긴설명".repeat(20);
    const longPair = { ...w3, word: longWord, meaning: longMeaning };
    vi.mocked(fetchWords).mockResolvedValue({ words: [w1, w2, longPair], dueCount: 0 });
    render(<WordMatch />);
    const cards = await screen.findAllByRole("button", { name: "카드 뒤집기" });
    const label = screen.getByText(longWord);
    expect(label).not.toBeVisible();
    expect(screen.getByText(longMeaning)).not.toBeVisible();

    fireEvent.click(cards[1]);
    expect(screen.getByText(longWord)).toBe(label);
    expect(label).toBeVisible();
    expect(label).toHaveAttribute("aria-hidden", "false");
    expect(cards[1]).toHaveAccessibleName(longWord);

    fireEvent.click(screen.getByRole("button", { name: "다시 섞기" }));
    expect(screen.getByText(longWord)).toBe(label);
    expect(label).not.toBeVisible();
    expect(label).toHaveAttribute("aria-hidden", "true");
    expect(screen.queryByRole("button", { name: longWord })).not.toBeInTheDocument();
  });

  it("cleans up both the round clock and a pending mismatch when leaving the page", async () => {
    vi.useFakeTimers();
    vi.mocked(fetchWords).mockResolvedValue({ words: [w1, w2, w3], dueCount: 0 });
    const view = await act(async () => render(<WordMatch />));
    const cards = cardButtons();
    fireEvent.click(cards[0]);
    fireEvent.click(cards[1]);
    expect(vi.getTimerCount()).toBe(2);
    view.unmount();
    expect(vi.getTimerCount()).toBe(0);
  });

  it("clears the completed board and result when starting another round", async () => {
    vi.mocked(fetchWords).mockResolvedValue({ words: [w1, w2, w3], dueCount: 0 });
    render(<WordMatch />);
    const cards = await screen.findAllByRole("button", { name: "카드 뒤집기" });

    for (const index of [1, 2, 3, 4, 0, 5]) fireEvent.click(cards[index]);
    fireEvent.click(screen.getByRole("button", { name: "다시 하기" }));

    expect(screen.queryByText(/모두 맞혔어요/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "다시 하기" })).not.toBeInTheDocument();
    const restartedCards = screen.getAllByRole("button", { name: "카드 뒤집기" });
    expect(restartedCards).toHaveLength(6);
    restartedCards.forEach((card) => expect(card).toBeEnabled());
    expect(screen.getByText("시도 0번")).toBeInTheDocument();
    expect(screen.getByText("0초")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "다시 섞기" })).toBeInTheDocument();

    fireEvent.click(restartedCards[1]);
    fireEvent.click(restartedCards[2]);
    expect(screen.getByRole("button", { name: "jaded" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "지친" })).toBeDisabled();
    expect(screen.getByText("시도 1번")).toBeInTheDocument();
  });
});
