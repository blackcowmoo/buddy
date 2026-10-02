import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { EmptyState, LearningIntro } from "../components/LearningIntro";
import { confirmThenDelete } from "../lib/confirmDelete";
import {
  answerArticle,
  deleteArticleInstance,
  drawArticle,
  fetchArticleInstance,
  fetchArticleInstances,
  type ArticleAnswerResult,
  type ArticleDraw,
  type ArticleInstance,
} from "../lib/articles";
import { formatAbsoluteDate, formatMessageTime } from "../lib/time";
import { QuizChoices } from "../components/QuizChoices";
import { LearningPage } from "../components/LearningPage";
import { BackButton, PageSection, PageToolbar } from "../components/PageLayout";
import { DatedList } from "../components/DatedList";
import { HistoryItem } from "../components/HistoryItem";
import { ArticleReadAloud } from "../components/ArticleReadAloud";
import { usePollScaffold } from "../hooks/usePollScaffold";
import { checkDefinedWordStatus, defineWord } from "../lib/wordSearch";
import { fetchWords, saveWord, type WordReviewItem, type WordReviewStatus } from "../lib/wordReview";
import type { WordSuggestion } from "../lib/protocol";
import { useDismiss } from "../hooks/useDismiss";
import { LoadingHint } from "../components/LoadingHint";
import type { LoadState } from "../lib/loadState";
import { readStored, writeStored } from "../lib/storedValue";
import { newestFirst } from "../lib/listView";

// How often to re-check a draw that's still generating in the background
// (see asyncjob.KindArticleStudy) — a poll, not a push, since nothing on the
// server tells an already-open client "it's ready now" (same reasoning as
// App.tsx's pollEndedField).
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
type TrackedWordStatus = WordReviewStatus | "saving";

type SearchedWord = {
  key: number;
  word: string;
  result: WordSuggestion | null;
  loading: boolean;
  definitionVersion?: number;
};

// Mirrors wordlookup's v2 namespace: preserve lookup history while refreshing
// definitions created before the dictionary-gloss prompt contract.
const definitionVersion = 2;

function trackedWordKey(word: Pick<WordReviewItem, "word" | "meaning"> | WordSuggestion) {
  return `${word.word.trim().replace(/\s+/g, " ").toLocaleLowerCase()}\u0000${word.meaning.trim().toLocaleLowerCase()}`;
}

function learnButtonLabel(status: TrackedWordStatus | undefined, checking: boolean) {
  if (status === "saving") return "저장 중…";
  if (checking) return "확인 중…";
  if (status === "pending") return "✓ 확인 중";
  if (status === "verified") return "✓ 학습 중";
  if (status === "rejected") return "✓ 제외됨";
  return "학습하기";
}

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
    }).map((item) => {
      const result = item.definitionVersion === definitionVersion ? item.result : null;
      return { ...item, result, loading: result === null };
    });
  }, []);
}

