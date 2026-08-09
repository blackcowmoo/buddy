import type { WordSuggestion } from "./protocol";

// Asks the server (httpserver.wordSuggestHandler -> pipeline.SuggestWords)
// for English word/phrase candidates matching a native-language description
// of a word the learner can't recall. Returns null on any failure (network
// error, non-200, bad JSON) so the panel can show its own "couldn't load"
// message rather than silently rendering an empty result list.
export async function suggestWords(query: string): Promise<WordSuggestion[] | null> {
  try {
    const res = await fetch("api/words/suggest", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query }),
    });
    if (!res.ok) return null;
    const body = (await res.json()) as { suggestions: WordSuggestion[] };
    return body.suggestions;
  } catch {
    return null;
  }
}

// Asks the server (httpserver.wordDefineHandler -> pipeline.DefineWord) to
// define one English word/phrase the learner already knows exactly — tapped
// while reading (see ArticleQuiz.tsx's clickable article summary) — rather
// than described vaguely in their native language (that's suggestWords).
// context is the passage the word was tapped in, so the definition matches
// how it's actually used there. Returns null on any failure, same reasoning
// as suggestWords.
export async function defineWord(word: string, context: string): Promise<WordSuggestion | null> {
  try {
    const res = await fetch("api/words/define", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ word, context }),
    });
    if (!res.ok) return null;
    return (await res.json()) as WordSuggestion;
  } catch {
    return null;
  }
}
