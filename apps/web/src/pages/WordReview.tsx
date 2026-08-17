import { useCallback, useEffect, useRef, useState, type KeyboardEvent } from "react";
import { confirmThenDelete } from "../lib/confirmDelete";
import {
  deleteWord,
  fetchAutoAddStatus,
  fetchWords,
  reviewWord,
  startAutoAddWords,
  type WordReviewItem,
  startResearchWord,
  confirmResearchWord,
  saveWord,
} from "../lib/wordReview";
import type { WordSuggestion } from "../lib/protocol";
import { formatAbsoluteDateTime } from "../lib/time";
import { shuffled } from "../lib/shuffle";
import { checkQuizAnswer, normalizeQuizAnswer as normalizeAnswer, quizBlankInputClass, quizChoiceClass } from "../lib/quizCheck";
import { SubPageHeader } from "../components/SubPageHeader";
import { usePollScaffold } from "../hooks/usePollScaffold";
import { LoadingHint } from "../components/LoadingHint";
import type { LoadState } from "../lib/loadState";

// How often to re-check an auto-add job that's still generating in the
// background (see asyncjob.KindWordAutoAdd) — a poll, not a push, same
// reasoning as ArticleQuiz.tsx's articleStudyPollIntervalMs.
const wordAutoAddPollIntervalMs = 3000;

// Phrase words that carry no meaning of their own and are often swapped
// out by the LLM's example sentence (e.g. "one's" → "my"/"his"), so they
// shouldn't be required to literally match when masking.
const maskStopWords = new Set([
  "a", "an", "the", "to", "of", "in", "on", "at", "for", "and", "or",
  "one's", "someone's", "somebody's", "one", "oneself", "yourself",
  "himself", "herself", "themselves", "sb", "sb's", "sth",
]);

const escapeRegExp = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

// A token's dictionary spelling doesn't always survive inflection verbatim
// (optimize -> optimizing drops the "e"; study -> studied swaps "y" for
// "i"), so `\btoken\w*` alone misses those forms. Try the token as-is first,
// then progressively shorter stems that match how English spelling changes
// before "-ing"/"-ed"/"-ies", so e.g. "optimize" also matches via "optimiz".
function candidateStems(token: string): string[] {
  const stems = [token];
  if (/[a-zA-Z]e$/i.test(token)) stems.push(token.slice(0, -1)); // optimize -> optimiz
  if (/[^aeiou]y$/i.test(token)) stems.push(token.slice(0, -1)); // study -> stud
  return stems;
}

// Placeholder swapped in for each blanked word before splitting the sentence
// into typeable segments — a character that can never occur in the sentence
// text itself.
const BLANK = "\u0000";

