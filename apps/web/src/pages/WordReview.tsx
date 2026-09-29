import { Fragment, useCallback, useEffect, useId, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { QuizChoices } from "../components/QuizChoices";
import { QuizBlankInput } from "../components/QuizBlankInput";
import { WordMeaningReview } from "../components/WordMeaningReview";
import { WordDuplicateNotice } from "../components/WordDuplicateNotice";
import { EmptyState, LearningIntro } from "../components/LearningIntro";
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
  selectResearchWord,
  selectWordMeaning,
  meaningNeedsReview,
  wordStudyStatus,
  findSameWordEntries,
  type MeaningChoice,
} from "../lib/wordReview";
import type { WordSuggestion } from "../lib/protocol";
import { formatAbsoluteDateTime } from "../lib/time";
import { buildReviewQueue, currentQuestion, reshuffleRecognitionChoices, type QuizItem } from "../lib/wordReviewQuiz";
import { checkQuizAnswer, normalizeQuizAnswer as normalizeAnswer } from "../lib/quizCheck";
import { LearningPage } from "../components/LearningPage";
import { usePollScaffold } from "../hooks/usePollScaffold";
import { LoadingHint } from "../components/LoadingHint";
import type { LoadState } from "../lib/loadState";
import { newestFirst } from "../lib/listView";

// How often to re-check an auto-add job that's still generating in the
// background (see asyncjob.KindWordAutoAdd) — a poll, not a push, same
// reasoning as ArticleQuiz.tsx's articleStudyPollIntervalMs.
const wordAutoAddPollIntervalMs = 3000;
const reviewWordsPageSize = 20;

function formatReviewAge(unixSeconds: number | undefined): string {
  if (!unixSeconds) return "아직 복습한 적 없음";
  const days = Math.max(0, Math.floor((Date.now() / 1000 - unixSeconds) / (24 * 60 * 60)));
  return days === 0 ? "오늘 복습함" : `${days}일 전 복습함`;
}

