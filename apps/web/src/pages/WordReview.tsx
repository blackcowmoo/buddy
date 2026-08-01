import { useCallback, useEffect, useState } from "react";
import { confirmThenDelete } from "../lib/confirmDelete";
import { deleteWord, fetchWords, reviewWord, type WordReviewItem } from "../lib/wordReview";
import { formatAbsoluteDateTime } from "../lib/time";
import { shuffled } from "../lib/shuffle";

type LoadState = "loading" | "ready" | "error";

// Mirrors App.tsx's QuizPanel/normalizeQuizAnswer: a loose match (trim,
// lowercase, drop trailing punctuation) so "Ecstatic." and "ecstatic" both
// count as correct, without pulling in a full spellcheck/LLM-judge pass for
// what's meant to be a quick self-check.
function normalizeAnswer(s: string): string {
  return s.trim().toLowerCase().replace(/[.,!?;:'"]+$/g, "");
}

// Phrase words that carry no meaning of their own and are often swapped
// out by the LLM's example sentence (e.g. "one's" → "my"/"his"), so they
// shouldn't be required to literally match when masking.
const maskStopWords = new Set([
  "a", "an", "the", "to", "of", "in", "on", "at", "for", "and", "or",
  "one's", "someone's", "somebody's", "one", "oneself", "yourself",
  "himself", "herself", "themselves", "sb", "sb's", "sth",
]);

const escapeRegExp = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

// Hides the target word/phrase inside its own example sentence so the
// recall-mode review question doesn't just hand the learner the answer —
// same spirit as QuizPanel's LLM-generated fill-in-the-blank prompts,
// applied here to the plain example sentence saved alongside the word.
// `answer` is what the learner is expected to type back: usually `word`
// itself, but see the fallback branch below for the multi-blank case.
function computeBlank(example: string, word: string): { masked: string; answer: string } {
  if (!word) return { masked: example, answer: word };
  const exact = new RegExp(escapeRegExp(word), "gi");
  if (exact.test(example)) {
    return { masked: example.replace(exact, "____"), answer: word };
  }

  // The saved example doesn't contain `word` verbatim — this happens when
  // the LLM inflects a phrase for the sentence's subject/tense (e.g. word
  // "do one's best" → example "do my best"). Fall back to masking each
  // significant word of the phrase on its own (tolerant of suffix changes
  // like run → running), leaving words in between — "my" here — visible so
  // the learner can see where they fit, rather than one blank that either
  // hands over the whole answer or swallows unrelated sentence words. The
  // learner types the blanked words back as one space-separated answer, so
  // `answer` is built the same way rather than from the full dictionary
  // form (which would require typing "one's", never itself blanked).
  const tokens = word
    .split(/\s+/)
    .map((t) => t.replace(/[^a-zA-Z']/g, ""))
    .filter((t) => t.length > 1 && !maskStopWords.has(t.toLowerCase()));

  let masked = example;
  const matched: string[] = [];
  for (const token of tokens) {
    const regex = new RegExp(`\\b${escapeRegExp(token)}\\w*`, "i");
    if (!regex.test(masked)) continue;
    matched.push(token);
    masked = masked.replace(regex, "____");
  }
  if (matched.length === 0) return { masked: example, answer: word };
  return { masked, answer: matched.join(" ") };
}

// A review session mixes two question shapes so a learner practices both
// producing English (writing) and understanding it (reading), not just one:
// - "recall": meaning + masked example -> type the word.
// - "recognition": word + example -> pick the correct meaning from 4
//   choices, 3 of them pulled from the learner's own other verified words'
//   meanings (see startQuiz) — no LLM call, built entirely from data already
//   loaded, and no new asyncjob (this app assumes a slow local LLM — see
//   wordreview's package doc — so a review session must never wait on one).
type QuizMode = "recall" | "recognition";
interface QuizItem {
  word: WordReviewItem;
  mode: QuizMode;
  choices?: string[]; // recognition only: the 4 shuffled options
}

// minRecognitionChoices-1 other verified words' meanings are needed to fill
// out a 4-option multiple-choice question — below that, recognition mode
// would either repeat an option or show fewer than 4, so that word gets
// recall mode instead.
const minRecognitionDistractors = 3;

export function WordReview() {
  const [state, setState] = useState<LoadState>("loading");
  const [words, setWords] = useState<WordReviewItem[]>([]);
  const [dueCount, setDueCount] = useState(0);

  // null = list view; an array (possibly empty) = quiz in progress, built
  // once from the due words at the moment "복습 시작" was pressed so the
  // question order/mode stays stable even as answers update `words` below.
  const [quizQueue, setQuizQueue] = useState<QuizItem[] | null>(null);
  const [index, setIndex] = useState(0);
  const [answer, setAnswer] = useState("");
  const [selectedChoice, setSelectedChoice] = useState<string | null>(null);
  const [checked, setChecked] = useState(false);
  const [correctCount, setCorrectCount] = useState(0);

  useEffect(() => {
    fetchWords().then((result) => {
      if (result === null) {
        setState("error");
        return;
      }
      setWords(result.words);
      setDueCount(result.dueCount);
      setState("ready");
    });
  }, []);

  const handleDelete = (id: string) => confirmThenDelete("이 단어를 삭제할까요?", deleteWord, id, setWords);

  const startQuiz = useCallback(() => {
    const now = Date.now() / 1000;
    const verified = words.filter((w) => w.status === "verified");
    const due = verified.filter((w) => w.nextReviewAt <= now);
    const queue: QuizItem[] = shuffled(due).map((w) => {
      const otherMeanings = verified.filter((other) => other.id !== w.id).map((other) => other.meaning);
      const useRecognition = otherMeanings.length >= minRecognitionDistractors && Math.random() < 0.5;
      if (!useRecognition) {
        return { word: w, mode: "recall" };
      }
      const distractors = shuffled(otherMeanings).slice(0, minRecognitionDistractors);
      return { word: w, mode: "recognition", choices: shuffled([w.meaning, ...distractors]) };
    });
    setQuizQueue(queue);
    setIndex(0);
    setAnswer("");
    setSelectedChoice(null);
    setChecked(false);
    setCorrectCount(0);
  }, [words]);

  const backToList = useCallback(() => setQuizQueue(null), []);

  const currentItem = quizQueue?.[index] ?? null;
  const current = currentItem?.word ?? null;
  const recallBlank = currentItem?.mode === "recall" ? computeBlank(currentItem.word.example, currentItem.word.word) : null;
  const isCorrect =
    checked && currentItem
      ? currentItem.mode === "recall"
        ? normalizeAnswer(answer) === normalizeAnswer(recallBlank!.answer)
        : selectedChoice === currentItem.word.meaning
      : false;

  const finishCheck = useCallback(
    (item: QuizItem, correct: boolean) => {
      setChecked(true);
      if (correct) setCorrectCount((c) => c + 1);
      void reviewWord(item.word.id, correct).then((updated) => {
        if (!updated) return;
        setWords((prev) => prev.map((w) => (w.id === updated.id ? updated : w)));
        setDueCount((c) => Math.max(0, c - 1));
      });
    },
    [],
  );

  const checkRecall = useCallback(() => {
    if (!currentItem || checked || currentItem.mode !== "recall" || !answer.trim()) return;
    const expected = computeBlank(currentItem.word.example, currentItem.word.word).answer;
    finishCheck(currentItem, normalizeAnswer(answer) === normalizeAnswer(expected));
  }, [currentItem, checked, answer, finishCheck]);

  const chooseRecognition = useCallback(
    (choice: string) => {
      if (!currentItem || checked || currentItem.mode !== "recognition") return;
      setSelectedChoice(choice);
      finishCheck(currentItem, choice === currentItem.word.meaning);
    },
    [currentItem, checked, finishCheck],
  );

  const next = useCallback(() => {
    setIndex((i) => i + 1);
    setAnswer("");
    setSelectedChoice(null);
    setChecked(false);
  }, []);

  const verifiedWords = words.filter((w) => w.status === "verified");
  const pendingWords = words.filter((w) => w.status === "pending");
  const rejectedWords = words.filter((w) => w.status === "rejected");

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <h1>단어 복습</h1>
        </div>
        {/* Relative link (not "/"): resolves against the current page URL,
            same reasoning as Recordings.tsx's back link, so this still works
            under a ROOT_PATH prefix like "/pr/14/words". */}
        <a className="ghost icon-btn" href="." aria-label="대화로 돌아가기" title="대화로 돌아가기">
          ←
        </a>
      </header>

      <main className="convo word-review-page">
        {state === "loading" && <p className="hint">불러오는 중…</p>}
        {state === "error" && <p className="hint">단어 목록을 불러오지 못했습니다. 네트워크 문제일 수 있습니다.</p>}

        {state === "ready" && quizQueue === null && (
          <>
            <p className="hint word-review-due-hint" role="status">
              {dueCount > 0 ? `복습할 단어 ${dueCount}개가 있어요.` : "지금 복습할 단어가 없어요."}
            </p>
            {dueCount > 0 && (
              <button type="button" className="quiz-start-btn" onClick={startQuiz}>
                복습 시작
              </button>
            )}
            {words.length === 0 && (
              <p className="hint">
                아직 학습 중인 단어가 없어요. 채팅에서 🔎로 단어를 찾아 "학습하기"를 눌러보세요.
              </p>
            )}

            {verifiedWords.length > 0 && (
              <ul className="word-list">
                {verifiedWords.map((w) => (
                  <li key={w.id} className="word-list-row">
                    <div className="word-list-meta">
                      <span className="word-search-word">{w.word}</span>
                      <span className="word-search-meaning">{w.meaning}</span>
                      <span className="word-list-next">다음 복습: {formatAbsoluteDateTime(w.nextReviewAt)}</span>
                    </div>
                    <button
                      type="button"
                      className="ghost icon-btn word-list-delete"
                      onClick={() => void handleDelete(w.id)}
                      aria-label="단어 삭제"
                      title="단어 삭제"
                    >
                      🗑
                    </button>
                  </li>
                ))}
              </ul>
            )}

            {pendingWords.length > 0 && (
              <>
                <h2 className="word-section-title">확인 중</h2>
                <ul className="word-list">
                  {pendingWords.map((w) => (
                    <li key={w.id} className="word-list-row">
                      <div className="word-list-meta">
                        <span className="word-search-word">{w.word}</span>
                        <span className="word-search-meaning">{w.meaning}</span>
                        <span className="word-list-next">확인 중…</span>
                      </div>
                      <button
                        type="button"
                        className="ghost icon-btn word-list-delete"
                        onClick={() => void handleDelete(w.id)}
                        aria-label="단어 삭제"
                        title="단어 삭제"
                      >
                        🗑
                      </button>
                    </li>
                  ))}
                </ul>
              </>
            )}

            {rejectedWords.length > 0 && (
              <>
                <h2 className="word-section-title">제외된 단어</h2>
                <ul className="word-list">
                  {rejectedWords.map((w) => (
                    <li key={w.id} className="word-list-row word-list-row-rejected">
                      <div className="word-list-meta">
                        <span className="word-search-word">{w.word}</span>
                        <span className="word-search-meaning">{w.meaning}</span>
                        {w.verifyReason && <span className="word-list-reject-reason">{w.verifyReason}</span>}
                      </div>
                      <button
                        type="button"
                        className="ghost icon-btn word-list-delete"
                        onClick={() => void handleDelete(w.id)}
                        aria-label="단어 삭제"
                        title="단어 삭제"
                      >
                        🗑
                      </button>
                    </li>
                  ))}
                </ul>
              </>
            )}
          </>
        )}

        {state === "ready" && quizQueue !== null && (
          <div className="quiz-panel">
            <button type="button" className="ghost quiz-back-btn" onClick={backToList}>
              ← 목록으로
            </button>
            {current === null || currentItem === null ? (
              <div className="quiz-question">
                <div className="quiz-score" role="status">
                  {quizQueue.length === 0
                    ? "복습할 단어가 없어요."
                    : `${quizQueue.length}개 중 ${correctCount}개 맞혔어요!`}
                </div>
                <button type="button" className="quiz-next-btn" onClick={backToList}>
                  완료
                </button>
              </div>
            ) : (
              <div className="quiz-question">
                <div className="quiz-progress">
                  {index + 1} / {quizQueue.length}
                </div>
                {currentItem.mode === "recall" ? (
                  <>
                    <div className="quiz-prompt">{current.meaning}</div>
                    <div className="word-search-example">{recallBlank!.masked}</div>
                    <input
                      type="text"
                      className="quiz-answer-input"
                      // Width tracks what's actually been typed, not the
                      // expected answer -- a box pre-sized to fit the
                      // answer would give its length away before the
                      // learner types anything.
                      style={{ width: `${Math.min(40, Math.max(8, answer.length + 2))}ch` }}
                      value={answer}
                      onChange={(e) => setAnswer(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key !== "Enter") return;
                        if (checked) next();
                        else checkRecall();
                      }}
                      disabled={checked}
                      placeholder={
                        recallBlank!.answer.includes(" ")
                          ? "빈칸에 들어갈 단어들을 띄어쓰기로 구분해 입력하세요"
                          : "빈칸에 들어갈 단어를 입력하세요"
                      }
                      aria-label="정답 입력"
                    />
                    {!checked && (
                      <button type="button" className="quiz-check-btn" onClick={checkRecall} disabled={!answer.trim()}>
                        확인
                      </button>
                    )}
                  </>
                ) : (
                  <>
                    <div className="quiz-prompt">{current.word}</div>
                    <div className="word-search-example">{current.example}</div>
                    <div className="quiz-choices">
                      {currentItem.choices!.map((choice, i) => {
                        const isSelected = choice === selectedChoice;
                        const isAnswer = choice === current.meaning;
                        const cls = !checked
                          ? "quiz-choice-btn"
                          : isSelected
                            ? `quiz-choice-btn ${isAnswer ? "correct" : "incorrect"}`
                            : isAnswer
                              ? "quiz-choice-btn correct"
                              : "quiz-choice-btn";
                        return (
                          <button
                            key={i}
                            type="button"
                            className={cls}
                            onClick={() => chooseRecognition(choice)}
                            disabled={checked}
                          >
                            {choice}
                          </button>
                        );
                      })}
                    </div>
                  </>
                )}
                {checked && (
                  <>
                    <div className={`quiz-result ${isCorrect ? "correct" : "incorrect"}`} role="status">
                      {isCorrect
                        ? "정답이에요!"
                        : `아쉬워요. 정답: ${currentItem.mode === "recall" ? recallBlank!.answer : current.meaning}`}
                    </div>
                    <button type="button" className="quiz-next-btn" onClick={next}>
                      {index + 1 < quizQueue.length ? "다음 단어" : "결과 보기"}
                    </button>
                  </>
                )}
              </div>
            )}
          </div>
        )}
      </main>
    </div>
  );
}