// Hides the target word/phrase inside its own example sentence so the
// recall-mode review question doesn't just hand the learner the answer —
// same spirit as QuizPanel's LLM-generated fill-in-the-blank prompts,
// applied here to the plain example sentence saved alongside the word.
// Returns the sentence split around each blank (`parts.length ===
// answers.length + 1`; `parts[i]` sits before blank `i`, `parts[i + 1]`
// after it) so the caller can render a real input in place of each blank,
// with `answers[i]` being what the learner is expected to type into it.
function computeBlank(example: string, word: string): { parts: string[]; answers: string[] } {
  if (!word) return { parts: [example], answers: [] };
  const exact = new RegExp(escapeRegExp(word), "i");
  const exactMatch = example.match(exact);
  if (exactMatch && exactMatch.index !== undefined) {
    const masked = example.slice(0, exactMatch.index) + BLANK + example.slice(exactMatch.index + exactMatch[0].length);
    return { parts: masked.split(BLANK), answers: [word] };
  }

  // The saved example doesn't contain `word` verbatim — this happens when
  // the LLM inflects a phrase for the sentence's subject/tense (e.g. word
  // "do one's best" → example "do my best"). Fall back to masking each
  // significant word of the phrase on its own (tolerant of suffix changes
  // like run → running and spelling changes like optimize → optimizing, via
  // candidateStems), leaving words in between — "my" here — visible so
  // the learner can see where they fit, rather than one blank that either
  // hands over the whole answer or swallows unrelated sentence words. The
  // dictionary form's "one's" is never itself blanked (filtered as a stop
  // word below), so it never shows up in `answers` either.
  const tokens = word
    .split(/\s+/)
    .map((t) => t.replace(/[^a-zA-Z']/g, ""))
    .filter((t) => t.length > 1 && !maskStopWords.has(t.toLowerCase()));

  let masked = example;
  // Tokens are looked up (and blanked) in `word`'s own order, which doesn't
  // always match the order the words fall in the sentence — track where
  // each match actually landed so `answers` can be sorted back into
  // left-to-right sentence order, lining up with the blanks in `parts`.
  const matches: { index: number; matched: string }[] = [];
  for (const token of tokens) {
    let m: RegExpMatchArray | null = null;
    for (const stem of candidateStems(token)) {
      const regex = new RegExp(`\\b${escapeRegExp(stem)}\\w*`, "i");
      m = masked.match(regex);
      if (m) break;
    }
    if (!m || m.index === undefined) continue;
    // Grade against the inflected spelling that's actually in the sentence
    // (e.g. "optimizing"), not the dictionary form passed in as `word`
    // ("optimize") — the blank sits where the sentence's grammar demands
    // the inflected form, so that's the only spelling a learner can
    // correctly type there.
    matches.push({ index: m.index, matched: m[0] });
    masked = masked.slice(0, m.index) + BLANK + masked.slice(m.index + m[0].length);
  }
  if (matches.length === 0) return { parts: [example], answers: [] };
  matches.sort((a, b) => a.index - b.index);
  return { parts: masked.split(BLANK), answers: matches.map((m) => m.matched) };
}

function blanksMatch(expected: string[], given: string[]): boolean {
  return expected.length === given.length && expected.every((exp, i) => normalizeAnswer(given[i] ?? "") === normalizeAnswer(exp));
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
  // One entry per blank in the current recall question, typed directly into
  // the input rendered inline at that blank's position (see recallBlank
  // below) rather than one shared free-text box.
  const [answers, setAnswers] = useState<string[]>([]);
  const [selectedChoice, setSelectedChoice] = useState<string | null>(null);
  const [checked, setChecked] = useState(false);
  const [checkingSimilarity, setCheckingSimilarity] = useState(false);
  const [similarHint, setSimilarHint] = useState(false);
  const [correctCount, setCorrectCount] = useState(0);
  const blankRefs = useRef<(HTMLInputElement | null)[]>([]);
  const [autoAdding, setAutoAdding] = useState(false);
  const [autoAddError, setAutoAddError] = useState<string | null>(null);
  const [researching, setResearching] = useState<Set<string>>(new Set());
  const [researchResults, setResearchResults] = useState<Record<string, WordSuggestion[]>>({});

  // Poll scaffolding for an auto-add job still generating in the background
  // (see usePollScaffold's doc comment). Losing this component (navigating
  // away, or the tab closing) only stops *watching* —
  // asyncjob.KindWordAutoAdd keeps generating regardless (see
  // lib/wordReview.ts's startAutoAddWords doc comment); reopening this page
  // resumes watching via the mount effect below.
  const { tokenRef: pollTokenRef, schedulePoll } = usePollScaffold();

  // Polls the auto-add job's status until it leaves "pending" — started
  // either right after pressing "새 단어 추가로 학습하기" or, on mount, when
  // reopening this page finds one already in flight (see the mount effect
  // below).
  const pollAutoAdd = useCallback((token: object) => {
    const tick = async () => {
      if (pollTokenRef.current !== token) return; // a newer run took over
      const status = await fetchAutoAddStatus();
      if (pollTokenRef.current !== token) return;
      if (!status) {
        schedulePoll(tick, wordAutoAddPollIntervalMs); // transient fetch failure — keep trying
        return;
      }
      if (status.status === "pending") {
        schedulePoll(tick, wordAutoAddPollIntervalMs);
        return;
      }
      setAutoAdding(false);
      if (status.status === "failed") {
        setAutoAddError("단어를 추가하지 못했어요. 잠시 후 다시 시도해주세요.");
        return;
      }
      // "done" (or "" — treated the same as an already-finished idle state).
      if (status.count === 0) {
        setAutoAddError("추천할 새 단어를 찾지 못했어요. 잠시 후 다시 시도해주세요.");
      }
      fetchWords().then((result) => {
        if (result) {
          setWords(result.words);
          setDueCount(result.dueCount);
        }
      });
    };
    schedulePoll(tick, wordAutoAddPollIntervalMs);
  }, [schedulePoll]);

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
    // Resume watching an auto-add job that was already running when this
    // page loads — e.g. the learner pressed "새 단어 추가로 학습하기",
    // navigated away, and just came back.
    fetchAutoAddStatus().then((status) => {
      if (status?.status !== "pending") return;
      setAutoAdding(true);
      const token = {};
      pollTokenRef.current = token;
      pollAutoAdd(token);
    });
  }, [pollAutoAdd]);

  // Research jobs are persisted server-side. Polling the normal word list
  // makes completion visible again after leaving and reopening this page.
  useEffect(() => {
    const timer = window.setInterval(() => {
      if (!words.some((w) => w.researchStatus === "pending")) return;
      fetchWords().then((result) => { if (result) { setWords(result.words); setDueCount(result.dueCount); } });
    }, 2000);
    return () => window.clearInterval(timer);
  }, [words]);

  const handleDelete = (id: string) => confirmThenDelete("이 단어를 삭제할까요?", deleteWord, id, setWords);

  const handleResearch = useCallback(async (word: WordReviewItem) => {
    setResearching((prev) => new Set(prev).add(word.id));
    const updated = await startResearchWord(word.id);
    setResearching((prev) => { const next = new Set(prev); next.delete(word.id); return next; });
    if (updated) setWords((prev) => prev.map((w) => w.id === updated.id ? updated : w));
  }, []);

  const handleConfirmResearch = useCallback(async (word: WordReviewItem) => {
    const updated = await confirmResearchWord(word.id);
    if (updated) {
      setWords((prev) => {
        const withoutConfirmed = prev.filter((w) => w.id !== word.id);
        const existingIndex = withoutConfirmed.findIndex((w) => w.id === updated.id);
        if (existingIndex < 0) return [...withoutConfirmed, updated];
        const next = [...withoutConfirmed];
        next[existingIndex] = updated;
        return next;
      });
    }
  }, []);

  const handleChooseMeaning = useCallback(async (oldWord: WordReviewItem, suggestion: WordSuggestion) => {
    const saved = await saveWord(suggestion, oldWord.originalWord ?? oldWord.word);
    if (!saved) return;
    await deleteWord(oldWord.id);
    setWords((prev) => [...prev.filter((w) => w.id !== oldWord.id), saved]);
    setResearchResults((prev) => {
      const next = { ...prev };
      delete next[oldWord.id];
      return next;
    });
  }, []);

  const researchControls = (word: WordReviewItem) => {
    if (word.researchStatus === "confirmed") return null;
    return (
      <>
        <button type="button" className="ghost word-research-btn" onClick={() => void handleResearch(word)} disabled={researching.has(word.id) || word.researchStatus === "pending"}>
          {researching.has(word.id) || word.researchStatus === "pending" ? "다시 찾는 중…" : "다시 검색"}
        </button>
        {(word.researchResults ?? researchResults[word.id] ?? []).map((s, i) => (
          <button key={i} type="button" className="ghost word-research-choice" onClick={() => void handleChooseMeaning(word, s)}>
            {s.meaning} · {s.example}
          </button>
        ))}
        {word.researchStatus !== "pending" && <button type="button" className="ghost word-research-btn" onClick={() => void handleConfirmResearch(word)}>확정</button>}
      </>
    );
  };

  const answersForItem = (item: QuizItem | undefined): string[] =>
    item && item.mode === "recall" ? new Array(computeBlank(item.word.example, item.word.word).answers.length).fill("") : [];

  const startQuiz = useCallback(() => {
    const now = Date.now() / 1000;
    const verified = words.filter((w) => w.status === "verified" && w.researchStatus === "confirmed");
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
    setAnswers(answersForItem(queue[0]));
    setSelectedChoice(null);
    setChecked(false);
    setCheckingSimilarity(false);
    setSimilarHint(false);
    setCorrectCount(0);
  }, [words]);

  const backToList = useCallback(() => setQuizQueue(null), []);

  // Backs the "새 단어 추가로 학습하기" button that takes over "복습 시작"'s
  // slot once dueCount hits 0 — starts a background job that generates a
  // batch of new words fit to the learner's profile and saves them as
  // "확인 중" (pending) rows, exactly the same lifecycle a manually
  // search-and-saved word already has (see httpserver.wordAutoAddHandler
  // for the validation it goes through). Generation itself runs
  // server-side (see asyncjob.KindWordAutoAdd) — this only starts it and
  // hands off to pollAutoAdd, so navigating away and back finds it still
  // going (see the mount effect above).
  const handleAutoAdd = useCallback(async () => {
    setAutoAdding(true);
    setAutoAddError(null);
    const status = await startAutoAddWords();
    if (!status || status.status === "failed") {
      setAutoAdding(false);
      setAutoAddError("단어를 추가하지 못했어요. 잠시 후 다시 시도해주세요.");
      return;
    }
    const token = {};
    pollTokenRef.current = token;
    pollAutoAdd(token);
  }, [pollAutoAdd]);

  const currentItem = quizQueue?.[index] ?? null;
  const current = currentItem?.word ?? null;
  const recallBlank = currentItem?.mode === "recall" ? computeBlank(currentItem.word.example, currentItem.word.word) : null;

  // Put the learner straight into the first answer field whenever a recall
  // question appears. This is especially important on mobile, where focusing
  // the field is what brings up the keyboard without an extra tap.
  useEffect(() => {
    if (currentItem?.mode !== "recall" || checked || checkingSimilarity) return;
    blankRefs.current[0]?.focus();
  }, [currentItem, checked, checkingSimilarity]);

  const isCorrect =
    checked && currentItem
      ? currentItem.mode === "recall"
        ? blanksMatch(recallBlank!.answers, answers)
        : selectedChoice === currentItem.word.meaning
      : false;

  const finishCheck = useCallback(
    (item: QuizItem, correct: boolean) => {
      setChecked(true);
      if (correct) {
        // The review call for a correct answer is deferred until the
        // learner advances (see advanceCorrect) rather than fired here,
        // because "억지로 맞췄어요" needs to pick between two different
        // outcomes (advance vs. repeat) before anything is sent.
        setCorrectCount((c) => c + 1);
        return;
      }
      // A miss resets the word's schedule to be due again today instead of
      // tomorrow (see the backend's nextSchedule) -- requeue it here too,
      // at the back of this session's queue, so the learner actually gets
      // that same-day retry now rather than only next time they open
      // review.
      setQuizQueue((prev) => (prev ? [...prev, item] : prev));
      void reviewWord(item.word.id, false).then((updated) => {
        if (!updated) return;
        setWords((prev) => prev.map((w) => (w.id === updated.id ? updated : w)));
        setDueCount((c) => Math.max(0, c - 1));
      });
    },
    [],
  );

  const checkRecall = useCallback(async () => {
    if (!currentItem || checked || currentItem.mode !== "recall" || answers.some((a) => !a.trim())) return;
    const expected = computeBlank(currentItem.word.example, currentItem.word.word).answers;
    if (blanksMatch(expected, answers)) {
      finishCheck(currentItem, true);
      return;
    }
    setCheckingSimilarity(true);
    const similar = await checkQuizAnswer(recallBlank!.parts.join("___"), expected.join(" "), undefined, answers.join(" "));
    setCheckingSimilarity(false);
    if (similar) {
      setSimilarHint(true);
      setAnswers(answersForItem(currentItem));
      blankRefs.current[0]?.focus();
      return;
    }
    finishCheck(currentItem, false);
  }, [currentItem, checked, answers, finishCheck]);

  const chooseRecognition = useCallback(
    (choice: string) => {
      if (!currentItem || checked || currentItem.mode !== "recognition") return;
      setSelectedChoice(choice);
      finishCheck(currentItem, choice === currentItem.word.meaning);
    },
    [currentItem, checked, finishCheck],
  );

  const advanceToNext = useCallback(() => {
    setIndex(index + 1);
    setAnswers(answersForItem(quizQueue?.[index + 1]));
    setSelectedChoice(null);
    setChecked(false);
    setCheckingSimilarity(false);
    setSimilarHint(false);
  }, [index, quizQueue]);

  // Sends the deferred correct-answer review call, then moves on. repeat
  // marks the learner flagging a technically-correct-but-forced guess (the
  // "억지로 맞췄어요" button): the word gets rescheduled at the same interval
  // it just came from instead of advancing to the next, wider one (see
  // wordreview.nextSchedule server-side) -- so a word that took 30 days to
  // come up stays on a 30-day cadence for as long as repeat keeps getting
  // pressed, only moving to 60 once answered confidently.
  const advanceCorrect = useCallback(
    (repeat: boolean) => {
      if (currentItem) {
        void reviewWord(currentItem.word.id, true, repeat).then((updated) => {
          if (!updated) return;
          setWords((prev) => prev.map((w) => (w.id === updated.id ? updated : w)));
          setDueCount((c) => Math.max(0, c - 1));
        });
      }
      advanceToNext();
    },
    [currentItem, advanceToNext],
  );

  const next = useCallback(() => {
    if (isCorrect) {
      advanceCorrect(false);
      return;
    }
    advanceToNext();
  }, [isCorrect, advanceCorrect, advanceToNext]);

  const markForced = useCallback(() => advanceCorrect(true), [advanceCorrect]);

  // Enter in a blank moves to the next blank, submits from the last blank,
  // or (once checked) advances to the next question -- so the learner never
  // has to reach for the mouse mid-question.
  const handleBlankKeyDown = useCallback(
    (e: KeyboardEvent<HTMLInputElement>, i: number) => {
      if (e.key !== "Enter") return;
      e.preventDefault();
      if (checked) {
        next();
        return;
      }
      if (i < answers.length - 1) {
        blankRefs.current[i + 1]?.focus();
      } else {
        checkRecall();
      }
    },
    [checked, answers.length, next, checkRecall],
  );

  const verifiedWords = words.filter((w) => w.status === "verified" && w.researchStatus === "confirmed");
  // Keep every word that still needs learner confirmation together, including
  // verified rows created from a direct/article definition but not yet
  // confirmed. Rejected rows remain in their own exclusion section below.
  const unconfirmedWords = words.filter((w) =>
    w.status !== "rejected" && (w.status === "pending" || w.researchStatus !== "confirmed"),
  );
  const rejectedWords = words.filter((w) => w.status === "rejected");

  return (
    <div className="app">
      <SubPageHeader title="단어 복습" />

      <main className="convo word-review-page">
        {state === "loading" && <LoadingHint />}
        {state === "error" && <p className="hint">단어 목록을 불러오지 못했습니다. 네트워크 문제일 수 있습니다.</p>}

        {state === "ready" && quizQueue === null && (
          <>
            <p className="hint word-review-due-hint" role="status">
              {dueCount > 0 ? `복습할 단어 ${dueCount}개가 있어요.` : "지금 복습할 단어가 없어요."}
            </p>
            {words.length === 0 && (
              <p className="hint">
                아직 학습 중인 단어가 없어요. 채팅에서 🔎로 단어를 찾아 "학습하기"를 눌러보세요.
              </p>
            )}

            <WordListSection
              title="복습중인 단어"
              words={verifiedWords}
              onDelete={(id) => void handleDelete(id)}
              renderMeta={(w) => <><span className="word-list-next">다음 복습: {formatAbsoluteDateTime(w.nextReviewAt)}</span>{researchControls(w)}</>}
            />
            <WordListSection
              title="확정 전 단어"
              words={unconfirmedWords}
              onDelete={(id) => void handleDelete(id)}
              renderMeta={(w) => <><span className="word-list-next">확정 전</span>{researchControls(w)}</>}
            />
            <WordListSection
              title="제외된 단어"
              words={rejectedWords}
              rowClassName="word-list-row-rejected"
              onDelete={(id) => void handleDelete(id)}
              renderMeta={(w) => (
                <>
                  {w.verifyReason && <span className="word-list-reject-reason">{w.verifyReason}</span>}
                  {researchControls(w)}
                </>
              )}
            />
            {dueCount > 0 ? (
              <button type="button" className="quiz-start-btn" onClick={startQuiz}>
                복습 시작
              </button>
            ) : (
              <button type="button" className="quiz-start-btn" onClick={() => void handleAutoAdd()} disabled={autoAdding}>
                {autoAdding ? "새 단어 찾는 중…" : "새 단어 추가로 학습하기"}
              </button>
            )}
            {autoAdding && (
              <p className="hint">
                <span className="spinning">⏳</span> 새 단어를 찾는 중이에요. 이 화면을 나갔다 와도 계속 진행돼요.
              </p>
            )}
            {autoAddError && <p className="hint">{autoAddError}</p>}
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
                    {/* Blanks are typed directly in place inside the
                        sentence rather than gathered into one separate
                        textarea below -- each blank is its own input, sized
                        to what's been typed into it (not the hidden
                        answer's length, which would give it away). */}
                    <div className="word-search-example quiz-blank-sentence">
                      {recallBlank!.parts.map((part, i) => (
                        <span key={i}>
                          {part}
                          {i < recallBlank!.answers.length && (
                            <input
                              autoFocus={i === 0}
                              ref={(el) => {
                                blankRefs.current[i] = el;
                              }}
                              type="text"
                              className={quizBlankInputClass(
                                checked,
                                normalizeAnswer(answers[i] ?? "") === normalizeAnswer(recallBlank!.answers[i]),
                              )}
                              style={{ width: `${Math.min(16, Math.max(3, (answers[i]?.length ?? 0) + 1))}ch` }}
                              maxLength={40}
                              value={answers[i] ?? ""}
                              onChange={(e) =>
                                setAnswers((prev) => {
                                  const next = [...prev];
                                  next[i] = e.target.value;
                                  return next;
                                })
                              }
                              onKeyDown={(e) => handleBlankKeyDown(e, i)}
                              disabled={checked || checkingSimilarity}
                              aria-label={recallBlank!.answers.length > 1 ? `빈칸 ${i + 1} 정답 입력` : "정답 입력"}
                            />
                          )}
                        </span>
                      ))}
                    </div>
                    {!checked && (
                      <>
                        {similarHint && <p className="quiz-result similar" role="status">유사한 정답이에요! 시제에 맞춰 다시 입력해보세요.</p>}
                        <div className="quiz-next-actions">
                          <button type="button" className="quiz-check-btn" onClick={() => void checkRecall()} disabled={checkingSimilarity || answers.length === 0 || answers.some((a) => !a.trim())}>
                            {checkingSimilarity ? "확인 중…" : "확인"}
                          </button>
                          <button type="button" className="ghost quiz-forced-btn" onClick={() => finishCheck(currentItem, false)} disabled={checkingSimilarity}>
                            모르겠어요
                          </button>
                        </div>
                      </>
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
                        const cls = quizChoiceClass(checked, isSelected, isAnswer);
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
                        : `아쉬워요. 정답: ${currentItem.mode === "recall" ? recallBlank!.answers.join(" ") : current.meaning}`}
                    </div>
                    <div className="quiz-next-actions">
                      <button type="button" className="quiz-next-btn" onClick={next}>
                        {index + 1 < quizQueue.length ? "다음 단어" : "결과 보기"}
                      </button>
                      {isCorrect && current.stage > 0 && (
                        <button
                          type="button"
                          className="ghost quiz-forced-btn"
                          onClick={markForced}
                          title="확신 없이 찍어서 맞춘 경우, 같은 간격으로 다시 복습해요"
                        >
                          😅 억지로 맞춘 것 같아요
                        </button>
                      )}
                    </div>
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

function WordListSection({
  title,
  words,
  rowClassName,
  renderMeta,
  onDelete,
}: {
  title?: string;
  words: WordReviewItem[];
  rowClassName?: string;
  renderMeta: (w: WordReviewItem) => React.ReactNode;
  onDelete: (id: string) => void;
}) {
  if (words.length === 0) return null;
  return (
    <>
      {title && <h2 className="word-section-title">{title}</h2>}
      <ul className="word-list">
        {words.map((w) => (
          <li key={w.id} className={rowClassName ? `word-list-row ${rowClassName}` : "word-list-row"}>
            <div className="word-list-meta">
              <span className="word-search-word">{w.word}</span>
              <span className="word-search-meaning">{w.meaning}</span>
              {renderMeta(w)}
            </div>
            <button
              type="button"
              className="ghost icon-btn word-list-delete"
              onClick={() => onDelete(w.id)}
              aria-label="단어 삭제"
              title="단어 삭제"
            >
              🗑
            </button>
          </li>
        ))}
      </ul>
    </>
  );
}
