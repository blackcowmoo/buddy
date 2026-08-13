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
// how it's actually used there. Returns null on any failure, same reasoning
// as suggestWords.
type ArticleWordLookupResponse = {
  status: "pending" | "done" | "missing";
  result?: WordSuggestion;
};

const wordLookupPollIntervalMs = 1000;

// Checks the server-side article lookup cache without starting a new model
// job. This lets the reading UI distinguish an existing server result from a
// word that still needs the learner to request a lookup.
export async function checkDefinedWord(articleID: string, word: string, position: number): Promise<WordSuggestion | null> {
  const path = `api/articles/${encodeURIComponent(articleID)}/words/define`;
  const response = await postJSON<ArticleWordLookupResponse | null>(path, { word, position, checkOnly: true }, null);
  return response?.status === "done" ? response.result ?? null : null;
}

// Starts a durable article lookup and waits for its Redis-backed result while
// this page remains open. Leaving the page only stops the caller; the server's
// asyncjob continues and the next visit submits the same deterministic key,
// which is then an immediate cache hit.
export async function defineWord(articleID: string, word: string, position: number): Promise<WordSuggestion | null> {
  const path = `api/articles/${encodeURIComponent(articleID)}/words/define`;
  for (;;) {
    const response = await postJSON<ArticleWordLookupResponse | null>(path, { word, position }, null);
    if (!response) return null;
    if (response.status === "done") return response.result ?? null;
    await new Promise((resolve) => setTimeout(resolve, wordLookupPollIntervalMs));
  }
}
