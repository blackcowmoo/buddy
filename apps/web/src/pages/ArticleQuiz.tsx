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

// How often to re-check a draw that's still generating in the background
// (see asyncjob.KindArticleStudy) — a poll, not a push, since nothing on the
// server tells an already-open client "it's ready now" (same reasoning as
// App.tsx's pollStudySummary).
const articleStudyPollIntervalMs = 3000;

type LoadState = "loading" | "ready" | "error";

// A single draw walks through these in order: "reading" (English summary,
// TTS read-aloud) -> "quiz" (native-language 4-choice comprehension check)
// -> "result" (reveal). null means the list view — past attempts, and the
// button to draw a new one.
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
  const [selected, setSelected] = useState<number | null>(null);
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
      setSelected(null);
      setResult(null);
      setView("reading");
      setDrawState("idle");
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
      setSelected(null);
      setResult(null);
      setDrawState("idle");
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

  const startQuiz = useCallback(() => setView("quiz"), []);

  const chooseAnswer = useCallback(
    async (index: number) => {
      if (!draw || selected !== null) return;
      setSelected(index);
      const res = await answerArticle(draw.id, index);
      if (res) {
        setResult(res);
        setView("result");
      }
    },
    [draw, selected],
  );

  const backToList = useCallback(() => {
    pollTokenRef.current = null; // stop watching; generation itself keeps going server-side
    setView(null);
    setDraw(null);
    setResult(null);
    setSelected(null);
    setDrawState("idle");
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
                {tts === "speaking" && (
                  // Read-aloud deliberately requests the "ambient" audio
                  // session type (see handleRead) so it mixes with — never
                  // pauses — music already playing in another app, which
                  // means it inherits the same rule as any other ambient
                  // sound: silenced while the hardware ring/silent switch is
                  // on. Called out here so that reads as expected, not as
                  // broken playback.
                  <p className="hint">무음 스위치가 켜져 있으면 읽어주기 소리가 나지 않아요.</p>
                )}
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
            <div className="quiz-prompt">이 문단의 내용과 일치하는 해석을 고르세요.</div>
            <div className="quiz-choices">
              {draw.choices.map((choice, i) => (
                <button
                  key={i}
                  type="button"
                  className="quiz-choice-btn"
                  onClick={() => void chooseAnswer(i)}
                  disabled={selected !== null}
                >
                  {choice}
                </button>
              ))}
            </div>
          </div>
        )}

        {view === "result" && draw && result && (
          <div className="quiz-panel">
            <div className={`quiz-result ${result.correct ? "correct" : "incorrect"}`} role="status">
              {result.correct ? "정답이에요!" : "아쉬워요, 오답이에요."}
            </div>
            <p className="article-summary">{draw.summary}</p>
            <div className="quiz-choices">
              {draw.choices.map((choice, i) => {
                const isAnswer = i === result.correctIndex;
                const isSelected = i === selected;
                const cls = quizChoiceClass(true, isSelected, isAnswer);
                return (
                  <button key={i} type="button" className={cls} disabled>
                    {choice}
                  </button>
                );
              })}
            </div>
            <div className="article-explanation">{result.explanation}</div>
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
