import { Fragment, useCallback, useEffect, useRef, useState } from "react";
import { confirmThenDelete } from "../lib/confirmDelete";
import {
  answerArticle,
  articleAudioURL,
  deleteArticleInstance,
  drawArticle,
  fetchArticleInstance,
  fetchArticleInstances,
  type ArticleAnswerResult,
  type ArticleDraw,
  type ArticleInstance,
} from "../lib/articles";
import { formatAbsoluteDate, formatDateDivider, formatMessageTime, shouldShowDateDivider } from "../lib/time";
import { quizChoiceClass } from "../lib/quizCheck";
import { SubPageHeader } from "../components/SubPageHeader";
import { usePollScaffold } from "../hooks/usePollScaffold";
import { requestAmbientAudioSession } from "../lib/audioSession";
import { loadPlaybackRate } from "../lib/ttsSettings";

// How often to re-check a draw that's still generating in the background
// (see asyncjob.KindArticleStudy) — a poll, not a push, since nothing on the
// server tells an already-open client "it's ready now" (same reasoning as
// App.tsx's pollStudySummary).
const articleStudyPollIntervalMs = 3000;

type LoadState = "loading" | "ready" | "error";

// A single draw walks through these in order: "reading" (English summary,
// TTS read-aloud) -> "quiz" (a series of independent native-language
// 2-choice fact checks, see pipeline.articleStudySystemPrompt) -> "result"
// (reveal). null means the list view — past attempts, and the button to
// draw a new one.
type View = "reading" | "quiz" | "result" | null;

type DrawState = "idle" | "drawing" | "noMore" | "error";
type TtsState = "idle" | "loading" | "speaking" | "error";

