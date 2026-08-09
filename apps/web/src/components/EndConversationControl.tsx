import { useCallback, useEffect, useRef, useState } from "react";
import type { QuizQuestion, StudySummarySentence } from "../lib/protocol";
import { markQuizCompleted } from "../lib/sessions";
import { useDismiss } from "../hooks/useDismiss";
import { checkQuizAnswer, normalizeQuizAnswer, quizBlankInputClass } from "../lib/quizCheck";

// Per-question quiz progress, hoisted out of QuizPanel itself into
// EndConversationControl (see quizProgress there). QuizPanel is only
// conditionally mounted (quizMode && ...), and the whole popover (including
// it) unmounts whenever `open` goes false — so if this lived as QuizPanel's
// own useState, dismissing the popover with an outside click and reopening
// it would always restart the quiz from question 1. Keeping it in a
// component that stays mounted for the life of the session means reopening
// just re-renders QuizPanel with whatever progress was already made.
interface QuizProgress {
  index: number;
  answer: string;
  checked: boolean;
  // True while an answer that didn't match answer/acceptableAnswers
  // literally is being double-checked against checkQuizAnswer's LLM fallback
  // (see QuizPanel's check()) — distinct from `checked`, which only flips
  // once that fallback (if any) has actually resolved, so the UI shows
  // "확인하는 중…" instead of prematurely revealing a result.
  checking: boolean;
  correct: boolean;
  correctCount: number;
}
const initialQuizProgress: QuizProgress = {
  index: 0,
  answer: "",
  checked: false,
  checking: false,
  correct: false,
  correctCount: 0,
};