// "오늘의 아티클": draws a news article the learner hasn't seen before (see
// lib/articles.ts's drawArticle, which excludes every article already drawn
// — no daily limit, only repeats are excluded), shows an English study
// paragraph to read (with server-generated audio via ArticleReadAloud),
// then a native-language multiple-choice comprehension check.
// Past attempts live in their own list here, the same
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
  } | null>(null);
  const wordLookupRef = useRef<HTMLDivElement>(null);
  const wordLookupAnchorRef = useRef<HTMLSpanElement>(null);
  const [centerWordLookup, setCenterWordLookup] = useState(false);
  const [searchedWords, setSearchedWords] = useState<SearchedWord[]>([]);
  const [searchedWordsOpen, setSearchedWordsOpen] = useState(false);
  // The article's local search history only records what was looked up. The
  // vocabulary list is the durable source of truth for whether that exact
  // word+meaning is already tracked, including after a reload or on another
  // device. null means the initial check is still in flight, during which
  // learn buttons stay disabled so a fast revisit cannot submit twice.
  // Saving belongs to the same word+meaning entry, so the popover and history
  // cannot offer a duplicate save while either one is waiting for its response.
  const [trackedWords, setTrackedWords] = useState<Map<string, TrackedWordStatus> | null>(null);
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
  const resumedStoredLookupsRef = useRef(new Set<string>());
  useDismiss(wordLookup !== null, wordLookupRef, () => setWordLookup(null));

  const updateSearchedWord = useCallback((key: number, word: string, update: Partial<SearchedWord>) => {
    if (update.result) update = { ...update, definitionVersion };
    setSearchedWords((prev) => {
      const existing = prev.find((item) => item.key === key);
      const next = existing
        ? prev.map((item) => (item.key === key ? { ...item, ...update } : item))
        : [...prev, { key, word, result: null, loading: false, ...update }];
      if (draw) {
        writeStored(searchedWordsStorageKey(draw.id), JSON.stringify(next.map(({ key: itemKey, word: itemWord, result, definitionVersion }) => ({ key: itemKey, word: itemWord, result, definitionVersion }))));
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
  const { tokenRef: pollTokenRef, startPoll } = usePollScaffold();

  // Polls one draw's status until it leaves "pending"/"failed" — started
  // right after a fresh draw, or when reopening a still-generating row from
  // the list (see openInstance). "failed" keeps polling rather than giving
  // up: the asyncjob reaper retries the job from scratch on its own (see
  // asyncjob.Queue.Execute), so a later attempt can still land.
  const pollDraw = useCallback(
    (id: string, token: object) => startPoll(token, {
      intervalMs: articleStudyPollIntervalMs,
      fetchResult: () => fetchArticleInstance(id),
      onResult: (updated) => {
        if (!updated) return true;
        setDraw(updated);
        return updated.status !== "done" || !updated.translation;
      },
    }),
    [startPoll],
  );

  const loadInstances = useCallback(() => {
    fetchArticleInstances().then((list) => {
      setInstances(newestFirst(list, (item) => item.createdAt));
      setVisibleInstanceCount(articleListPageSize);
      setState("ready");
    });
  }, []);

  useEffect(() => {
    loadInstances();
  }, [loadInstances]);

  useEffect(() => {
    let active = true;
    void fetchWords().then((result) => {
      if (!active) return;
      setTrackedWords(new Map((result?.words ?? []).map((word) => [trackedWordKey(word), word.status])));
    });
    return () => {
      active = false;
    };
  }, []);

  const loadOlderInstances = useCallback(() => {
    if (visibleInstanceCount >= instances.length) return;
    setVisibleInstanceCount((count) => Math.min(count + articleListPageSize, instances.length));
  }, [instances.length, visibleInstanceCount]);

  const visibleInstances = instances.slice(0, visibleInstanceCount);

  const showDraw = useCallback((next: ArticleDraw) => {
    setDraw(next);
    setSelections([]);
    setResult(null);
    setView("reading");
    setDrawState("idle");
    setWordLookup(null);
    setSearchedWords(loadSearchedWords(next.id));
    setSearchedWordsOpen(false);
  }, []);

  const handleDraw = useCallback(async () => {
    setDrawState("drawing");
    const res = await drawArticle();
    if (res.status === "ok") {
      showDraw(res.draw);
      if (res.draw.status !== "done") {
        const token = {};
        pollTokenRef.current = token;
        pollDraw(res.draw.id, token);
      }
    } else {
      setDrawState(res.status);
    }
  }, [pollDraw, showDraw]);

  // Reopens one of the caller's own draws from the list, whether it's done
  // (read the past summary/quiz result) or still generating (resume
  // watching it finish instead of it looking abandoned).
  const openInstance = useCallback(
    async (id: string) => {
      const found = await fetchArticleInstance(id);
      if (!found) return;
      showDraw(found);
      if (found.status !== "done" || !found.translation) {
        const token = {};
        pollTokenRef.current = token;
        pollDraw(found.id, token);
      }
    },
    [pollDraw, showDraw],
  );

  const followWordLookup = useCallback((articleID: string, key: number, word: string) => {
    const lookupKey = `${articleID}:${key}`;
    const existing = pendingWordLookupsRef.current.get(lookupKey);
    if (existing) {
      setWordLookup((prev) => (prev && prev.key === key ? { ...prev, loading: true, failed: false } : prev));
      return;
    }

    setWordLookup((prev) => (prev && prev.key === key ? { ...prev, loading: true, failed: false } : prev));
    updateSearchedWord(key, word, { loading: true });
    const lookup = defineWord(articleID, word, key);
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
  }, [updateSearchedWord]);

  // A searched word with no stored result represents an interrupted lookup
  // from an earlier visit. Resume it as soon as the article is reopened;
  // Redis deduplication means this watches the existing job when it is still
  // running and safely starts a fresh retry only after that job is gone.
  useEffect(() => {
    if (!draw || draw.status !== "done") return;
    for (const item of searchedWords) {
      const lookupKey = `${draw.id}:${item.key}`;
      if (!item.result && !resumedStoredLookupsRef.current.has(lookupKey)) {
        resumedStoredLookupsRef.current.add(lookupKey);
        followWordLookup(draw.id, item.key, item.word);
      }
    }
  }, [draw, followWordLookup, searchedWords]);

  // Selects one word tapped inside the reading paragraph (see the word-token
  // buttons in the reading view below). The server cache and durable job
  // status are checked first; only a true miss leaves the learner a second
  // action to start a lookup.
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
      });

      if (localResult || pending || failedWordLookupsRef.current.has(lookupKey)) return;
      void checkDefinedWordStatus(draw.id, word, key).then((response) => {
        if (response?.status === "pending") {
          followWordLookup(draw.id, key, word);
          return;
        }
        const serverResult = response?.status === "done" ? response.result ?? null : null;
        if (serverResult) {
          wordLookupCacheRef.current.set(lookupKey, serverResult);
          // Keep the searched-words overlay in sync with the popover when
          // the server already has a cached definition. Without this, the
          // popover shows the meaning while the overlay still has a null
          // result and incorrectly renders the failure message.
          updateSearchedWord(key, word, { loading: false, result: serverResult });
        }
        setWordLookup((prev) =>
          prev && prev.key === key
            ? { ...prev, loading: false, result: serverResult, failed: false }
            : prev,
        );
      });
    },
    [draw, followWordLookup, searchedWords, updateSearchedWord],
  );

  // The whole study paragraph is short (one paragraph), so it's sent as
  // context every time rather than trying to isolate just the containing
  // sentence.
  const requestWordLookup = useCallback(() => {
    if (!draw || !wordLookup || wordLookup.loading) return;
    const { key, word } = wordLookup;
    followWordLookup(draw.id, key, word);
  }, [draw, followWordLookup, wordLookup]);

  const learnWord = useCallback((item: Pick<SearchedWord, "word" | "result">) => {
    if (!item.result || trackedWords === null) return;
    const key = trackedWordKey(item.result);
    if (trackedWords.has(key)) return;
    setTrackedWords((current) => new Map(current).set(key, "saving"));
    void saveWord(item.result, item.word).then((saved) => {
      setTrackedWords((current) => {
        const next = new Map(current);
        next.delete(key);
        if (saved) next.set(trackedWordKey(saved), saved.status);
        return next;
      });
    });
  }, [trackedWords]);

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
    setWordLookup(null);
    setSearchedWordsOpen(false);
    loadInstances();
  }, [loadInstances]);

  const handleDelete = (id: string) =>
    confirmThenDelete("이 아티클 퀴즈를 삭제할까요?", deleteArticleInstance, id, setInstances);

  const drawFeedback = drawState === "noMore" ? (
    <p className="hint" role="status">지금은 새로 볼 아티클이 없어요. 나중에 다시 시도해보세요.</p>
  ) : drawState === "error" ? (
    <p className="hint" role="alert">아티클을 가져오지 못했어요. 연결 상태를 확인한 뒤 ‘새 아티클 뽑기’를 다시 눌러 주세요.</p>
  ) : null;

  const learnWordButton = (item: Pick<SearchedWord, "word" | "result">) => {
    if (!item.result) return null;
    const status = trackedWords?.get(trackedWordKey(item.result));
    return (
      <button
        type="button"
        className="word-learn-btn"
        onClick={() => learnWord(item)}
        disabled={trackedWords === null || status !== undefined}
      >
        {learnButtonLabel(status, trackedWords === null)}
      </button>
    );
  };

  // Reading and answer review intentionally share this exact renderer. The
  // lookup state, per-position cache, and persisted searched-word history
  // therefore continue across the quiz instead of the result view becoming
  // a separate, non-interactive copy of the English passage.
  const searchableSummary = draw && (
    <div className="article-summary">
      {draw.summary.split(/([A-Za-z']+)/g).map((part, i) =>
        /^[A-Za-z']+$/.test(part) ? (
          <span className="article-word-anchor" key={i} ref={wordLookup?.key === i ? wordLookupAnchorRef : undefined}>
            <button
              type="button"
              className={wordLookup?.key === i ? "article-word selected" : "article-word"}
              onClick={() => openWordLookup(i, part)}
              aria-haspopup="menu"
              aria-expanded={wordLookup?.key === i}
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
                    {learnWordButton(wordLookup)}
                  </>
                )}
              </div>
            )}
          </span>
        ) : (
          <span key={i}>{part}</span>
        ),
      )}
    </div>
  );

  const searchedWordsControl = draw?.status === "done" && searchedWords.length > 0 && (
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
              {learnWordButton(item)}
            </div>
          ))}
        </div>
      )}
      <button type="button" className="article-searched-words-btn" onClick={() => setSearchedWordsOpen((open) => !open)} aria-expanded={searchedWordsOpen}>
        🔎 검색한 단어 {searchedWords.length}
      </button>
    </div>
  );

  return (
    <LearningPage
      title="오늘의 아티클"
      viewKey={view ?? "list"}
      onScroll={(event) => {
        const page = event.currentTarget;
        if (view === null && state === "ready" && page.scrollHeight - page.scrollTop - page.clientHeight <= articleListLoadThreshold) {
          loadOlderInstances();
        }
      }}
    >
      {view === null && (
        <>
          <LearningIntro eyebrow="읽으며 넓어지는 영어" title="새로운 이야기를 읽어 봐요" description="아티클 속 단어를 눌러 뜻을 알아보고, 퀴즈로 읽은 내용을 되짚어 보세요." steps={["아티클 고르기", "읽고 단어 찾기", "퀴즈로 확인하기"]} />
          <PageToolbar>
            <button type="button" className="quiz-start-btn" onClick={() => void handleDraw()} disabled={drawState === "drawing"}>
              {drawState === "drawing" ? "가져오는 중…" : "새 아티클 뽑기"}
            </button>
          </PageToolbar>
          {drawFeedback}
          <PageSection title="나의 아티클 기록" description={state === "ready" ? `총 ${instances.length}개 · 최근 기록부터` : undefined}>
            {state === "loading" && <LoadingHint />}
            {state === "ready" && instances.length === 0 && (
              <EmptyState title="아직 읽은 아티클이 없어요." description="‘새 아티클 뽑기’를 눌러 읽을거리를 만나 보세요. 읽던 글은 이 목록에서 다시 열 수 있어요." />
            )}
            <DatedList items={visibleInstances}>{(inst) => (
              <HistoryItem
                title={`[${inst.source}] ${inst.title}`}
                badges={inst.status !== "done" && (
                  <span className="study-summary-pending-badge" title="아티클을 만드는 중">
                    <span className="spinning">⏳</span> 생성 중
                  </span>
                )}
                meta={<>
                  {formatMessageTime(inst.createdAt)}
                  {inst.answered && (inst.correct ? " · 정답" : " · 오답")}
                </>}
                onOpen={() => void openInstance(inst.id)}
                onDelete={() => void handleDelete(inst.id)}
                deleteLabel="아티클 퀴즈 삭제"
              />
            )}</DatedList>

            {visibleInstances.length < instances.length && (
              <button type="button" className="ghost" onClick={loadOlderInstances}>
                이전 아티클 더 보기
              </button>
            )}
          </PageSection>
        </>
      )}

      {view !== null && <BackButton onClick={backToList} disabled={submitting || drawState === "drawing"} />}

      {view === "reading" && draw && (
        <PageSection className="page-card" title={`[${draw.source}] ${draw.title}`} description={draw.publishedAt > 0 ? formatAbsoluteDate(new Date(draw.publishedAt * 1000)) : undefined}>
          {draw.status === "done" ? (
            <>
              {/* This container includes the block-level lookup popover for
                  the selected token. A <p> cannot legally contain that
                  panel and browsers may re-parent it unpredictably. */}
              {searchableSummary}
              <ArticleReadAloud key={draw.id} articleId={draw.id} />
              <PageToolbar>
                <button type="button" className="quiz-start-btn" onClick={startQuiz}>문제풀기</button>
                {searchedWordsControl}
              </PageToolbar>
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
        </PageSection>
      )}

      {view === "quiz" && draw && (
        <PageSection title="읽은 내용 확인하기" className="page-card">
          <div className="language-label">영어 원문</div>
          <p className="article-summary">{draw.summary}</p>
          <div className="quiz-prompt">이 문단의 내용과 일치하는 것을 각각 고르세요.</div>
          {draw.subQuestions.map((sub, qi) => (
            <div key={qi} className="article-sub-question">
              <div className="quiz-prompt">{sub.prompt}</div>
              <QuizChoices
                options={sub.options}
                selectedIndex={selections[qi] ?? null}
                disabled={submitting}
                label={sub.prompt}
                onSelect={(index) => pickOption(qi, index)}
              />
            </div>
          ))}
          <PageToolbar>
            <button
              type="button"
              className="quiz-start-btn"
              onClick={() => void submitAnswers()}
              disabled={!allAnswered || submitting}
            >
              {submitting ? "채점 중…" : "답안 확인"}
            </button>
          </PageToolbar>
        </PageSection>
      )}

      {view === "result" && draw && result && (
        <PageSection title="아티클 학습 결과" className="page-card">
          <div className={`quiz-result ${result.correct ? "correct" : "incorrect"}`} role="status">
            {result.correct ? "정답이에요!" : `아쉬워요, ${result.score}/${result.total} 정답이에요.`}
          </div>
          <div className="language-label">영어 원문</div>
          {searchableSummary}
          {(result.translation || draw.translation) && (
            <>
              <div className="language-label">한글 번역</div>
              <p className="translation-quote" lang="ko">
                {result.translation || draw.translation}
              </p>
            </>
          )}
          {searchedWordsControl && (
            <div className="article-result-search-history">
              {searchedWordsControl}
            </div>
          )}
          {result.subQuestions.map((sub, qi) => (
            <div key={qi} className="article-sub-question">
              <div className="quiz-prompt">{sub.prompt}</div>
              <QuizChoices
                options={sub.options}
                selectedIndex={sub.selectedOptionIndex}
                correctIndex={sub.correctOptionIndex}
                label={sub.prompt}
              />
              <div className="article-explanation">{sub.explanation}</div>
            </div>
          ))}
          <PageToolbar>
            <button
              type="button"
              className="quiz-start-btn"
              onClick={() => void handleDraw()}
              disabled={drawState === "drawing"}
            >
              {drawState === "drawing" ? "가져오는 중…" : "다른 아티클 뽑기"}
            </button>
          </PageToolbar>
          {drawFeedback}
        </PageSection>
      )}
    </LearningPage>
  );
}