// "오늘의 아티클": draws a news article the learner hasn't seen before (see
// lib/articles.ts's drawArticle, which excludes every article already drawn
// — no daily limit, only repeats are excluded), shows an English study
// paragraph to read (with an optional read-aloud for listening practice —
// audio generated and cached server-side once per shared article, see
// lib/articles.ts's articleAudioURL, unlike App.tsx's per-message chat
// read-aloud, which is still generated client-side since each reply is
// unique to that conversation), then a native-language multiple-choice
// comprehension check. Past attempts live in their own list here, the same
// "instant, unlimited, own list" shape as InstantSessions.tsx.
export function ArticleQuiz() {
  const [state, setState] = useState<LoadState>("loading");
  const [instances, setInstances] = useState<ArticleInstance[]>([]);
  const [view, setView] = useState<View>(null);
  const [drawState, setDrawState] = useState<DrawState>("idle");
  const [draw, setDraw] = useState<ArticleDraw | null>(null);
  // One entry per sub-question, in order; null means "not yet picked".
  const [selections, setSelections] = useState<(number | null)[]>([]);
  const [submitting, setSubmitting] = useState(false);
  const [result, setResult] = useState<ArticleAnswerResult | null>(null);
  const [tts, setTts] = useState<TtsState>("idle");
  const audioRef = useRef<HTMLAudioElement | null>(null);

  // Poll scaffolding for a draw still generating in the background (see
  // usePollScaffold's doc comment). Losing this component (navigating away,
  // or the tab closing) only stops *watching* — asyncjob.KindArticleStudy
  // keeps generating regardless (see lib/articles.ts's drawArticle doc
  // comment); reopening this page and tapping the still-pending row resumes
  // watching.
  const { tokenRef: pollTokenRef, schedulePoll } = usePollScaffold();

  // Polls one draw's status until it leaves "pending"/"failed" — started
  // right after a fresh draw, or when reopening a still-generating row from
  // the list (see openInstance). "failed" keeps polling rather than giving
  // up: the asyncjob reaper retries the job from scratch on its own (see
  // asyncjob.Queue.Execute), so a later attempt can still land.
  const pollDraw = useCallback(
    (id: string, token: object) => {
      const tick = async () => {
        if (pollTokenRef.current !== token) return; // left this draw, or started another
        const updated = await fetchArticleInstance(id);
        if (pollTokenRef.current !== token) return;
        if (!updated) {
          schedulePoll(tick, articleStudyPollIntervalMs); // transient fetch failure — keep trying
          return;
        }
        setDraw(updated);
        if (updated.status !== "done") schedulePoll(tick, articleStudyPollIntervalMs);
      };
      schedulePoll(tick, articleStudyPollIntervalMs);
    },
    [schedulePoll],
  );

  const loadInstances = useCallback(() => {
    fetchArticleInstances().then((list) => {
      setInstances(list);
      setState("ready");
    });
  }, []);

  useEffect(() => {
    loadInstances();
  }, [loadInstances]);

  const handleDraw = useCallback(async () => {
    setDrawState("drawing");
    const res = await drawArticle();
    if (res.status === "ok") {
      setDraw(res.draw);
      setSelections([]);
      setResult(null);
      setView("reading");
      setDrawState("idle");
      setTts("idle");
      if (res.draw.status !== "done") {
        const token = {};
        pollTokenRef.current = token;
        pollDraw(res.draw.id, token);
      }
    } else {
      setDrawState(res.status);
    }
  }, [pollDraw]);

  // Reopens one of the caller's own draws from the list, whether it's done
  // (read the past summary/quiz result) or still generating (resume
  // watching it finish instead of it looking abandoned).
  const openInstance = useCallback(
    async (id: string) => {
      const found = await fetchArticleInstance(id);
      if (!found) return;
      setDraw(found);
      setSelections([]);
      setResult(null);
      setDrawState("idle");
      setTts("idle");
      setView("reading");
      if (found.status !== "done") {
        const token = {};
        pollTokenRef.current = token;
        pollDraw(found.id, token);
      }
    },
    [pollDraw],
  );

  // Plays the English summary's read-aloud audio — generated and cached
  // server-side once per shared article (see lib/articles.ts's
  // articleAudioURL), so this is just pointing a plain <audio> element at
  // it, the same "src + play(), let the browser handle buffering" shape as
  // Recordings.tsx's playback. "loading"/"speaking" are driven by the
  // element's own buffering/playing events (below) rather than tracked by
  // hand, so the label never claims audio is playing before it actually is.
  const handleRead = useCallback(() => {
    const el = audioRef.current;
    if (!el || !draw) return;
    // Must run synchronously in this click, before play() — see
    // requestAmbientAudioSession's doc comment.
    requestAmbientAudioSession();
    setTts("loading");
    el.playbackRate = loadPlaybackRate();
    el.src = articleAudioURL(draw.id);
    el.play().catch((err) => {
      console.error("tts:", err);
      // Show the failure briefly instead of silently reverting to the idle
      // "🔊 읽어주기" label, which reads as if nothing was ever pressed even
      // though playback genuinely failed.
      setTts("error");
      setTimeout(() => setTts("idle"), 2000);
    });
  }, [draw]);

  const startQuiz = useCallback(() => {
    setSelections((prev) => (draw ? draw.subQuestions.map(() => null) : prev));
    setView("quiz");
  }, [draw]);

  // Picks/changes the learner's answer for one sub-question — doesn't submit
  // on its own (unlike the old single 4-choice question, several picks are
  // needed before there's anything to score), so a pick can still be
  // changed before submitAnswers is tapped.
  const pickOption = useCallback((subIndex: number, optionIndex: number) => {
    setSelections((prev) => {
      const next = [...prev];
      next[subIndex] = optionIndex;
      return next;
    });
  }, []);

  const allAnswered = selections.length > 0 && selections.every((s) => s !== null);

  const submitAnswers = useCallback(async () => {
    if (!draw || !allAnswered || submitting) return;
    setSubmitting(true);
    const res = await answerArticle(draw.id, selections as number[]);
    setSubmitting(false);
    if (res) {
      setResult(res);
      setView("result");
    }
  }, [draw, selections, allAnswered, submitting]);

  const backToList = useCallback(() => {
    pollTokenRef.current = null; // stop watching; generation itself keeps going server-side
    setView(null);
    setDraw(null);
    setResult(null);
    setSelections([]);
    setDrawState("idle");
    // The reading view's <audio> element unmounts with it (view leaves
    // "reading"), which does stop playback — but that's a DOM-level effect
    // its own onEnded/onError event never fires for, so without this the
    // "재생 중…"/"불러오는 중…" label would otherwise survive stale into
    // whatever's opened next.
    setTts("idle");
    loadInstances();
  }, [loadInstances]);

  const handleDelete = (id: string) =>
    confirmThenDelete("이 아티클 퀴즈를 삭제할까요?", deleteArticleInstance, id, setInstances);

  return (
    <div className="app">
      <SubPageHeader title="오늘의 아티클" />

      <main className="convo article-quiz-page">
        {drawState === "noMore" && (
          <p className="hint">지금은 새로 볼 아티클이 없어요. 나중에 다시 시도해보세요.</p>
        )}
        {drawState === "error" && (
          <p className="hint">아티클을 가져오지 못했습니다. 네트워크 문제일 수 있습니다.</p>
        )}

        {view === null && (
          <>
            <button
              type="button"
              className="quiz-start-btn"
              onClick={() => void handleDraw()}
              disabled={drawState === "drawing"}
            >
              {drawState === "drawing" ? "가져오는 중…" : "새 아티클 뽑기"}
            </button>

            {state === "loading" && <p className="hint">불러오는 중…</p>}
            {state === "ready" && instances.length === 0 && (
              <p className="hint">아직 읽은 아티클이 없어요.</p>
            )}
            {instances.map((inst, i) => {
              const prev = instances[i - 1];
              const showDivider = shouldShowDateDivider(prev?.createdAt, inst.createdAt);
              return (
                <Fragment key={inst.id}>
                  {showDivider && (
                    <div className="date-divider">
                      <span>{formatDateDivider(inst.createdAt)}</span>
                    </div>
                  )}
                  <div className="session-row article-instance-row">
                    <div
                      className="session-item article-instance-item"
                      onClick={() => void openInstance(inst.id)}
                    >
                      {inst.status !== "done" && (
                        <span className="study-summary-pending-badge" title="아티클을 만드는 중">
                          <span className="spinning">⏳</span> 생성 중
                        </span>
                      )}
                      <span className="title">
                        [{inst.source}] {inst.title}
                      </span>
                      <span className="time">
                        {formatMessageTime(inst.createdAt)}
                        {inst.answered && (inst.correct ? " · 정답" : " · 오답")}
                      </span>
                    </div>
                    <button
                      type="button"
                      className="ghost icon-btn session-delete"
                      onClick={() => void handleDelete(inst.id)}
                      aria-label="아티클 퀴즈 삭제"
                      title="아티클 퀴즈 삭제"
                    >
                      🗑
                    </button>
                  </div>
                </Fragment>
              );
            })}
          </>
        )}

        {view === "reading" && draw && (
          <div className="quiz-panel article-reading-panel">
            <div className="article-meta">
              [{draw.source}] {draw.title}
            </div>
            {draw.publishedAt > 0 && (
              <div className="article-date">{formatAbsoluteDate(new Date(draw.publishedAt * 1000))}</div>
            )}
            {draw.status === "done" ? (
              <>
                <p className="article-summary">{draw.summary}</p>
                <audio
                  ref={audioRef}
                  style={{ display: "none" }}
                  onPlaying={() => setTts("speaking")}
                  onWaiting={() => setTts("loading")}
                  onEnded={() => setTts("idle")}
                  onError={() => {
                    setTts("error");
                    setTimeout(() => setTts("idle"), 2000);
                  }}
                />
                <button
                  type="button"
                  className="ghost article-read-aloud-btn"
                  onClick={handleRead}
                  disabled={tts !== "idle"}
                >
                  {tts === "loading"
                    ? "불러오는 중…"
                    : tts === "speaking"
                      ? "재생 중…"
                      : tts === "error"
                        ? "재생 실패, 다시 시도해주세요"
                        : "🔊 읽어주기"}
                </button>
                <button type="button" className="quiz-start-btn" onClick={startQuiz}>
                  문제풀기
                </button>
              </>
            ) : (
              // Still generating (see asyncjob.KindArticleStudy) — this view
              // polls in the background (see pollDraw) whether the learner
              // just drew this or reopened a pending row from the list; no
              // action needed here beyond waiting or leaving.
              <p className="hint">
                <span className="spinning">⏳</span> 아티클을 요약하고 문제를 만드는 중이에요. 이 화면을 나갔다 와도
                계속 진행돼요.
              </p>
            )}
            <button type="button" className="ghost quiz-back-btn" onClick={backToList}>
              ← 목록으로
            </button>
          </div>
        )}

        {view === "quiz" && draw && (
          <div className="quiz-panel">
            <p className="article-summary">{draw.summary}</p>
            <div className="quiz-prompt">이 문단의 내용과 일치하는 것을 각각 고르세요.</div>
            {draw.subQuestions.map((sub, qi) => (
              <div key={qi} className="article-sub-question">
                <div className="quiz-prompt">{sub.prompt}</div>
                <div className="quiz-choices">
                  {sub.options.map((option, oi) => (
                    <button
                      key={oi}
                      type="button"
                      className={
                        selections[qi] === oi ? "quiz-choice-btn selected" : "quiz-choice-btn"
                      }
                      onClick={() => pickOption(qi, oi)}
                    >
                      {option}
                    </button>
                  ))}
                </div>
              </div>
            ))}
            <button
              type="button"
              className="quiz-start-btn"
              onClick={() => void submitAnswers()}
              disabled={!allAnswered || submitting}
            >
              {submitting ? "채점 중…" : "제출하기"}
            </button>
          </div>
        )}

        {view === "result" && draw && result && (
          <div className="quiz-panel">
            <div className={`quiz-result ${result.correct ? "correct" : "incorrect"}`} role="status">
              {result.correct ? "정답이에요!" : `아쉬워요, ${result.score}/${result.total} 정답이에요.`}
            </div>
            <p className="article-summary">{draw.summary}</p>
            {result.subQuestions.map((sub, qi) => (
              <div key={qi} className="article-sub-question">
                <div className="quiz-prompt">{sub.prompt}</div>
                <div className="quiz-choices">
                  {sub.options.map((option, oi) => {
                    const isAnswer = oi === sub.correctOptionIndex;
                    const isSelected = oi === sub.selectedOptionIndex;
                    const cls = quizChoiceClass(true, isSelected, isAnswer);
                    return (
                      <button key={oi} type="button" className={cls} disabled>
                        {option}
                      </button>
                    );
                  })}
                </div>
                <div className="article-explanation">{sub.explanation}</div>
              </div>
            ))}
            <button
              type="button"
              className="quiz-start-btn"
              onClick={() => void handleDraw()}
              disabled={drawState === "drawing"}
            >
              {drawState === "drawing" ? "가져오는 중…" : "다른 아티클 뽑기"}
            </button>
            <button type="button" className="ghost quiz-back-btn" onClick={backToList}>
              ← 목록으로
            </button>
          </div>
        )}
      </main>
    </div>
  );
}