// Learner-triggered wrap-up. Confirming "end this conversation" freezes the
// room read-only immediately (see endSession/httpserver.sessionEndHandler) —
// the study-summary synthesis (every grammar/vocabulary/phrasing/context
// issue flagged so far, folded into one "what to study next" recommendation)
// happens as a background job from there, not something this panel waits
// on: onEnd fires, and the learner is back on the list, before that LLM call
// has even started. Reopening an ended room shows whatever the room's own
// SessionDetail already carries — ended/studySummary/studySummaryStatus —
// with "pending"/"failed" kept fresh by pollStudySummary (see enterChat)
// while the background job is still working.
export function EndConversationControl({
  sessionId,
  ended,
  studySummary,
  studySummaryStatus,
  quiz,
  quizStatus,
  quizCompleted,
  onEnd,
  onRestudy,
  onQuizCompleted,
  onQuizReset,
}: {
  sessionId: string | null;
  ended: boolean;
  studySummary: StudySummarySentence[];
  studySummaryStatus: "pending" | "done" | "failed";
  quiz: QuizQuestion[];
  quizStatus: "pending" | "done" | "failed";
  quizCompleted: boolean;
  onEnd: () => void;
  onRestudy: () => void;
  onQuizCompleted: () => void;
  onQuizReset: () => void;
}) {
  const [open, setOpen] = useState(false);
  // quizMode lives here (not inside QuizPanel) so it survives QuizPanel
  // unmounting/remounting as the popover is closed and reopened — see the
  // quiz progress state just below for why that no longer means losing
  // place. The questions themselves are a prop now (pre-generated alongside
  // the wrap-up — see App's endedQuiz/pollQuizStatus), not fetched on
  // demand here anymore.
  const [quizMode, setQuizMode] = useState(false);
  // Local-only echo of "내가 읽었음" being tapped this visit, so the button
  // swaps to a confirmation instantly rather than waiting on quizCompleted
  // to round-trip back through App's own state.
  const [acknowledged, setAcknowledged] = useState(false);
  const [quizProgress, setQuizProgress] = useState<QuizProgress>(initialQuizProgress);
  const panelRef = useRef<HTMLDivElement>(null);

  const resetQuizProgress = useCallback(() => {
    setQuizProgress(initialQuizProgress);
  }, []);

  // A new question set (e.g. after "퀴즈 다시 만들기" — see onQuizReset)
  // invalidates any progress made against the old one. pollQuizStatus stops
  // polling once quizStatus is "done", so this doesn't fire again mid-quiz.
  useEffect(() => {
    resetQuizProgress();
  }, [quiz, resetQuizProgress]);

  // Dismissing the popover (outside click/Escape) only hides it — it must
  // not reset quizMode/progress, or reopening would look identical to a
  // real restart even though the state above is preserved.
  const close = useCallback(() => {
    setOpen(false);
  }, []);
  useDismiss(open, panelRef, close);

  const toggle = useCallback(() => {
    if (!sessionId) return;
    if (open) {
      close();
    } else {
      setOpen(true);
    }
  }, [sessionId, open, close]);

  const startQuiz = useCallback(() => {
    setQuizMode(true);
  }, []);

  const backToSummary = useCallback(() => {
    setQuizMode(false);
  }, []);

  // Acknowledges a quiz with nothing to ask about (see quiz.length === 0
  // below) — the same "studied this" checkmark a fully-correct quiz sets
  // (see QuizPanel), just without any questions to answer first.
  const acknowledgeNoQuiz = useCallback(async () => {
    if (!sessionId) return;
    setAcknowledged(true);
    const ok = await markQuizCompleted(sessionId);
    if (ok) onQuizCompleted();
  }, [sessionId, onQuizCompleted]);

  if (!sessionId) return null;

  return (
    <div className="end-conversation" ref={panelRef}>
      <button
        type="button"
        className="ghost icon-btn end-conversation-btn"
        aria-haspopup="true"
        aria-expanded={open}
        aria-label="대화 종료"
        title="대화 종료"
        onClick={toggle}
      >
        🎓
      </button>
      {open && (
        <div className="study-panel end-conversation-panel" role="menu">
          {!ended && (
            <>
              <div className="compaction-summary-header">
                대화를 종료할까요? 종료하면 지금까지의 대화를 바탕으로 학습 피드백을 정리해요.
              </div>
              <button type="button" className="end-conversation-confirm" onClick={onEnd}>
                예, 종료할래요
              </button>
            </>
          )}
          {ended && studySummaryStatus === "pending" && (
            <div className="compaction-loading" role="status">
              학습 피드백을 정리하는 중…
            </div>
          )}
          {ended && studySummaryStatus === "failed" && (
            <div className="compaction-empty" role="status">
              학습 피드백을 정리하지 못했어요. 잠시 후 다시 확인해주세요.
            </div>
          )}
          {ended && studySummaryStatus === "done" && !quizMode && (
            <>
              <div className="compaction-summary-header">
                {studySummary.length > 0
                  ? "이 대화는 종료됐어요. 그때의 학습 피드백이에요."
                  : "이번 대화에서는 딱히 걸린 부분이 없었어요. 아주 잘했어요!"}
              </div>
              {studySummary.length > 0 && (
                <div className="compaction-summary-text">
                  {studySummary.map((s, i) => (
                    <p key={i} className="study-summary-sentence">
                      <span className="study-summary-en">{s.english}</span>
                      <span className="study-summary-ko">{s.translation}</span>
                    </p>
                  ))}
                </div>
              )}
              {studySummary.length > 0 && quiz.length > 0 && (
                <div className="quiz-actions">
                  {/* Once quizCompleted is set, this quiz is done for good —
                      finishing it (right or wrong, see QuizPanel's finalize)
                      is what a learner leaving and coming back later must
                      see as "already studied", not a dangling invitation to
                      redo it. The only way to get a fresh attempt is
                      regenerating a new question set below. */}
                  {quizCompleted ? (
                    <div className="quiz-acknowledged" role="status">
                      학습 완료로 표시했어요.
                    </div>
                  ) : (
                    <button type="button" className="quiz-start-btn" onClick={startQuiz}>
                      퀴즈 풀기
                    </button>
                  )}
                  <button type="button" className="ghost quiz-reset-btn" onClick={onQuizReset}>
                    퀴즈 다시 만들기
                  </button>
                </div>
              )}
              {studySummary.length > 0 && quiz.length === 0 && quizStatus === "done" && (
                quizCompleted || acknowledged ? (
                  <div className="quiz-acknowledged" role="status">
                    학습 완료로 표시했어요.
                  </div>
                ) : (
                  <button type="button" className="quiz-ack-btn" onClick={() => void acknowledgeNoQuiz()}>
                    내가 읽었음
                  </button>
                )
              )}
              {studySummary.length > 0 && quiz.length === 0 && quizStatus !== "done" && (
                <div className="quiz-preparing" role="status">
                  퀴즈를 준비하는 중…
                </div>
              )}
              {studySummary.length === 0 && (
                <button type="button" className="restudy-btn" onClick={onRestudy}>
                  다시 확인하기
                </button>
              )}
            </>
          )}
          {ended && studySummaryStatus === "done" && quizMode && (
            <QuizPanel
              sessionId={sessionId}
              questions={quiz}
              progress={quizProgress}
              setProgress={setQuizProgress}
              onBack={backToSummary}
              onCompleted={onQuizCompleted}
            />
          )}
        </div>
      )}
    </div>
  );
}

// isQuizAnswerAccepted checks a typed answer against QuizQuestion.answer AND
// every listed acceptableAnswers — a close synonym the model already vetted
// as fitting this exact blank (see quizSystemPrompt server-side) shouldn't
// be marked wrong just for not being the one word the model happened to
// list first.
function isQuizAnswerAccepted(question: QuizQuestion, raw: string): boolean {
  const normalized = normalizeQuizAnswer(raw);
  if (normalized === normalizeQuizAnswer(question.answer)) return true;
  return (question.acceptableAnswers ?? []).some((a) => normalizeQuizAnswer(a) === normalized);
}

