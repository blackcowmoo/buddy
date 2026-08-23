import { Fragment, useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
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
import { checkDefinedWord, defineWord } from "../lib/wordSearch";
import { saveWord } from "../lib/wordReview";
import type { WordSuggestion } from "../lib/protocol";
import { useDismiss } from "../hooks/useDismiss";
import { LoadingHint } from "../components/LoadingHint";
import type { LoadState } from "../lib/loadState";
import { readStored, writeStored } from "../lib/storedValue";

// How often to re-check a draw that's still generating in the background
// (see asyncjob.KindArticleStudy) — a poll, not a push, since nothing on the
// server tells an already-open client "it's ready now" (same reasoning as
// App.tsx's pollStudySummary).
const articleStudyPollIntervalMs = 3000;
const articleListPageSize = 20;
const articleListLoadThreshold = 80;

// Keep the lookup directly below the tapped word while it fits. If the
// anchor is too close to either viewport edge, the panel must leave the
// paragraph's positioning context and use the whole window instead.
export function shouldCenterWordLookup(anchorLeft: number, panelWidth: number, viewportWidth: number, margin = 12) {
  return anchorLeft < margin || anchorLeft + panelWidth > viewportWidth - margin;
}


// A single draw walks through these in order: "reading" (English summary,
// TTS read-aloud) -> "quiz" (a series of independent native-language
// 2-choice fact checks, see pipeline.articleStudySystemPrompt) -> "result"
// (reveal). null means the list view — past attempts, and the button to
// draw a new one.
type View = "reading" | "quiz" | "result" | null;

type DrawState = "idle" | "drawing" | "noMore" | "error";
type TtsState = "idle" | "loading" | "speaking" | "error";

type SearchedWord = {
  key: number;
  word: string;
  result: WordSuggestion | null;
  loading: boolean;
  saving: boolean;
  saved: boolean;
};

function searchedWordsStorageKey(articleID: string) {
  return `buddy.article.searched-words.${articleID}`;
}

function loadSearchedWords(articleID: string): SearchedWord[] {
  return readStored(searchedWordsStorageKey(articleID), (raw) => {
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return undefined;
    return parsed.filter((item): item is SearchedWord => {
      if (!item || typeof item !== "object") return false;
      const value = item as Partial<SearchedWord>;
      return typeof value.key === "number" && typeof value.word === "string" && (value.result === null || typeof value.result === "object");
    }).map((item) => ({ ...item, loading: false, saving: false, saved: false }));
  }, []);
}

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
  const [visibleInstanceCount, setVisibleInstanceCount] = useState(articleListPageSize);
  const [view, setView] = useState<View>(null);
  const [drawState, setDrawState] = useState<DrawState>("idle");
  const [draw, setDraw] = useState<ArticleDraw | null>(null);
  // One entry per sub-question, in order; null means "not yet picked".
  const [selections, setSelections] = useState<(number | null)[]>([]);
  const [submitting, setSubmitting] = useState(false);
  const [result, setResult] = useState<ArticleAnswerResult | null>(null);
  const [tts, setTts] = useState<TtsState>("idle");
  const articlePageRef = useRef<HTMLElement | null>(null);
  const pendingScrollCorrectionRef = useRef<{ height: number; top: number } | null>(null);
  const audioRef = useRef<HTMLAudioElement | null>(null);
  // Invalidates a pending play() rejection when the learner cancels before
  // the browser has finished starting playback.
  const ttsAttemptRef = useRef(0);

  // A learner tapping a word inside the reading paragraph can look it up and,
  // if it's new to them, add it to their vocabulary study list — the same
  // save target as WordSearchControl's "학습하기", just reached from the
  // exact word already in front of them instead of a typed-out Korean
  // description. `key` is the tapped token's index within draw.summary's
  // split (not the word text alone), since the same word can appear more
  // than once in a paragraph and each tap should look up/save independently.
  // null means no popover is open.
  const [wordLookup, setWordLookup] = useState<{
    key: number;
    word: string;
    loading: boolean;
    failed: boolean;
    result: WordSuggestion | null;
    saving: boolean;
    saved: boolean;
  } | null>(null);
  const wordLookupRef = useRef<HTMLDivElement>(null);
  const wordLookupAnchorRef = useRef<HTMLSpanElement>(null);
  const [centerWordLookup, setCenterWordLookup] = useState(false);
  const [searchedWords, setSearchedWords] = useState<SearchedWord[]>([]);
  const [searchedWordsOpen, setSearchedWordsOpen] = useState(false);
  // Successful lookups are kept for this page session so reopening a word
  // doesn't make the learner confirm (or request) the same lookup again.
  // Include the token position because the server resolves words in context,
  // and the same spelling can have different meanings in different places.
  const wordLookupCacheRef = useRef(new Map<string, WordSuggestion>());
  // Keep lookup attempts alive while the learner moves between words. A
  // pending lookup must still look like "찾는 중…" when its word is opened
  // again, and a failed attempt must not regress to the initial "찾기" action.
  const pendingWordLookupsRef = useRef(new Map<string, Promise<WordSuggestion | null>>());
  const failedWordLookupsRef = useRef(new Set<string>());
  useDismiss(wordLookup !== null, wordLookupRef, () => setWordLookup(null));

  const updateSearchedWord = useCallback((key: number, word: string, update: Partial<SearchedWord>) => {
    setSearchedWords((prev) => {
      const existing = prev.find((item) => item.key === key);
      const next = existing
        ? prev.map((item) => (item.key === key ? { ...item, ...update } : item))
        : [...prev, { key, word, result: null, loading: false, saving: false, saved: false, ...update }];
      if (draw) {
        writeStored(searchedWordsStorageKey(draw.id), JSON.stringify(next.map(({ key: itemKey, word: itemWord, result }) => ({ key: itemKey, word: itemWord, result }))));
      }
      return next;
    });
  }, [draw]);

  // Measure after the panel has been laid out. This keeps the usual
  // word-adjacent placement, but switches to a viewport-centered panel when
  // an edge word would push it outside the screen.
  useLayoutEffect(() => {
    if (!wordLookup) return;
    const updatePlacement = () => {
      const panel = wordLookupRef.current;
      const anchor = wordLookupAnchorRef.current;
      if (!panel || !anchor) return;
      const panelRect = panel.getBoundingClientRect();
      const anchorRect = anchor.getBoundingClientRect();
      const viewportWidth = window.innerWidth || document.documentElement.clientWidth;
      setCenterWordLookup(shouldCenterWordLookup(anchorRect.left, panelRect.width, viewportWidth));
    };
    updatePlacement();
    window.addEventListener("resize", updatePlacement);
    return () => window.removeEventListener("resize", updatePlacement);
  }, [wordLookup]);

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
        if (updated.status !== "done" || !updated.translation) schedulePoll(tick, articleStudyPollIntervalMs);
      };
      schedulePoll(tick, articleStudyPollIntervalMs);
    },
    [schedulePoll],
  );

  const loadInstances = useCallback(() => {
    fetchArticleInstances().then((list) => {
      // The API returns newest first, but this page grows downward: the
      // newest attempt belongs at the bottom, next to the draw action.
      setInstances(list.slice().sort((a, b) => a.createdAt - b.createdAt));
      setVisibleInstanceCount(articleListPageSize);
      setState("ready");
    });
  }, []);

  useEffect(() => {
    loadInstances();
  }, [loadInstances]);

  // Keep the newest article and the action for drawing another one in view
  // when entering the list. The list itself remains a normal top-to-bottom
  // scroll container so the learner can scroll upward into older articles.
  useLayoutEffect(() => {
    if (view === null && state === "ready") {
      const page = articlePageRef.current;
      if (page) page.scrollTop = page.scrollHeight;
    }
  }, [instances.length, state, view]);

  // Older attempts are prepended in batches while the learner scrolls upward.
  // Compensate for the added content so the row they were looking at stays in
  // the same place instead of jumping down by one whole batch.
  useLayoutEffect(() => {
    const correction = pendingScrollCorrectionRef.current;
    const page = articlePageRef.current;
    if (!correction || !page) return;
    page.scrollTop = correction.top + (page.scrollHeight - correction.height);
    pendingScrollCorrectionRef.current = null;
  }, [visibleInstanceCount]);

  const loadOlderInstances = useCallback(() => {
    if (visibleInstanceCount >= instances.length) return;
    const page = articlePageRef.current;
    if (page) pendingScrollCorrectionRef.current = { height: page.scrollHeight, top: page.scrollTop };
    setVisibleInstanceCount((count) => Math.min(count + articleListPageSize, instances.length));
  }, [instances.length, visibleInstanceCount]);

  const visibleInstances = instances.slice(Math.max(0, instances.length - visibleInstanceCount));

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
      setWordLookup(null);
      setSearchedWords(loadSearchedWords(res.draw.id));
      setSearchedWordsOpen(false);
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
      setWordLookup(null);
      setSearchedWords(loadSearchedWords(found.id));
      setSearchedWordsOpen(false);
      setView("reading");
      if (found.status !== "done" || !found.translation) {
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
    const attempt = ++ttsAttemptRef.current;
    // Must run synchronously in this click, before play() — see
    // requestAmbientAudioSession's doc comment.
    requestAmbientAudioSession();
    setTts("loading");
    el.playbackRate = loadPlaybackRate();
    el.src = articleAudioURL(draw.id);
    el.play().catch((err) => {
      if (ttsAttemptRef.current !== attempt) return;
      console.error("tts:", err);
      // Show the failure briefly instead of silently reverting to the idle
      // "🔊 읽어주기" label, which reads as if nothing was ever pressed even
      // though playback genuinely failed.
      setTts("error");
      setTimeout(() => setTts("idle"), 2000);
    });
  }, [draw]);

  // Stops both an audible read and one that is still buffering. Resetting the
  // position means the next "읽어주기" starts from the beginning.
  const cancelRead = useCallback(() => {
    ttsAttemptRef.current += 1;
    const el = audioRef.current;
    if (el) {
      el.pause();
      el.currentTime = 0;
    }
    setTts("idle");
  }, []);

  // Selects one word tapped inside the reading paragraph (see the word-token
  // buttons in the reading view below). The server cache is checked first;
  // only a cache miss leaves the learner a second action to start a lookup.
  const openWordLookup = useCallback(
    (key: number, word: string) => {
      if (!draw) return;
      setCenterWordLookup(false);
      const lookupKey = `${draw.id}:${key}`;
      const localResult = wordLookupCacheRef.current.get(lookupKey) ?? searchedWords.find((item) => item.key === key)?.result ?? null;
      const pending = pendingWordLookupsRef.current.has(lookupKey);
      setWordLookup({
        key,
        word,
        loading: pending || (!localResult && !failedWordLookupsRef.current.has(lookupKey)),
        failed: !pending && !localResult && failedWordLookupsRef.current.has(lookupKey),
        result: localResult ?? null,
        saving: false,
        saved: false,
      });

      if (localResult || pending || failedWordLookupsRef.current.has(lookupKey)) return;
      void checkDefinedWord(draw.id, word, key).then((serverResult) => {
        if (serverResult) wordLookupCacheRef.current.set(lookupKey, serverResult);
        setWordLookup((prev) =>
          prev && prev.key === key
            ? { ...prev, loading: false, result: serverResult, failed: false }
            : prev,
        );
      });
    },
    [draw, searchedWords, updateSearchedWord],
  );

  // The whole study paragraph is short (one paragraph), so it's sent as
  // context every time rather than trying to isolate just the containing
  // sentence.
  const requestWordLookup = useCallback(() => {
    if (!draw || !wordLookup || wordLookup.loading) return;
    const { key, word } = wordLookup;
    const lookupKey = `${draw.id}:${key}`;
    const pending = pendingWordLookupsRef.current.get(lookupKey);
    if (pending) {
      setWordLookup((prev) => (prev && prev.key === key ? { ...prev, loading: true, failed: false } : prev));
      return;
    }
    setWordLookup((prev) => (prev && prev.key === key ? { ...prev, loading: true } : prev));
    updateSearchedWord(key, word, { loading: true });
    const lookup = defineWord(draw.id, word, key);
    pendingWordLookupsRef.current.set(lookupKey, lookup);
    void lookup.then((result) => {
      pendingWordLookupsRef.current.delete(lookupKey);
      if (result) {
        wordLookupCacheRef.current.set(lookupKey, result);
        failedWordLookupsRef.current.delete(lookupKey);
      } else {
        failedWordLookupsRef.current.add(lookupKey);
      }
      updateSearchedWord(key, word, { loading: false, result });
      setWordLookup((prev) =>
        prev && prev.key === key ? { ...prev, loading: false, failed: result === null, result } : prev,
      );
    });
  }, [draw, updateSearchedWord, wordLookup]);

  // Saves the currently open word-lookup popover's result to the learner's
  // vocabulary study list — same saveWord() call and pending-until-verified
  // lifecycle as WordSearchControl's "학습하기" button.
  const learnLookedUpWord = useCallback(() => {
    if (!wordLookup || !wordLookup.result || wordLookup.saving || wordLookup.saved) return;
    const key = wordLookup.key;
    const suggestion = wordLookup.result;
    setWordLookup((prev) => (prev && prev.key === key ? { ...prev, saving: true } : prev));
    void saveWord(suggestion, wordLookup.word).then((saved) => {
      updateSearchedWord(key, wordLookup.word, { saving: false, saved: !!saved, result: suggestion });
      setWordLookup((prev) => (prev && prev.key === key ? { ...prev, saving: false, saved: !!saved } : prev));
    });
  }, [updateSearchedWord, wordLookup]);

  const learnSearchedWord = useCallback((item: SearchedWord) => {
    if (!item.result || item.saving || item.saved) return;
    updateSearchedWord(item.key, item.word, { saving: true });
    void saveWord(item.result, item.word).then((saved) => {
      updateSearchedWord(item.key, item.word, { saving: false, saved: !!saved });
    });
  }, [updateSearchedWord]);

  const startQuiz = useCallback(() => {
    setSelections((prev) => (draw ? draw.subQuestions.map(() => null) : prev));
    setWordLookup(null);
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
    setWordLookup(null);
    setSearchedWordsOpen(false);
    loadInstances();
  }, [loadInstances]);

  const handleDelete = (id: string) =>
    confirmThenDelete("이 아티클 퀴즈를 삭제할까요?", deleteArticleInstance, id, setInstances);

  return (
    <div className="app">
      <SubPageHeader title="오늘의 아티클" />

      <main
        ref={articlePageRef}
        className="convo article-quiz-page"
        onScroll={(event) => {
          if (event.currentTarget.scrollTop <= articleListLoadThreshold) loadOlderInstances();
        }}
      >
        {drawState === "noMore" && (
          <p className="hint">지금은 새로 볼 아티클이 없어요. 나중에 다시 시도해보세요.</p>
        )}
        {drawState === "error" && (
          <p className="hint">아티클을 가져오지 못했습니다. 네트워크 문제일 수 있습니다.</p>
        )}

        {view === null && (
          <>
            {state === "loading" && <LoadingHint />}
            {state === "ready" && instances.length === 0 && (
              <p className="hint">아직 읽은 아티클이 없어요.</p>
            )}
            {visibleInstances.length < instances.length && (
              <p className="hint article-list-load-hint">위로 스크롤하면 이전 아티클을 더 불러와요.</p>
            )}
            {visibleInstances.map((inst, i) => {
              const instanceIndex = instances.length - visibleInstances.length + i;
              const prev = instances[instanceIndex - 1];
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

            <button
              type="button"
              className="quiz-start-btn"
              onClick={() => void handleDraw()}
              disabled={drawState === "drawing"}
            >
              {drawState === "drawing" ? "가져오는 중…" : "새 아티클 뽑기"}
            </button>
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
                <p className="article-summary">
                  {draw.summary.split(/([A-Za-z']+)/g).map((part, i) =>
                    /^[A-Za-z']+$/.test(part) ? (
                      <span className="article-word-anchor" key={i} ref={wordLookup?.key === i ? wordLookupAnchorRef : undefined}>
                        <button
                          type="button"
                          className="article-word"
                          onClick={() => openWordLookup(i, part)}
                        >
                          {part}
                        </button>
                        {wordLookup?.key === i && (
                          <div
                            className={`word-lookup-panel${centerWordLookup ? " word-lookup-panel-centered" : ""}`}
                            role="menu"
                            ref={wordLookupRef}
                          >
                            <div className="word-lookup-header">
                              <span className="word-search-word">{wordLookup.word}</span>
                              <button
                                type="button"
                                className="ghost icon-btn"
                                onClick={() => setWordLookup(null)}
                                aria-label="단어 뜻 닫기"
                              >
                                ✕
                              </button>
                            </div>
                            {wordLookup.loading && <div className="word-search-status">찾는 중…</div>}
                            {!wordLookup.loading && !wordLookup.result && !wordLookup.failed && (
                              <button type="button" className="word-learn-btn" onClick={requestWordLookup}>
                                찾기
                              </button>
                            )}
                            {!wordLookup.loading && wordLookup.failed && (
                              <div className="word-search-status">뜻을 가져오지 못했어요.</div>
                            )}
                            {!wordLookup.loading && !wordLookup.failed && wordLookup.result && (
                              <>
                                <span className="word-search-meaning">{wordLookup.result.meaning}</span>
                                <span className="word-search-example">{wordLookup.result.example}</span>
                                <button
                                  type="button"
                                  className="word-learn-btn"
                                  onClick={learnLookedUpWord}
                                  disabled={wordLookup.saving || wordLookup.saved}
                                >
                                  {wordLookup.saved ? "✓ 확인 중" : "학습하기"}
                                </button>
                              </>
                            )}
                          </div>
                        )}
                      </span>
                    ) : (
                      <span key={i}>{part}</span>
                    ),
                  )}
                </p>
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
                {(tts === "loading" || tts === "speaking") && (
                  <button type="button" className="ghost article-read-aloud-btn" onClick={cancelRead}>
                    취소
                  </button>
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
            <div className="article-reading-actions">
              <button type="button" className="ghost quiz-back-btn" onClick={backToList}>
                ← 목록으로
              </button>
              {draw.status === "done" && searchedWords.length > 0 && (
                <div className="article-searched-words-control">
                {searchedWordsOpen && (
                  <div className="article-searched-words-panel" role="dialog" aria-label="검색한 단어 목록">
                    <div className="word-lookup-header">
                      <strong>검색한 단어</strong>
                      <button type="button" className="ghost icon-btn" onClick={() => setSearchedWordsOpen(false)} aria-label="검색한 단어 목록 닫기">✕</button>
                    </div>
                    {searchedWords.map((item) => (
                      <div className="searched-word-row" key={item.key}>
                        <div className="searched-word-definition">
                          <strong>{item.word}</strong>
                          <span>{item.loading ? "뜻을 찾는 중…" : item.result?.meaning ?? "뜻을 가져오지 못했어요."}</span>
                        </div>
                        {item.result && (
                          <button type="button" className="word-learn-btn" onClick={() => learnSearchedWord(item)} disabled={item.saving || item.saved}>
                            {item.saved ? "✓ 확인 중" : item.saving ? "저장 중…" : "학습하기"}
                          </button>
                        )}
                      </div>
                    ))}
                  </div>
                )}
                <button type="button" className="article-searched-words-btn" onClick={() => setSearchedWordsOpen((open) => !open)} aria-expanded={searchedWordsOpen}>
                  🔎 검색한 단어 {searchedWords.length}
                </button>
                </div>
              )}
            </div>
          </div>
        )}

        {view === "quiz" && draw && (
          <div className="quiz-panel">
            <div className="article-language-label">영어 원문</div>
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
            <div className="article-language-label">영어 원문</div>
            <p className="article-summary">{draw.summary}</p>
            {(result.translation || draw.translation) && (
              <>
                <div className="article-language-label">한글 번역</div>
                <p className="article-translation" lang="ko">
                  {result.translation || draw.translation}
                </p>
              </>
            )}
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
