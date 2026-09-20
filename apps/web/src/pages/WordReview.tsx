import { useCallback, useEffect, useLayoutEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import { QuizChoices } from "../components/QuizChoices";
import { EmptyState, LearningIntro } from "../components/LearningIntro";
import { confirmThenDelete } from "../lib/confirmDelete";
import {
  deleteWord,
  fetchAutoAddStatus,
  fetchWords,
  reviewWord,
  startAutoAddWords,
  type WordReviewItem,
  type WordReviewQuestion,
  startResearchWord,
  confirmResearchWord,
  saveWord,
} from "../lib/wordReview";
import type { WordSuggestion } from "../lib/protocol";
import { formatAbsoluteDateTime } from "../lib/time";
import { shuffled } from "../lib/shuffle";
import { checkQuizAnswer, normalizeQuizAnswer as normalizeAnswer, quizBlankInputClass } from "../lib/quizCheck";
import { SubPageHeader } from "../components/SubPageHeader";
import { usePollScaffold } from "../hooks/usePollScaffold";
import { LoadingHint } from "../components/LoadingHint";
import type { LoadState } from "../lib/loadState";

// How often to re-check an auto-add job that's still generating in the
// background (see asyncjob.KindWordAutoAdd) — a poll, not a push, same
// reasoning as ArticleQuiz.tsx's articleStudyPollIntervalMs.
const wordAutoAddPollIntervalMs = 3000;
const reviewWordsPageSize = 20;
const currentReviewQuestionVersion = 1;

// Recall questions are generated and persisted server-side. Unlike the old
// client-side substring masking, the stored answer is the complete form the
// sentence requires ("organized", not dictionary-form "organize" plus a
// visible trailing "d"). The version check is a defensive rolling-deploy
// guard for responses served by an older backend replica.
function currentQuestion(word: WordReviewItem): WordReviewQuestion | null {
  const question = word.reviewQuestion;
  if (!question || question.version < currentReviewQuestionVersion) return null;
  if (question.prompt.split("___").length !== 2 || !question.answer.trim()) return null;
  return question;
}

// A review session mixes two question shapes so a learner practices both
// producing English (writing) and understanding it (reading), not just one:
// - "recall": meaning + a generated sentence blank -> type the complete
//   grammatical form required there.
// - "recognition": word + example -> pick the correct meaning from 8
//   choices, 7 of them pulled from the learner's own other verified words'
//   meanings (see startQuiz) — no LLM call, built entirely from data already
//   loaded, and no new asyncjob (this app assumes a slow local LLM — see
//   wordreview's package doc — so a review session must never wait on one).
type QuizMode = "recall" | "recognition";
interface QuizItem {
  word: WordReviewItem;
  mode: QuizMode;
  choices?: string[]; // recognition only: the 8 shuffled meaning options
}

// A missed recognition question is put back into the current session for an
// immediate retry. Give that retry a fresh choice order so remembering the
// previous button position cannot substitute for knowing the meaning. The
// fallback swap matters when a mocked or unlucky shuffle returns the exact
// same order; a retry should visibly move the choices whenever possible.
function reshuffleRecognitionChoices(item: QuizItem): QuizItem {
  if (item.mode !== "recognition" || !item.choices || item.choices.length < 2) return item;
  const choices = shuffled(item.choices);
  if (choices.every((choice, i) => choice === item.choices![i])) {
    [choices[0], choices[1]] = [choices[1], choices[0]];
  }
  return { ...item, choices };
}

function formatReviewAge(unixSeconds: number | undefined): string {
  if (!unixSeconds) return "아직 복습한 적 없음";
  const days = Math.max(0, Math.floor((Date.now() / 1000 - unixSeconds) / (24 * 60 * 60)));
  return days === 0 ? "오늘 복습함" : `${days}일 전 복습함`;
}

// minRecognitionDistractors other verified words' meanings are needed to fill
// out an 8-option multiple-choice question — below that, recognition mode
// would either repeat an option or show fewer than 8, so that word gets
// recall mode instead.
const minRecognitionDistractors = 7;

export function WordReview() {
  const [state, setState] = useState<LoadState>("loading");
  const [words, setWords] = useState<WordReviewItem[]>([]);
  const [dueCount, setDueCount] = useState(0);
  // Match the article history: recent reviews first, older entries below.
  // The page owns scrolling so nested lists do not trap touch gestures.
  const [reviewOlderCount, setReviewOlderCount] = useState(0);
  const reviewPageRef = useRef<HTMLElement>(null);

  // null = list view; an array (possibly empty) = quiz in progress, built
  // once from the due words at the moment "복습 시작" was pressed so the
  // question order/mode stays stable even as reviews update `words` below.
  const [quizQueue, setQuizQueue] = useState<QuizItem[] | null>(null);
  const [index, setIndex] = useState(0);
  // The current recall answer, typed directly into the generated sentence's
  // one blank rather than a separate free-text box.
  const [answer, setAnswer] = useState("");
  const [selectedChoice, setSelectedChoice] = useState<string | null>(null);
  const [checked, setChecked] = useState(false);
  const [checkingSimilarity, setCheckingSimilarity] = useState(false);
  const [similarHint, setSimilarHint] = useState(false);
  const [correctCount, setCorrectCount] = useState(0);
  const blankRef = useRef<HTMLInputElement>(null);
  const nextButtonRef = useRef<HTMLButtonElement>(null);
  const [autoAdding, setAutoAdding] = useState(false);
  const [autoAddError, setAutoAddError] = useState<string | null>(null);
  const [researching, setResearching] = useState<Set<string>>(new Set());

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

  // Research jobs and review-question regeneration are persisted server-side.
  // Polling the normal word list makes either completion visible without a
  // manual reload. The backend deduplicates these refresh-triggered jobs.
  useEffect(() => {
    const timer = window.setInterval(() => {
      const hasPendingWork = words.some((w) =>
        w.researchStatus === "pending" ||
        (w.status === "verified" && currentQuestion(w) === null),
      );
      if (!hasPendingWork) return;
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
  }, []);

  const researchControls = (word: WordReviewItem) => {
    if (word.researchStatus === "confirmed") return null;
    return (
      <>
        <button type="button" className="ghost word-research-btn" onClick={() => void handleResearch(word)} disabled={researching.has(word.id) || word.researchStatus === "pending"}>
          {researching.has(word.id) || word.researchStatus === "pending" ? "다시 찾는 중…" : "다시 검색"}
        </button>
        {(word.researchResults ?? []).map((s, i) => (
          <button key={i} type="button" className="ghost word-research-choice" onClick={() => void handleChooseMeaning(word, s)}>
            {s.meaning} · {s.example}
          </button>
        ))}
        {word.researchStatus !== "pending" && <button type="button" className="ghost word-research-btn" onClick={() => void handleConfirmResearch(word)}>확정</button>}
      </>
    );
  };

  const startQuiz = useCallback(() => {
    const now = Date.now() / 1000;
    const verified = words.filter((w) => w.status === "verified" && w.researchStatus === "confirmed");
    // A rolling deployment can briefly return a legacy row alongside a new
    // dueCount. Do not construct any question until its current version and
    // exact grammatical answer are both present.
    const due = verified.filter((w) => w.nextReviewAt <= now && currentQuestion(w) !== null);
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
  const recallQuestion = currentItem?.mode === "recall" ? currentQuestion(currentItem.word) : null;
  const [recallPrefix, recallSuffix] = recallQuestion?.prompt.split("___") ?? [];

  // Put the learner straight into the first answer field whenever a recall
  // question appears. This is especially important on mobile, where focusing
  // the field is what brings up the keyboard without an extra tap.
  useEffect(() => {
    if (!recallQuestion || checked || checkingSimilarity) return;
    blankRef.current?.focus();
  }, [recallQuestion, checked, checkingSimilarity]);

  const isCorrect =
    checked && currentItem
      ? currentItem.mode === "recall"
        ? normalizeAnswer(answer) === normalizeAnswer(recallQuestion!.answer)
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
      setQuizQueue((prev) => (prev ? [...prev, reshuffleRecognitionChoices(item)] : prev));
      void reviewWord(item.word.id, false, false, currentQuestion(item.word)!.version).then((updated) => {
        if (!updated) return;
        setWords((prev) => prev.map((w) => (w.id === updated.id ? updated : w)));
        // The item is also present in the in-progress queue. Keep that copy
        // in sync with the server immediately after a miss: an incorrect
        // answer resets stage to 0, and a later same-session retry must not
        // offer the "forced guess" action based on the stale pre-miss stage.
        setQuizQueue((prev) => prev?.map((queued) =>
          queued.word.id === updated.id ? { ...queued, word: updated } : queued,
        ) ?? prev);
        setDueCount((c) => Math.max(0, c - 1));
      });
    },
    [],
  );

  const checkRecall = useCallback(async () => {
    if (!currentItem || checked || checkingSimilarity || currentItem.mode !== "recall" || !recallQuestion || !answer.trim()) return;
    if (normalizeAnswer(answer) === normalizeAnswer(recallQuestion.answer)) {
      finishCheck(currentItem, true);
      return;
    }
    setCheckingSimilarity(true);
    const similar = await checkQuizAnswer(recallQuestion.prompt, recallQuestion.answer, undefined, answer);
    setCheckingSimilarity(false);
    if (similar) {
      setSimilarHint(true);
      setAnswer("");
      blankRef.current?.focus();
      return;
    }
    finishCheck(currentItem, false);
  }, [currentItem, checked, checkingSimilarity, recallQuestion, answer, finishCheck]);

  const checkRecognition = () => {
    if (!currentItem || checked || currentItem.mode !== "recognition" || selectedChoice === null) return;
    finishCheck(currentItem, selectedChoice === currentItem.word.meaning);
  };

  const skipQuestion = () => {
    if (!currentItem || checked || checkingSimilarity) return;
    // An unsubmitted draft must not count as correct when the learner
    // explicitly asks to reveal the answer instead.
    setAnswer("");
    setSelectedChoice(null);
    finishCheck(currentItem, false);
  };

  useEffect(() => {
    if (checked) nextButtonRef.current?.focus();
  }, [checked]);

  const advanceToNext = useCallback(() => {
    setIndex(index + 1);
    setAnswer("");
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
        void reviewWord(currentItem.word.id, true, repeat, currentQuestion(currentItem.word)!.version).then((updated) => {
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

  // Enter submits the sentence blank or, once checked, advances to the next
  // question so the learner never has to reach for the mouse mid-question.
  const handleBlankKeyDown = useCallback(
    (e: KeyboardEvent<HTMLInputElement>) => {
      if (e.key !== "Enter" || e.nativeEvent.isComposing) return;
      e.preventDefault();
      if (checked) {
        next();
        return;
      }
      checkRecall();
    },
    [checked, next, checkRecall],
  );

  const verifiedWords = words.filter((w) => w.status === "verified" && w.researchStatus === "confirmed");
  const nowSeconds = Date.now() / 1000;
  const readyDueCount = verifiedWords.filter((w) => w.nextReviewAt <= nowSeconds && currentQuestion(w) !== null).length;
  const dueQuestionBackfillCount = verifiedWords.filter((w) => w.nextReviewAt <= nowSeconds && currentQuestion(w) === null).length;
  const availableDueCount = Math.min(dueCount, readyDueCount);
  // Keep every word that still needs learner confirmation together, including
  // verified rows created from a direct/article definition but not yet
  // confirmed. Rejected rows remain in their own exclusion section below.
  const unconfirmedWords = words.filter((w) =>
    w.status !== "rejected" && (w.status === "pending" || w.researchStatus !== "confirmed"),
  );
  const rejectedWords = words.filter((w) => w.status === "rejected");

  const sortedVerifiedWords = [...verifiedWords].sort((a, b) => {
    const lastReviewedDifference = (b.lastReviewedAt ?? 0) - (a.lastReviewedAt ?? 0);
    return lastReviewedDifference !== 0 ? lastReviewedDifference : a.id.localeCompare(b.id);
  });
  const visibleVerifiedWords = sortedVerifiedWords.slice(0, reviewWordsPageSize * (reviewOlderCount + 1));
  const hasOlderReviewWords = visibleVerifiedWords.length < sortedVerifiedWords.length;
  const loadOlderReviewWords = () => {
    if (hasOlderReviewWords) setReviewOlderCount((count) => count + 1);
  };
  const isListView = quizQueue === null;

  // Start each view at its heading and primary action. Background polling
  // must not move the page or collapse words the learner already loaded.
  useLayoutEffect(() => {
    if (state === "ready" && reviewPageRef.current) reviewPageRef.current.scrollTop = 0;
  }, [state, isListView]);

  return (
    <div className="app">
      <SubPageHeader title="단어 복습" />

      <main
        className="convo word-review-page"
        ref={reviewPageRef}
        onScroll={(event) => {
          const page = event.currentTarget;
          if (isListView && state === "ready" && page.scrollHeight - page.scrollTop - page.clientHeight <= 80) {
            loadOlderReviewWords();
          }
        }}
      >
        {quizQueue === null && <LearningIntro eyebrow="다시 만날수록 익숙해지는 단어" title="배운 표현을 내 것으로 만들어요" description="복습할 때가 된 단어를 문장 속에서 떠올려 보세요. 아직 낯선 표현은 다시 연습할 수 있어요." steps={["단어 모으기", "문장으로 복습", "다시 익히기"]} />}
        {state === "loading" && <LoadingHint />}
        {state === "error" && <p className="hint" role="alert">단어 목록을 불러오지 못했어요. 연결 상태를 확인한 뒤 다시 열어 주세요.</p>}

        {state === "ready" && quizQueue === null && (
          <>
            <p className="hint word-review-due-hint" role="status">
              {availableDueCount > 0
                ? `복습할 단어 ${availableDueCount}개가 있어요.`
                : dueQuestionBackfillCount > 0
                  ? `복습 문제 ${dueQuestionBackfillCount}개를 새 버전으로 준비 중이에요.`
                  : "지금 복습할 단어가 없어요."}
            </p>
            {availableDueCount > 0 ? (
              <button type="button" className="quiz-start-btn" onClick={startQuiz}>
                복습 시작
              </button>
            ) : dueQuestionBackfillCount > 0 ? (
              <button type="button" className="quiz-start-btn" disabled>
                복습 문제 준비 중…
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

            {words.length === 0 && (
              <EmptyState title="아직 학습 중인 단어가 없어요." description="‘새 단어 추가로 학습하기’로 시작하거나, 대화에서 단어를 검색한 뒤 ‘학습하기’를 눌러 모아 보세요." />
            )}

            <WordListSection
              title="복습중인 단어"
              count={verifiedWords.length}
              words={visibleVerifiedWords}
              onLoadMore={hasOlderReviewWords ? loadOlderReviewWords : undefined}
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
          </>
        )}

        {state === "ready" && quizQueue !== null && (
          <section className="quiz-panel" aria-label="단어 복습 문제">
            <button type="button" className="ghost quiz-back-btn" onClick={backToList}>
              ← 목록으로
            </button>
            {current === null || currentItem === null ? (
              <div className="quiz-question">
                <div className="section-heading"><h2>복습 결과</h2></div>
                <div className="quiz-score" role="status">
                  {quizQueue.length === 0
                    ? "복습할 단어가 없어요."
                    : `${quizQueue.length}개 중 ${correctCount}개 맞혔어요!`}
                </div>
                <button autoFocus type="button" className="quiz-next-btn" onClick={backToList}>
                  완료
                </button>
              </div>
            ) : (
              <div className="quiz-question">
                <div className="section-heading history-heading">
                  <h2>{currentItem.mode === "recall" ? "문장 속 단어 떠올리기" : "단어의 뜻 고르기"}</h2>
                  <span className="quiz-progress" aria-label="문제 진행">{index + 1} / {quizQueue.length}</span>
                </div>
                <div className="quiz-progress" role="note">
                  마지막 복습: {formatReviewAge(current.lastReviewedAt)}
                </div>
                {currentItem.mode === "recall" ? (
                  <>
                    <p className="quiz-meaning-hint">{current.meaning}</p>
                    {/* The answer is typed directly in place inside the
                        sentence rather than in a separate textarea. Its
                        width follows what has been typed, not the hidden
                        answer's length, which would give it away. */}
                    <div className="quiz-prompt quiz-blank-sentence" lang="en">
                      <span>
                        {recallPrefix}
                        <input
                          autoFocus
                          ref={blankRef}
                          type="text"
                          className={quizBlankInputClass(
                            checked,
                            normalizeAnswer(answer) === normalizeAnswer(recallQuestion!.answer),
                          )}
                          style={{ width: `${Math.min(16, Math.max(3, answer.length + 1))}ch` }}
                          maxLength={255}
                          value={answer}
                          onChange={(e) => setAnswer(e.target.value)}
                          onKeyDown={handleBlankKeyDown}
                          disabled={checked || checkingSimilarity}
                          aria-label="정답 입력"
                        />
                      </span>
                      <span>{recallSuffix}</span>
                    </div>
                  </>
                ) : (
                  <>
                    <div className="quiz-prompt" lang="en"><strong>{current.word}</strong></div>
                    <div className="quiz-prompt" lang="en">{current.example}</div>
                    <QuizChoices
                      options={currentItem.choices!}
                      selectedIndex={selectedChoice === null ? null : currentItem.choices!.indexOf(selectedChoice)}
                      correctIndex={checked ? currentItem.choices!.indexOf(current.meaning) : undefined}
                      label="단어의 뜻"
                      onSelect={(index) => setSelectedChoice(currentItem.choices![index])}
                    />
                  </>
                )}
                {!checked && (
                  <>
                    {similarHint && <p className="quiz-result similar" role="status">유사한 정답이에요! 다시 입력해보세요.</p>}
                    <div className="quiz-next-actions">
                      <button
                        type="button"
                        className="quiz-check-btn"
                        onClick={() => currentItem.mode === "recall" ? void checkRecall() : checkRecognition()}
                        disabled={checkingSimilarity || (currentItem.mode === "recall" ? !answer.trim() : selectedChoice === null)}
                      >
                        {checkingSimilarity ? "채점 중…" : "답안 확인"}
                      </button>
                      <button type="button" className="ghost quiz-forced-btn" onClick={skipQuestion} disabled={checkingSimilarity}>
                        잘 모르겠어요
                      </button>
                    </div>
                  </>
                )}
                {checked && (
                  <>
                    <div className={`quiz-result ${isCorrect ? "correct" : "incorrect"}`} role="status">
                      {isCorrect
                        ? "정답이에요!"
                        : `아쉬워요. 정답: ${currentItem.mode === "recall" ? recallQuestion!.answer : current.meaning}`}
                    </div>
                    <div className="quiz-next-actions">
                      <button ref={nextButtonRef} type="button" className="quiz-next-btn" onClick={next}>
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
          </section>
        )}
      </main>
    </div>
  );
}

function WordListSection({ title, words, count = words.length, rowClassName, renderMeta, onDelete, onLoadMore }: {
  title: string;
  words: WordReviewItem[];
  count?: number;
  rowClassName?: string;
  renderMeta: (w: WordReviewItem) => React.ReactNode;
  onDelete: (id: string) => void;
  onLoadMore?: () => void;
}) {
  const titleId = useId();
  if (words.length === 0) return null;
  return (
    <section className="word-list-section" aria-labelledby={titleId}>
      <div className="section-heading history-heading">
        <h2 id={titleId}>{title}</h2>
        <p>{count}개</p>
      </div>
      <ul className="word-list">
        {words.map((w) => (
          <li key={w.id} className="session-row">
            <div className={`session-item word-list-item${rowClassName ? ` ${rowClassName}` : ""}`}>
              <span className="title" lang="en">{w.word}</span>
              <span className="word-list-meaning">{w.meaning}</span>
              {renderMeta(w)}
            </div>
            <button
              type="button"
              className="ghost icon-btn session-delete"
              onClick={() => onDelete(w.id)}
              aria-label="단어 삭제"
              title="단어 삭제"
            >
              🗑
            </button>
          </li>
        ))}
      </ul>
      {onLoadMore && <button type="button" className="ghost" onClick={onLoadMore}>이전 단어 더 보기</button>}
    </section>
  );
}