// The fill-in-the-blank practice quiz shown in place of the study summary
// once a learner taps "퀴즈 풀기" (see EndConversationControl, which only
// shows that button once questions is non-empty — pre-generated alongside
// the wrap-up, so there's nothing left to fetch or wait on here). One
// question at a time; typing an answer and confirming reveals whether it
// matched (see isQuizAnswerAccepted) plus the explanation/translation, then
// advances — ending on a plain right/total score. Finishing the last
// question — right or wrong, since going back to reread the wrap-up and
// retaking the same questions later is exactly the wasted trip this
// checkmark exists to avoid — calls onCompleted (see markQuizCompleted) so
// the room list can show the same "studied this" checkmark a session with
// nothing left to quiz gets via "내가 읽었음", and so EndConversationControl
// hides "퀴즈 풀기" in favor of "퀴즈 다시 만들기" from then on.
function QuizPanel({
  sessionId,
  questions,
  progress,
  setProgress,
  onBack,
  onCompleted,
}: {
  sessionId: string;
  questions: QuizQuestion[];
  progress: QuizProgress;
  setProgress: React.Dispatch<React.SetStateAction<QuizProgress>>;
  onBack: () => void;
  onCompleted: () => void;
}) {
  const { index, answer, checked, checking, correct, correctCount } = progress;
  const question = questions[index];

  // finalize records one question's outcome once it's fully settled — either
  // immediately (an exact/listed-synonym match) or after checkQuizAnswer's
  // LLM fallback resolves — so check()'s two paths share the exact same
  // "advance score, complete the quiz if this was the last question" logic
  // instead of duplicating it. Reaching the last question at all marks the
  // quiz completed, regardless of whether this particular answer was right —
  // the checkmark means "studied this", not "aced this".
  const finalize = useCallback(
    (isAnswerCorrect: boolean) => {
      setProgress((p) => ({
        ...p,
        checked: true,
        checking: false,
        correct: isAnswerCorrect,
        correctCount: p.correctCount + (isAnswerCorrect ? 1 : 0),
      }));
      if (index + 1 === questions.length) {
        void markQuizCompleted(sessionId).then((ok) => {
          if (ok) onCompleted();
        });
      }
    },
    [index, questions.length, sessionId, onCompleted, setProgress],
  );

  const check = useCallback(() => {
    if (!question || checked || checking || !answer.trim()) return;
    if (isQuizAnswerAccepted(question, answer)) {
      finalize(true);
      return;
    }
    // Didn't match answer/acceptableAnswers literally — ask a fast chat-model
    // call whether it's still a valid synonym the quiz's own generation step
    // didn't think to list (see checkQuizAnswer's doc comment on why this is
    // biased toward "no" server-side, so a hallucinated false positive can't
    // teach the learner something wrong).
    setProgress((p) => ({ ...p, checking: true }));
    void checkQuizAnswer(question.prompt, question.answer, question.acceptableAnswers, answer).then((verdict) => {
      finalize(verdict);
    });
  }, [question, checked, checking, answer, finalize, setProgress]);

  const next = useCallback(() => {
    setProgress((p) => ({ ...p, index: p.index + 1, answer: "", checked: false, checking: false, correct: false }));
  }, [setProgress]);

  if (!question) return null;

  return (
    <div className="quiz-panel">
      <button type="button" className="ghost quiz-back-btn" onClick={onBack}>
        ← 요약으로
      </button>
      <div className="quiz-question">
        <div className="quiz-progress">
          {index + 1} / {questions.length}
        </div>
        {/* The blank is typed directly in place inside the sentence rather
            than in a separate box below it -- generation guarantees exactly
            one "___" per prompt (see pipeline_study.go's quizSystemPrompt),
            so a plain two-way split is enough here (see WordReview.tsx's
            computeBlank for the multi-blank case that needs more). */}
        <div className="quiz-prompt quiz-blank-sentence">
          {(() => {
            const [before, after] = question.prompt.split("___");
            return (
              <>
                <span>{before}</span>
                <input
                  type="text"
                  className={quizBlankInputClass(checked, correct)}
                  style={{ width: `${Math.min(16, Math.max(3, answer.length + 1))}ch` }}
                  value={answer}
                  onChange={(e) => setProgress((p) => ({ ...p, answer: e.target.value }))}
                  onKeyDown={(e) => {
                    if (e.key !== "Enter") return;
                    if (checked) next();
                    else check();
                  }}
                  disabled={checked || checking}
                  aria-label="정답 입력"
                />
                <span>{after}</span>
              </>
            );
          })()}
        </div>
        {question.answerMeaning && <div className="quiz-meaning-hint">💡 {question.answerMeaning}</div>}
        {!checked && (
          <button type="button" className="quiz-check-btn" onClick={check} disabled={!answer.trim() || checking}>
            {checking ? "확인하는 중…" : "확인"}
          </button>
        )}
        {checked && (
          <>
            <div className={`quiz-result ${correct ? "correct" : "incorrect"}`} role="status">
              {correct ? "정답이에요!" : `아쉬워요. 정답: ${question.answer}`}
            </div>
            <p className="study-summary-sentence">
              <span className="study-summary-en">{question.explanation}</span>
              <span className="study-summary-ko">{question.explanationTranslation}</span>
            </p>
            <div className="study-summary-ko quiz-translation">{question.translation}</div>
            {index + 1 < questions.length ? (
              <button type="button" className="quiz-next-btn" onClick={next}>
                다음 문제
              </button>
            ) : (
              <div className="quiz-score" role="status">
                {questions.length}문제 중 {correctCount}개 맞혔어요!
              </div>
            )}
          </>
        )}
      </div>
    </div>
  );
}
