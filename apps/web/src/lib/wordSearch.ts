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
