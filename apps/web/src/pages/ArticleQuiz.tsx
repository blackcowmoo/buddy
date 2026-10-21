import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { EmptyState, LearningIntro } from "../components/LearningIntro";
import { confirmThenDelete } from "../lib/confirmDelete";
import {
  answerArticle,
  deleteArticleInstance,
  drawArticle,
  fetchArticleInstance,
  fetchArticleInstances,
  fetchArticlePhrases,
  type ArticleAnswerResult,
  type ArticleDraw,
  type ArticleInstance,
} from "../lib/articles";
import {
  freeTextEntryKey,
  normalizePhraseQuery,
  phraseLookupPosition,
  segmentArticleSummary,
} from "../lib/phraseSearch";
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
  // Position sent to the define endpoint; the durable lookup cache is keyed
  // by (article, word, position). Taps use the token's split index as both
  // key and position; free-text searches get a negative key (see
  // phraseSearch.ts's freeTextEntryKey) with its own position, so a typed
  // query can never overwrite a tapped token's entry.
  position: number;
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
      // Entries stored before phrase search never carried a position; they
      // were all taps, whose position always equaled their key.
      const position = typeof item.position === "number" ? item.position : item.key;
      return { ...item, position, result, loading: result === null };
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
  // Phrase-search surface (search box + suggested-phrase chips below the
  // paragraph, reading view only). Its result renders inline here rather
  // than anchored to a word — a typed query has no single anchor token.
  // Same entry shape as the popover so the shared lookup caches and
  // searched-words history drive both surfaces with one set of plumbing.
  const [phraseSearch, setPhraseSearch] = useState<{
    key: number;
    text: string;
    loading: boolean;
    failed: boolean;
    result: WordSuggestion | null;
  } | null>(null);
  const [phraseQuery, setPhraseQuery] = useState("");
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

  const updateSearchedWord = useCallback((key: number, word: string, position: number, update: Partial<SearchedWord>) => {
    if (update.result) update = { ...update, definitionVersion };
    setSearchedWords((prev) => {
      const existing = prev.find((item) => item.key === key);
      const next = existing
        ? prev.map((item) => (item.key === key ? { ...item, ...update } : item))
        : [...prev, { key, word, position, result: null, loading: false, ...update }];
      if (draw) {
        writeStored(searchedWordsStorageKey(draw.id), JSON.stringify(next.map(({ key: itemKey, word: itemWord, position: itemPosition, result, definitionVersion }) => ({ key: itemKey, word: itemWord, position: itemPosition, result, definitionVersion }))));
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

  // A second, independent scaffold for the key-phrases watch below: the
  // study poll above shares one tokenRef per instance, so reusing it here
  // would make every study-poll restart invalidate the phrases watch too.
  const { tokenRef: phrasesTokenRef, startPoll: startPhrasesPoll } = usePollScaffold();
  // Server-suggested key phrases for the article being read. Drives both the
  // in-text phrase grouping (see phraseSearch.ts's segmentArticleSummary)
  // and the chip row, because learners can't always tell which runs of words
  // are idioms.
  const [phrases, setPhrases] = useState<string[]>([]);
  const phrasesWatchedRef = useRef<string | null>(null);

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

  // Once an article's own summary is ready, watch for its suggested key
  // phrases. The GET itself dispatches generation on a cache miss (see
  // lib/articles.ts's fetchArticlePhrases), and anything that isn't a real
  // phrase list reports "pending", so the poll simply keeps checking until
  // the durable job lands. Navigating away stops watching only.
  useEffect(() => {
    if (!draw || draw.status !== "done") return;
    if (phrasesWatchedRef.current === draw.id) return;
    phrasesWatchedRef.current = draw.id;
    const token = {};
    phrasesTokenRef.current = token;
    startPhrasesPoll(token, {
      intervalMs: articleStudyPollIntervalMs,
      fetchResult: () => fetchArticlePhrases(draw.id),
      onResult: (result) => {
        if (result.status === "done") {
          setPhrases(result.phrases);
          return false;
        }
        return true;
      },
    });
  }, [draw, phrasesTokenRef, startPhrasesPoll]);

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
    setPhrases([]);
    phrasesWatchedRef.current = null;
    setPhraseSearch(null);
    setPhraseQuery("");
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

  // Starts (or joins) one durable define job and mirrors its progress into
  // both lookup surfaces — the anchored popover and the inline phrase-search
  // panel — whichever currently shows this key. `position` is what the
  // server cache keys on together with the word text (taps pass the token's
  // split index; free-text queries pass phraseSearch.ts's
  // phraseLookupPosition), while `key` only identifies the UI entry.
  const followWordLookup = useCallback((articleID: string, key: number, word: string, position: number) => {
    const lookupKey = `${articleID}:${key}`;
    const existing = pendingWordLookupsRef.current.get(lookupKey);
    if (existing) {
      setWordLookup((prev) => (prev && prev.key === key ? { ...prev, loading: true, failed: false } : prev));
      setPhraseSearch((prev) => (prev && prev.key === key ? { ...prev, loading: true, failed: false } : prev));
      return;
    }

    setWordLookup((prev) => (prev && prev.key === key ? { ...prev, loading: true, failed: false } : prev));
    setPhraseSearch((prev) => (prev && prev.key === key ? { ...prev, loading: true, failed: false } : prev));
    updateSearchedWord(key, word, position, { loading: true });
    const lookup = defineWord(articleID, word, position);
    pendingWordLookupsRef.current.set(lookupKey, lookup);
    void lookup.then((result) => {
      pendingWordLookupsRef.current.delete(lookupKey);
      if (result) {
        wordLookupCacheRef.current.set(lookupKey, result);
        failedWordLookupsRef.current.delete(lookupKey);
      } else {
        failedWordLookupsRef.current.add(lookupKey);
      }
      updateSearchedWord(key, word, position, { loading: false, result });
      setWordLookup((prev) =>
        prev && prev.key === key ? { ...prev, loading: false, failed: result === null, result } : prev,
      );
      setPhraseSearch((prev) =>
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
        followWordLookup(draw.id, item.key, item.word, item.position);
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
      const localResult = wordLookupCacheRef.current.get(lookupKey)
        ?? searchedWords.find((item) => item.key === key && item.word === word)?.result
        ?? null;
      const pending = pendingWordLookupsRef.current.has(lookupKey);
      setWordLookup({
        key,
        word,
        loading: pending || (!localResult && !failedWordLookupsRef.current.has(lookupKey)),
        failed: !pending && !localResult && failedWordLookupsRef.current.has(lookupKey),
        result: localResult ?? null,
      });

      if (localResult || pending || failedWordLookupsRef.current.has(lookupKey)) return;
      // A tap's position is its split index, which is also its key.
      void checkDefinedWordStatus(draw.id, word, key).then((response) => {
        if (response?.status === "pending") {
          followWordLookup(draw.id, key, word, key);
          return;
        }
        const serverResult = response?.status === "done" ? response.result ?? null : null;
        if (serverResult) {
          wordLookupCacheRef.current.set(lookupKey, serverResult);
          // Keep the searched-words overlay in sync with the popover when
          // the server already has a cached definition. Without this, the
          // popover shows the meaning while the overlay still has a null
          // result and incorrectly renders the failure message.
          updateSearchedWord(key, word, key, { loading: false, result: serverResult });
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
    followWordLookup(draw.id, key, word, key);
  }, [draw, followWordLookup, wordLookup]);

  // Runs a phrase/idiom search typed into the reading view's search box (or
  // tapped as a suggested-phrase chip). The query may contain spaces — the
  // whole run of words is sent to the same durable define job the word taps
  // use; only the identity differs: a negative hashed key (never colliding
  // with a tap's split-index key) and phraseLookupPosition's position.
  // Submitting the search IS the explicit request to define, so unlike a
  // word tap there's no checkDefinedWordStatus round-trip and "찾기"
  // confirmation — a miss goes straight to the job.
  const runPhraseSearch = useCallback((raw: string) => {
    if (!draw) return;
    const query = normalizePhraseQuery(raw);
    if (!query) return;
    setWordLookup(null);
    setPhraseQuery("");
    const key = freeTextEntryKey(query);
    const lookupKey = `${draw.id}:${key}`;
    const cached = wordLookupCacheRef.current.get(lookupKey)
      ?? searchedWords.find((item) => item.key === key && item.word === query)?.result
      ?? null;
    if (cached) {
      setPhraseSearch({ key, text: query, loading: false, failed: false, result: cached });
      return;
    }
    // Re-submitting a failed query retries it (the learner may have fixed
    // spelling, or the earlier job may have died server-side).
    failedWordLookupsRef.current.delete(lookupKey);
    const pending = pendingWordLookupsRef.current.has(lookupKey);
    setPhraseSearch({ key, text: query, loading: pending, failed: false, result: null });
    if (pending) return;
    followWordLookup(draw.id, key, query, phraseLookupPosition(draw.summary, query));
  }, [draw, followWordLookup, searchedWords]);

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
    phrasesTokenRef.current = null; // ditto for the key-phrases watch
    phrasesWatchedRef.current = null;
    setView(null);
    setDraw(null);
    setResult(null);
    setSelections([]);
    setDrawState("idle");
    setWordLookup(null);
    setSearchedWordsOpen(false);
    setPhrases([]);
    setPhraseSearch(null);
    setPhraseQuery("");
    loadInstances();
  }, [loadInstances, phrasesTokenRef]);

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
  //
  // segmentArticleSummary keeps single words tappable exactly as before, but
  // groups server-suggested key phrases into ONE tappable unit each — the
  // learner can't be expected to know that "in spite of" is one expression
  // rather than three unrelated words. A phrase tap sends the whitespace-
  // collapsed, edge-punctuation-stripped substring (a phrase spanning text
  // like "talk, again" is looked up as "talk again"; the definition comes
  // from this paragraph's context either way).
  const searchableSummary = draw && (
    <div className="article-summary">
      {segmentArticleSummary(draw.summary, phrases).map((segment, i) =>
        segment.type === "gap" ? (
          <span key={i}>{segment.text}</span>
        ) : (
          <span
            className={segment.type === "phrase" ? "article-phrase-anchor" : "article-word-anchor"}
            key={i}
            ref={wordLookup?.key === segment.key ? wordLookupAnchorRef : undefined}
          >
            <button
              type="button"
              className={
                segment.type === "phrase"
                  ? wordLookup?.key === segment.key ? "article-word article-phrase selected" : "article-word article-phrase"
                  : wordLookup?.key === segment.key ? "article-word selected" : "article-word"
              }
              onClick={() => openWordLookup(segment.key, segment.type === "phrase" ? normalizePhraseQuery(segment.text) : segment.text)}
              aria-haspopup="menu"
              aria-expanded={wordLookup?.key === segment.key}
            >
              {segment.text}
            </button>
            {wordLookup?.key === segment.key && (
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
                    aria-label="표현 뜻 닫기"
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
        ),
      )}
    </div>
  );

  const searchedWordsControl = draw?.status === "done" && searchedWords.length > 0 && (
    <div className="article-searched-words-control">
      {searchedWordsOpen && (
        <div className="article-searched-words-panel" role="dialog" aria-label="검색한 표현 목록">
          <div className="word-lookup-header">
            <strong>검색한 표현</strong>
            <button type="button" className="ghost icon-btn" onClick={() => setSearchedWordsOpen(false)} aria-label="검색한 표현 목록 닫기">✕</button>
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
        🔎 검색한 표현 {searchedWords.length}
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
              {/* Phrase lookup for text the learner suspects is an idiom but
                  that isn't among the suggested phrases (or before they've
                  loaded). A free-text query may contain spaces — the whole
                  run of words goes to the same durable define job word taps
                  use. Results render inline instead of anchored: a typed
                  query has no single anchor token to hang a popover from. */}
              <div className="article-phrase-search">
                <form
                  className="article-phrase-form"
                  onSubmit={(event) => {
                    event.preventDefault();
                    runPhraseSearch(phraseQuery);
                  }}
                >
                  <input
                    type="text"
                    className="article-phrase-input"
                    value={phraseQuery}
                    onChange={(event) => setPhraseQuery(event.target.value)}
                    placeholder="표현 검색 (예: in spite of)"
                    aria-label="표현 검색"
                    maxLength={255}
                  />
                  <button type="submit" className="quiz-start-btn" disabled={!normalizePhraseQuery(phraseQuery)}>
                    검색
                  </button>
                </form>
                {phrases.length > 0 && (
                  <div className="article-phrase-chips" role="group" aria-label="이 아티클의 주요 표현">
                    <span className="article-phrase-chips-label">이 아티클의 주요 표현</span>
                    {phrases.map((phrase) => (
                      <button
                        type="button"
                        key={phrase}
                        className={`article-phrase-chip${phraseSearch?.text === normalizePhraseQuery(phrase) ? " active" : ""}`}
                        onClick={() => runPhraseSearch(phrase)}
                      >
                        {phrase}
                      </button>
                    ))}
                  </div>
                )}
                {phraseSearch && (
                  <div className="article-phrase-result">
                    <div className="word-lookup-header">
                      <span className="word-search-word">{phraseSearch.text}</span>
                      <button
                        type="button"
                        className="ghost icon-btn"
                        onClick={() => setPhraseSearch(null)}
                        aria-label="표현 검색 결과 닫기"
                      >
                        ✕
                      </button>
                    </div>
                    {phraseSearch.loading && <div className="word-search-status">찾는 중…</div>}
                    {!phraseSearch.loading && phraseSearch.failed && (
                      <div className="word-search-status">뜻을 가져오지 못했어요. 다시 검색해 보세요.</div>
                    )}
                    {!phraseSearch.loading && !phraseSearch.failed && phraseSearch.result && (
                      <>
                        <span className="word-search-meaning">{phraseSearch.result.meaning}</span>
                        <span className="word-search-example">{phraseSearch.result.example}</span>
                        {learnWordButton({ word: phraseSearch.text, result: phraseSearch.result })}
                      </>
                    )}
                  </div>
                )}
              </div>
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
