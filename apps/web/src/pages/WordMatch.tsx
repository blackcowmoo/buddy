import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { fetchWords, type WordReviewItem } from "../lib/wordReview";
import { shuffled } from "../lib/shuffle";
import { SubPageHeader } from "../components/SubPageHeader";
import { LoadingHint } from "../components/LoadingHint";
import type { LoadState } from "../lib/loadState";

// Below this many verified words, a round would either repeat cards or be
// trivial (a 2-pair board is solved by elimination on the second flip) — so
// the learner is asked to save more words instead of being handed a
// no-op game.
const minPairs = 3;
// Above this many pairs the board stops fitting comfortably on a phone
// screen (see .word-match-grid's fixed column count) — extra verified words
// just aren't dealt into this round.
const maxPairs = 8;
// How long a mismatched pair stays face-up before flipping back, long enough
// to read both cards but short enough to keep the round snappy.
const mismatchDelayMs = 700;

interface Card {
  cardId: string; // unique per card (two cards share wordId, not cardId)
  wordId: string;
  kind: "word" | "meaning";
  label: string;
}

function dealCards(words: WordReviewItem[]): Card[] {
  const pairs = shuffled(words).slice(0, maxPairs);
  const cards: Card[] = pairs.flatMap((w) => [
    { cardId: `${w.id}-word`, wordId: w.id, kind: "word", label: w.word },
    { cardId: `${w.id}-meaning`, wordId: w.id, kind: "meaning", label: w.meaning },
  ]);
  return shuffled(cards);
}

export function WordMatch() {
  const [state, setState] = useState<LoadState>("loading");
  const [verifiedWords, setVerifiedWords] = useState<WordReviewItem[]>([]);
  const [cards, setCards] = useState<Card[]>([]);
  const [flipped, setFlipped] = useState<string[]>([]); // cardIds currently face-up, 0-2
  const [matched, setMatched] = useState<Set<string>>(new Set()); // wordIds solved
  const [moves, setMoves] = useState(0);
  const [elapsedSec, setElapsedSec] = useState(0);
  // Guards clicks while a mismatched pair is still on-screen, waiting for its
  // flip-back timeout — without this a fast third click could join a pair
  // that's about to be judged.
  const [checking, setChecking] = useState(false);

  const startedAtRef = useRef(0);

  const deal = useCallback((words: WordReviewItem[]) => {
    setCards(dealCards(words));
    setFlipped([]);
    setMatched(new Set());
    setMoves(0);
    setElapsedSec(0);
    startedAtRef.current = Date.now();
  }, []);

  useEffect(() => {
    fetchWords().then((result) => {
      if (result === null) {
        setState("error");
        return;
      }
      const verified = result.words.filter((w) => w.status === "verified");
      setVerifiedWords(verified);
      if (verified.length >= minPairs) deal(verified);
      setState("ready");
    });
  }, [deal]);

  const pairCount = useMemo(() => cards.length / 2, [cards]);
  const won = pairCount > 0 && matched.size === pairCount;

  // Ticks the elapsed-time readout once a second while a round is in
  // progress — purely cosmetic feedback (no score is ever saved), so it just
  // stops counting once the round is won instead of being cleared.
  useEffect(() => {
    if (cards.length === 0 || won) return;
    const id = setInterval(() => setElapsedSec(Math.floor((Date.now() - startedAtRef.current) / 1000)), 1000);
    return () => clearInterval(id);
  }, [cards, won]);

  const flipCard = useCallback(
    (card: Card) => {
      if (checking || matched.has(card.wordId) || flipped.includes(card.cardId)) return;
      if (flipped.length === 0) {
        setFlipped([card.cardId]);
        return;
      }
      if (flipped.length === 1) {
        const firstId = flipped[0];
        const first = cards.find((c) => c.cardId === firstId)!;
        setFlipped([firstId, card.cardId]);
        setMoves((m) => m + 1);
        if (first.wordId === card.wordId) {
          setMatched((prev) => new Set(prev).add(card.wordId));
          setFlipped([]);
          return;
        }
        setChecking(true);
        setTimeout(() => {
          setFlipped([]);
          setChecking(false);
        }, mismatchDelayMs);
      }
    },
    [checking, matched, flipped, cards],
  );

  const restart = useCallback(() => deal(verifiedWords), [deal, verifiedWords]);

  return (
    <div className="app">
      <SubPageHeader title="단어 매칭 게임" />

      <main className="convo word-match-page">
        {state === "loading" && <LoadingHint />}
        {state === "error" && <p className="hint">단어 목록을 불러오지 못했습니다. 네트워크 문제일 수 있습니다.</p>}

        {state === "ready" && verifiedWords.length < minPairs && (
          <p className="hint">
            복습 중인 단어가 {minPairs}개 이상 있어야 게임을 시작할 수 있어요. 채팅에서 🔎로 단어를 찾아 "학습하기"를
            눌러보세요.
          </p>
        )}

        {state === "ready" && verifiedWords.length >= minPairs && (
          <>
            <div className="word-match-stats" role="status">
              <span>시도 {moves}번</span>
              <span>{elapsedSec}초</span>
            </div>

            {won ? (
              <div className="word-match-won" role="status">
                <p>{pairCount}쌍을 모두 맞혔어요! {moves}번 만에, {elapsedSec}초 걸렸어요.</p>
                <button type="button" className="quiz-start-btn" onClick={restart}>
                  다시 하기
                </button>
              </div>
            ) : (
              <>
                <div className="word-match-grid">
                  {cards.map((card) => {
                    const isMatched = matched.has(card.wordId);
                    const isFaceUp = isMatched || flipped.includes(card.cardId);
                    return (
                      <button
                        key={card.cardId}
                        type="button"
                        className={
                          isMatched
                            ? "word-match-card word-match-card-matched"
                            : isFaceUp
                              ? "word-match-card word-match-card-flipped"
                              : "word-match-card"
                        }
                        onClick={() => flipCard(card)}
                        disabled={isMatched}
                        aria-label={isFaceUp ? card.label : "카드 뒤집기"}
                      >
                        {isFaceUp ? card.label : "?"}
                      </button>
                    );
                  })}
                </div>
                <button type="button" className="ghost quiz-back-btn" onClick={restart}>
                  다시 섞기
                </button>
              </>
            )}
          </>
        )}
      </main>
    </div>
  );
}