export function WordReview() {
  const [selectingMeanings, setSelectingMeanings] = useState<Set<string>>(new Set());
  const [meaningSelectionErrors, setMeaningSelectionErrors] = useState<Record<string, string | undefined>>({});
  const meaningSelections = useRef(new Set<string>());
  const [state, setState] = useState<LoadState>("loading");
  const [words, setWords] = useState<WordReviewItem[]>([]);
  const sameWordEntries = useMemo(() => findSameWordEntries(words), [words]);
  const [dueCount, setDueCount] = useState(0);
  // Match the article history: recent reviews first, older entries below.
  // The page owns scrolling so nested lists do not trap touch gestures.
  const [reviewOlderCount, setReviewOlderCount] = useState(0);

  // null = list view; an array (possibly empty) = quiz in progress, built
  // once from the due words at the moment "복습 시작" was pressed so the
  // question order/mode stays stable even as reviews update `words` below.
  const [quizQueue, setQuizQueue] = useState<QuizItem[] | null>(null);
  const [index, setIndex] = useState(0);
  // Current recall answers, typed directly into the generated sentence's
  // lexical blanks rather than a separate free-text box. There can be more
  // than one when a context-only pronoun stays visible between phrase parts.
  const [answers, setAnswers] = useState<string[]>([]);
  const [selectedChoice, setSelectedChoice] = useState<string | null>(null);
  const [checked, setChecked] = useState(false);
  const [checkingSimilarity, setCheckingSimilarity] = useState(false);
  const [similarHint, setSimilarHint] = useState(false);
  const [correctCount, setCorrectCount] = useState(0);
  const blankRefs = useRef<Array<HTMLInputElement | null>>([]);
  const nextButtonRef = useRef<HTMLButtonElement>(null);
  const [autoAdding, setAutoAdding] = useState(false);
  const [autoAddError, setAutoAddError] = useState<string | null>(null);
  const [researching, setResearching] = useState<Set<string>>(new Set());
  const [selectingResearch, setSelectingResearch] = useState<Set<string>>(new Set());
  const [researchErrors, setResearchErrors] = useState<Record<string, string>>({});
  const wordsRequest = useRef(0);

  const refreshWords = useCallback(async () => {
    const request = ++wordsRequest.current;
    const result = await fetchWords();
    // A poll started before a choice was saved must not restore that old
    // choice, even when its response arrives after the newer list.
    if (result && request === wordsRequest.current) {
      setWords(result.words);
      setDueCount(result.dueCount);
    }
    return result;
  }, []);

  // Poll scaffolding for an auto-add job still generating in the background
  // (see usePollScaffold's doc comment). Losing this component (navigating
  // away, or the tab closing) only stops *watching* —
  // asyncjob.KindWordAutoAdd keeps generating regardless (see
  // lib/wordReview.ts's startAutoAddWords doc comment); reopening this page
  // resumes watching via the mount effect below.
  const { tokenRef: pollTokenRef, startPoll } = usePollScaffold();

  // Polls the auto-add job's status until it leaves "pending" — started
  // either right after pressing "새 단어 추가로 학습하기" or, on mount, when
  // reopening this page finds one already in flight (see the mount effect
  // below).
  const pollAutoAdd = useCallback((token: object) => startPoll(token, {
    intervalMs: wordAutoAddPollIntervalMs,
    fetchResult: fetchAutoAddStatus,
    onResult: (status) => {
      if (!status || status.status === "pending") return true;
      setAutoAdding(false);
      if (status.status === "failed") {
        setAutoAddError("단어를 추가하지 못했어요. 잠시 후 다시 시도해주세요.");
        return false;
      }
      // "done" (or "" — treated the same as an already-finished idle state).
      if (status.count === 0) {
        setAutoAddError("추천할 새 단어를 찾지 못했어요. 잠시 후 다시 시도해주세요.");
      }
      void refreshWords();
      return false;
    },
  }), [startPoll, refreshWords]);

  useEffect(() => {
    refreshWords().then((result) => {
      if (result === null) {
        setState("error");
        return;
      }
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
  }, [pollAutoAdd, refreshWords]);

  // Research jobs and review-question regeneration are persisted server-side.
  // Polling the normal word list makes either completion visible without a
  // manual reload. The backend deduplicates these refresh-triggered jobs.
  useEffect(() => {
    const timer = window.setInterval(() => {
      const hasPendingWork = words.some((w) =>
        w.status === "pending" ||
        w.meaningStatus === "pending" ||
        w.researchStatus === "pending" ||
        (w.status === "verified" && currentQuestion(w) === null),
      );
      if (!hasPendingWork) return;
      void refreshWords();
    }, 2000);
    return () => window.clearInterval(timer);
  }, [words, refreshWords]);

  const handleDelete = (id: string) => confirmThenDelete("이 단어를 삭제할까요?", deleteWord, id, setWords);

  const handleSelectMeaning = async (word: WordReviewItem, choice: MeaningChoice) => {
    if (meaningSelections.current.has(word.id)) return;
    meaningSelections.current.add(word.id);
    setSelectingMeanings(new Set(meaningSelections.current));
    setMeaningSelectionErrors((prev) => ({ ...prev, [word.id]: undefined }));
    const updated = await selectWordMeaning(word, choice);
    if (updated) {
      wordsRequest.current++;
      setWords((prev) => prev.map((w) => w.id === updated.id ? updated : w));
      // Confirmation can make a previously withheld word due immediately.
      await refreshWords();
    } else {
      setMeaningSelectionErrors((prev) => ({ ...prev, [word.id]: "선택을 저장하지 못했어요. 다시 시도해 주세요." }));
    }
    meaningSelections.current.delete(word.id);
    setSelectingMeanings(new Set(meaningSelections.current));
  };

  const renderMeaning = (word: WordReviewItem) => (
    <>
      <WordMeaningReview word={word} selecting={selectingMeanings.has(word.id)} error={meaningSelectionErrors[word.id]}
        onSelect={(choice) => void handleSelectMeaning(word, choice)} />
      <WordDuplicateNotice words={sameWordEntries.get(word.id) ?? []} />
    </>
  );

  const handleResearch = useCallback(async (word: WordReviewItem) => {
    setResearchErrors((prev) => ({ ...prev, [word.id]: "" }));
    setResearching((prev) => new Set(prev).add(word.id));
    const updated = await startResearchWord(word.id);
    setResearching((prev) => { const next = new Set(prev); next.delete(word.id); return next; });
    if (updated) setWords((prev) => prev.map((w) => w.id === updated.id ? updated : w));
    else setResearchErrors((prev) => ({ ...prev, [word.id]: "검색을 시작하지 못했어요. 다시 시도해 주세요." }));
  }, []);

  const handleConfirmResearch = useCallback(async (word: WordReviewItem) => {
    // Confirmation can intentionally restore a rejected word, but it must
    // never bypass the initial model verification while that check is still
    // pending. The server enforces the same boundary for stale clients.
    if (word.status === "pending") return;
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
    setSelectingResearch((prev) => new Set(prev).add(oldWord.id));
    setResearchErrors((prev) => ({ ...prev, [oldWord.id]: "" }));
    const saved = await selectResearchWord(oldWord, suggestion);
    setSelectingResearch((prev) => { const next = new Set(prev); next.delete(oldWord.id); return next; });
    if (saved) setWords((prev) => prev.map((w) => w.id === oldWord.id ? saved : w));
    else setResearchErrors((prev) => ({ ...prev, [oldWord.id]: "예문을 적용하지 못했어요. 이미 학습 중인 뜻이거나 검색 결과가 바뀌었을 수 있어요. 다시 검색해 주세요." }));
  }, []);

  const researchControls = (word: WordReviewItem) => {
    if (word.researchStatus === "confirmed" || meaningNeedsReview(word)) return null;
    const pending = researching.has(word.id) || word.researchStatus === "pending";
    const selecting = selectingResearch.has(word.id);
    const results = word.researchStatus === "done" ? (word.researchResults ?? []).filter((s) => s.verified) : [];
    return (
      <>
        <button type="button" className="ghost word-research-btn" onClick={() => void handleResearch(word)} disabled={pending || selecting || word.status === "pending"}>
          {pending ? "예문을 찾고 검증하는 중…" : "다시 검색"}
        </button>
        {word.researchStatus === "failed" && <p role="status">검증된 새 예문을 찾지 못했어요. 기존 단어는 유지했어요.</p>}
        {word.researchStatus === "done" && results.length === 0 && <p role="status">검증된 예문이 없어요. 다시 검색해 주세요.</p>}
        {researchErrors[word.id] && <p role="alert">{researchErrors[word.id]}</p>}
        {results.map((s, i) => (
          <button key={i} type="button" className="ghost word-research-choice" onClick={() => void handleChooseMeaning(word, s)} disabled={pending || selecting || word.status === "pending"}>
            {s.meaning} · {s.example}
          </button>
        ))}
        {word.researchStatus !== "pending" && (
          <button
            type="button"
            className="ghost word-research-btn"
            onClick={() => void handleConfirmResearch(word)}
            disabled={word.status === "pending" || pending || selecting}
          >
            {word.status === "pending" ? "검증 중…" : "확정"}
          </button>
        )}
      </>
    );
  };

  const resetQuestion = useCallback(() => {
    setAnswers([]);
    setSelectedChoice(null);
    setChecked(false);
    setCheckingSimilarity(false);
    setSimilarHint(false);
  }, []);

  const startQuiz = useCallback(() => {
    setQuizQueue(buildReviewQueue(words, Date.now() / 1000));
    setIndex(0);
    resetQuestion();
    setCorrectCount(0);
  }, [words, resetQuestion]);

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
  const recallParts = recallQuestion?.prompt.split("___") ?? [];
  const recallAnswers = recallQuestion?.answers.map((_, i) => answers[i] ?? "") ?? [];
  const recallIsExact = recallQuestion !== null && recallQuestion.answers.every(
    (expected, i) => normalizeAnswer(recallAnswers[i]) === normalizeAnswer(expected),
  );
  const recallIsComplete = recallQuestion !== null && recallAnswers.every((answer) => answer.trim());

  // Put the learner straight into the first answer field whenever a recall
  // question appears. This is especially important on mobile, where focusing
  // the field is what brings up the keyboard without an extra tap.
  useEffect(() => {
    if (!recallQuestion || checked || checkingSimilarity) return;
    blankRefs.current.find((input) => input && !input.value.trim())?.focus();
  }, [recallQuestion, checked, checkingSimilarity]);

  const isCorrect =
    checked && currentItem
      ? currentItem.mode === "recall"
        ? recallIsExact
        : selectedChoice === currentItem.word.meaning
      : false;

  const submitReview = useCallback(async (item: QuizItem, correct: boolean, repeat = false) => {
    const updated = await reviewWord(item.word.id, correct, repeat, currentQuestion(item.word)!.version);
    if (!updated) return;
    setWords((prev) => prev.map((w) => w.id === updated.id ? updated : w));
    setDueCount((count) => Math.max(0, count - 1));
    if (!correct) {
      // A miss resets stage to 0. Update queued retries too, so they cannot
      // offer the forced-guess action based on the word's pre-miss stage.
      setQuizQueue((prev) => prev?.map((queued) =>
        queued.word.id === updated.id ? { ...queued, word: updated } : queued,
      ) ?? prev);
    }
  }, []);

  const finishCheck = useCallback(
    (item: QuizItem, correct: boolean) => {
      setChecked(true);
      if (correct) {
        // The review call for a correct answer is deferred until the
        // learner advances rather than fired here,
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
      void submitReview(item, false);
    },
    [submitReview],
  );

  const checkRecall = useCallback(async () => {
    if (!currentItem || checked || checkingSimilarity || currentItem.mode !== "recall" || !recallQuestion || !recallIsComplete) return;
    if (recallIsExact) {
      finishCheck(currentItem, true);
      return;
    }
    setCheckingSimilarity(true);
    const similar = await checkQuizAnswer(
      recallQuestion.prompt,
      recallQuestion.answers.join(" | "),
      undefined,
      recallAnswers.join(" | "),
      { wordId: currentItem.word.id },
    );
    setCheckingSimilarity(false);
    if (similar) {
      setSimilarHint(true);
      setAnswers([]);
      blankRefs.current[0]?.focus();
      return;
    }
    finishCheck(currentItem, false);
  }, [currentItem, checked, checkingSimilarity, recallQuestion, recallAnswers, recallIsComplete, recallIsExact, finishCheck]);

  const checkRecognition = () => {
    if (!currentItem || checked || currentItem.mode !== "recognition" || selectedChoice === null) return;
    finishCheck(currentItem, selectedChoice === currentItem.word.meaning);
  };

  const skipQuestion = () => {
    if (!currentItem || checked || checkingSimilarity) return;
    // An unsubmitted draft must not count as correct when the learner
    // explicitly asks to reveal the answer instead.
    setAnswers([]);
    setSelectedChoice(null);
    finishCheck(currentItem, false);
  };

  useEffect(() => {
    if (checked) nextButtonRef.current?.focus();
  }, [checked]);

  const advanceToNext = useCallback((repeat = false) => {
    if (isCorrect && currentItem) {
      // Correct answers wait until this choice: a forced guess repeats the
      // current interval instead of advancing to the next, wider one.
      void submitReview(currentItem, true, repeat);
    }
    setIndex(index + 1);
    resetQuestion();
  }, [isCorrect, currentItem, submitReview, index, resetQuestion]);

  // Enter submits the sentence blank or, once checked, advances to the next
  // question so the learner never has to reach for the mouse mid-question.
  const handleBlankKeyDown = useCallback(
    (e: KeyboardEvent<HTMLInputElement>, blankIndex: number) => {
      if (e.key !== "Enter" || e.nativeEvent.isComposing) return;
      e.preventDefault();
      if (checked) {
        advanceToNext();
        return;
      }
      const nextEmpty = blankRefs.current.findIndex((input, i) => i > blankIndex && input && !input.value.trim());
      if (nextEmpty >= 0) {
        blankRefs.current[nextEmpty]?.focus();
        return;
      }
      checkRecall();
    },
    [checked, advanceToNext, checkRecall],
  );

  const confirmedWords = words.filter((w) => wordStudyStatus(w) === "confirmed");
  const unconfirmedWords = words.filter((w) => wordStudyStatus(w) === "unconfirmed");
  const rejectedWords = words.filter((w) => wordStudyStatus(w) === "rejected");
  const nowSeconds = Date.now() / 1000;
  const readyDueCount = confirmedWords.filter((w) => w.nextReviewAt <= nowSeconds && currentQuestion(w) !== null).length;
  const dueQuestionBackfillCount = confirmedWords.filter((w) => w.nextReviewAt <= nowSeconds && currentQuestion(w) === null).length;
  const availableDueCount = Math.min(dueCount, readyDueCount);
  const pendingMeaningCount = words.filter((w) => w.meaningStatus === "pending").length;
  const completedMeaningCount = words.filter((w) => w.meaningStatus === "done").length;

  const sortedReviewWords = newestFirst(confirmedWords, (word) => word.lastReviewedAt ?? 0);
  const visibleReviewWords = sortedReviewWords.slice(0, reviewWordsPageSize * (reviewOlderCount + 1));
  const hasOlderReviewWords = visibleReviewWords.length < sortedReviewWords.length;
  const loadOlderReviewWords = () => {
    if (hasOlderReviewWords) setReviewOlderCount((count) => count + 1);
  };
  const isListView = quizQueue === null;

  return (
    <LearningPage
      title="단어 복습"
      viewKey={isListView ? "list" : "quiz"}
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

          {words.some((w) => w.status === "verified") && (
            <div>
              <p className="hint">새 버전에 맞춰 단어 뜻을 자동으로 다시 확인해요. 직접 확정해야 새 버전이 적용되며, 복습 진도는 유지돼요.</p>
              {pendingMeaningCount > 0 && <p className="hint" role="status">뜻 {pendingMeaningCount}개 정리 중이에요. 화면을 나가도 계속 진행돼요.</p>}
              {completedMeaningCount > 0 && <p className="hint" role="status">정리한 뜻 {completedMeaningCount}개를 확인하고 선택해 주세요.</p>}
            </div>
          )}

          {words.length === 0 && (
            <EmptyState title="아직 학습 중인 단어가 없어요." description="‘새 단어 추가로 학습하기’로 시작하거나, 대화에서 단어를 검색한 뒤 ‘학습하기’를 눌러 모아 보세요." />
          )}

          {/* Keep the two decision queues ahead of the paginated review
              history. Otherwise every newly loaded history page pushes
              pending/rejected words farther away, making them effectively
              unreachable for learners with a large vocabulary. */}
          <WordListSection
            title="확정 전 단어"
            words={unconfirmedWords}
            renderMeaning={renderMeaning}
            onDelete={(id) => void handleDelete(id)}
            renderMeta={(w) => <><span className="word-list-next">확정 전</span>{researchControls(w)}</>}
          />
          <WordListSection
            title="제외된 단어"
            words={rejectedWords}
            renderMeaning={renderMeaning}
            rowClassName="word-list-row-rejected"
            onDelete={(id) => void handleDelete(id)}
            renderMeta={(w) => (
              <>
                {w.verifyReason && <span className="word-list-reject-reason">{w.verifyReason}</span>}
                {researchControls(w)}
              </>
            )}
          />
          <WordListSection
            title="복습중인 단어"
            count={confirmedWords.length}
            words={visibleReviewWords}
            renderMeaning={renderMeaning}
            onLoadMore={hasOlderReviewWords ? loadOlderReviewWords : undefined}
            onDelete={(id) => void handleDelete(id)}
            renderMeta={(w) => <span className="word-list-next">다음 복습: {formatAbsoluteDateTime(w.nextReviewAt)}</span>}
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
                  <div className="quiz-prompt quiz-blank-sentence" lang="en">
                    {recallParts.map((part, blankIndex) => (
                      <Fragment key={blankIndex}>
                        <span>{part}</span>
                        {blankIndex < recallQuestion!.answers.length && (
                          <QuizBlankInput
                            autoFocus={blankIndex === 0}
                            ref={(input) => { blankRefs.current[blankIndex] = input; }}
                            checked={checked}
                            correct={normalizeAnswer(recallAnswers[blankIndex]) === normalizeAnswer(recallQuestion!.answers[blankIndex])}
                            maxLength={255}
                            value={recallAnswers[blankIndex]}
                            onChange={(e) => setAnswers((currentAnswers) => {
                              const nextAnswers = [...currentAnswers];
                              nextAnswers[blankIndex] = e.target.value;
                              return nextAnswers;
                            })}
                            onKeyDown={(e) => handleBlankKeyDown(e, blankIndex)}
                            disabled={checkingSimilarity}
                            aria-label={recallQuestion!.answers.length === 1 ? "정답 입력" : `정답 ${blankIndex + 1} 입력`}
                          />
                        )}
                      </Fragment>
                    ))}
                  </div>
                </>
              ) : (
                <>
                  <div className="quiz-prompt" lang="en"><strong>{current.word}</strong></div>
                  <div className="quiz-prompt" lang="en">{current.example}</div>
                  <QuizChoices
                    options={currentItem.choices}
                    selectedIndex={selectedChoice === null ? null : currentItem.choices.indexOf(selectedChoice)}
                    correctIndex={checked ? currentItem.choices.indexOf(current.meaning) : undefined}
                    label="단어의 뜻"
                    onSelect={(index) => setSelectedChoice(currentItem.choices[index])}
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
                      disabled={checkingSimilarity || (currentItem.mode === "recall" ? !recallIsComplete : selectedChoice === null)}
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
                      : `아쉬워요. 정답: ${currentItem.mode === "recall" ? recallQuestion!.answers.join(", ") : current.meaning}`}
                  </div>
                  <div className="quiz-next-actions">
                    <button ref={nextButtonRef} type="button" className="quiz-next-btn" onClick={() => advanceToNext()}>
                      {index + 1 < quizQueue.length ? "다음 단어" : "결과 보기"}
                    </button>
                    {isCorrect && current.stage > 0 && (
                      <button
                        type="button"
                        className="ghost quiz-forced-btn"
                        onClick={() => advanceToNext(true)}
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
    </LearningPage>
  );
}

function WordListSection({ title, words, count = words.length, rowClassName, renderMeaning, renderMeta, onDelete, onLoadMore }: {
  title: string;
  words: WordReviewItem[];
  count?: number;
  rowClassName?: string;
  renderMeaning: (w: WordReviewItem) => React.ReactNode;
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
              {renderMeaning(w)}
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
