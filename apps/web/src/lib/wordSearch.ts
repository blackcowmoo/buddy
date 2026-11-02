import { postJSON } from "./fetchJSON";
import type { WordSuggestion } from "./protocol";

// Asks the server (httpserver.wordSuggestHandler -> pipeline.SuggestWords)
// for English word/phrase candidates matching a native-language description
// of a word the learner can't recall. Returns null on any failure (network
// error, non-200, bad JSON) so the panel can show its own "couldn't load"
// message rather than silently rendering an empty result list.
export async function suggestWords(query: string): Promise<WordSuggestion[] | null> {
  const body = await postJSON<{ suggestions: WordSuggestion[] } | null>("api/words/suggest", { query }, null);
  return body?.suggestions ?? null;
}

// Asks the server (httpserver.wordDefineHandler -> pipeline.DefineWord) to
// define one English word/phrase the learner already knows exactly — tapped
// while reading (see ArticleQuiz.tsx's clickable article summary) — rather
// than described vaguely in their native language (that's suggestWords).
// context is the passage the word was tapped in, so the definition matches
// how it's actually used there.
type ArticleWordLookupResponse = {
  status: "pending" | "done" | "missing";
  result?: WordSuggestion;
};

const wordLookupPollIntervalMs = 1000;

// Gives up after this many consecutive failed hint poll responses. Unlike a
// word search, a hint is auto-fired by a mere phrase tap, so a broken hint
// path must not leave the page polling forever while the popover sits open.
const phraseHintMaxTransientErrors = 15;

// Returns the durable server-side state as well as a completed result. The
// reading page uses "pending" to resume watching a lookup after navigation
// or a deployment instead of mistaking an in-flight Redis job for a miss.
export async function checkDefinedWordStatus(articleID: string, word: string, position: number): Promise<ArticleWordLookupResponse | null> {
  const path = `api/articles/${encodeURIComponent(articleID)}/words/define`;
  return postJSON<ArticleWordLookupResponse | null>(path, { word, position, checkOnly: true }, null);
}

// Starts a durable article lookup and waits for its Redis-backed result while
// this page remains open. Leaving the page only stops the caller; the server's
// asyncjob continues and the next visit submits the same deterministic key,
// which is then an immediate cache hit.
export async function defineWord(articleID: string, word: string, position: number): Promise<WordSuggestion | null> {
  const path = `api/articles/${encodeURIComponent(articleID)}/words/define`;
  for (;;) {
    const response = await postJSON<ArticleWordLookupResponse | null>(path, { word, position }, null);
    if (response?.status === "done") return response.result ?? null;
    // A transient browser/network error is not a lookup failure. The Redis
    // job continues independently, so keep polling; once a failed server
    // attempt exhausts its retries, this same POST durably enqueues a fresh
    // attempt instead of freezing the UI in a permanent failure state.
    await new Promise((resolve) => setTimeout(resolve, wordLookupPollIntervalMs));
  }
}

type PhraseHintResponse = {
  status: "pending" | "done";
  hint?: string;
};

// Starts (or joins) the durable hint job behind httpserver's
// articlePhraseHintHandler and waits while the popover stays open. The hint
// only conveys a general sense of the tapped phrase and never the
// memorizable meaning — the full answer still requires an actual word
// search (defineWord). Returns null when the hint path stays unreachable,
// so the popover can just carry on without one.
export async function phraseHint(articleID: string, phrase: string, position: number): Promise<string | null> {
  const path = `api/articles/${encodeURIComponent(articleID)}/phrases/hint`;
  let transientErrors = 0;
  for (;;) {
    const response = await postJSON<PhraseHintResponse | null>(path, { word: phrase, position }, null);
    if (response?.status === "done") return response.hint ?? null;
    if (!response) {
      transientErrors += 1;
      if (transientErrors >= phraseHintMaxTransientErrors) return null;
    }
    await new Promise((resolve) => setTimeout(resolve, wordLookupPollIntervalMs));
  }
}
