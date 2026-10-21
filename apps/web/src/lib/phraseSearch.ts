// Pure helpers behind phrase search on the reading page (see
// pages/ArticleQuiz.tsx). The server suggests key phrases per article
// (GET api/articles/{id}/phrases — transport.DispatchArticlePhrases on the
// Go side) and the article's define endpoint already accepts multi-word
// queries; this module is the missing client half: grouping the paragraph
// into tappable word/phrase units, and turning a free-text query into the
// (word, position) pair the durable word-lookup cache is keyed by.

export type SummarySegment =
  // Verbatim non-word run (spaces, punctuation) between tappable units.
  | { type: "gap"; text: string }
  // Single tappable word; `key` is the token's index within
  // splitSummaryTokens, which doubles as the define-request position —
  // the exact coordinate system the pre-existing word-tap lookup uses.
  | { type: "word"; text: string; key: number }
  // Multi-word expression suggested by the server, rendered as ONE
  // tappable unit (the learner can't be expected to know which runs of
  // words are idioms). `text` is the exact summary substring including any
  // punctuation between the words; `key` is the first word's token index.
  | { type: "phrase"; text: string; key: number };

// Same capture split as ArticleQuiz's original word tokenizer, so lookup
// positions sent to the server stay identical across the whole feature.
export function splitSummaryTokens(summary: string): string[] {
  return summary.split(/([A-Za-z']+)/g);
}

const WORD_TOKEN = /^[A-Za-z']+$/;

// Collapses internal whitespace and strips edge punctuation ("in spite  of"
// and "in spite of," both mean the same expression; models and typers
// disagree on both). Case is preserved — it's part of what the learner
// sees and looks up, same as word taps keep the text's own capitalization.
export function normalizePhraseQuery(query: string): string {
  return query
    .trim()
    .replace(/\s+/g, " ")
    .replace(/^[^\p{L}\p{N}]+|[^\p{L}\p{N}]+$/gu, "");
}

function phraseWords(phrase: string): string[] {
  const normalized = normalizePhraseQuery(phrase);
  return normalized === "" ? [] : normalized.split(" ");
}

// Deterministic FNV-1a over the normalized text: the same query must map
// to the same server cache position across visits, or every re-search of
// an un-cached phrase would start a fresh LLM definition job.
function hashNormalized(text: string): number {
  let hash = 0x811c9dc5;
  for (let i = 0; i < text.length; i += 1) {
    hash ^= text.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193);
  }
  return hash >>> 0;
}

// Server-side guard on the position field (httpserver.maxWordLookupPosition).
const maxWordLookupPosition = 100000;

// True when word slot `start` of wordIndexes begins exactly the word
// sequence `words` (case-insensitively; words between them may be
// separated by any punctuation the paragraph puts there).
function matchesWords(tokens: string[], wordIndexes: number[], start: number, words: string[]): boolean {
  if (start + words.length > wordIndexes.length) return false;
  for (let i = 0; i < words.length; i += 1) {
    if (tokens[wordIndexes[start + i]].toLocaleLowerCase() !== words[i].toLocaleLowerCase()) return false;
  }
  return true;
}

function collectWordIndexes(tokens: string[]): number[] {
  const indexes: number[] = [];
  tokens.forEach((token, index) => {
    if (WORD_TOKEN.test(token)) indexes.push(index);
  });
  return indexes;
}

// Index suggested phrases by their first lowercased word, longest first,
// so at each position the most specific expression wins ("agreed to talk
// again" over "talk again"). Phrases whose words can't correspond to text
// tokens (e.g. a hyphenated "state-of-the-art", which the word tokenizer
// splits into pieces) simply never match; the server guarantees suggested
// phrases occur verbatim in the paragraph, so misses are cosmetic at worst
// — the chip row still offers them and words stay individually tappable.
function indexPhrases(phrases: string[]): Map<string, string[][]> {
  const byFirstWord = new Map<string, string[][]>();
  for (const phrase of phrases) {
    const words = phraseWords(phrase);
    if (words.length < 2) continue; // single "phrases" are plain word taps
    const first = words[0].toLocaleLowerCase();
    const list = byFirstWord.get(first) ?? [];
    list.push(words);
    byFirstWord.set(first, list);
  }
  for (const list of byFirstWord.values()) {
    list.sort((a, b) => b.length - a.length);
  }
  return byFirstWord;
}

// Segments the summary into gaps / word buttons / phrase buttons whose
// texts concatenate back to the summary EXACTLY (tests assert it — the
// paragraph must never gain or lose a character from tokenizing).
export function segmentArticleSummary(summary: string, phrases: string[]): SummarySegment[] {
  const tokens = splitSummaryTokens(summary);
  const wordIndexes = collectWordIndexes(tokens);
  const byFirstWord = indexPhrases(phrases);

  // Pass 1: greedy left-to-right, non-overlapping phrase matches.
  const phraseEndByStartToken = new Map<number, number>();
  let consumedWordSlot = 0;
  for (let slot = 0; slot < wordIndexes.length; slot += 1) {
    if (slot < consumedWordSlot) continue;
    const candidates = byFirstWord.get(tokens[wordIndexes[slot]].toLocaleLowerCase());
    if (!candidates) continue;
    for (const words of candidates) {
      if (matchesWords(tokens, wordIndexes, slot, words)) {
        phraseEndByStartToken.set(wordIndexes[slot], wordIndexes[slot + words.length - 1]);
        consumedWordSlot = slot + words.length;
        break;
      }
    }
  }

  // Pass 2: emit segments; a phrase swallows the raw tokens (words and the
  // punctuation between them) from its first word to its last.
  const segments: SummarySegment[] = [];
  for (let i = 0; i < tokens.length; i += 1) {
    const phraseEnd = phraseEndByStartToken.get(i);
    if (phraseEnd !== undefined) {
      segments.push({ type: "phrase", text: tokens.slice(i, phraseEnd + 1).join(""), key: i });
      i = phraseEnd;
    } else if (WORD_TOKEN.test(tokens[i])) {
      segments.push({ type: "word", text: tokens[i], key: i });
    } else if (tokens[i] !== "") {
      segments.push({ type: "gap", text: tokens[i] });
    }
  }
  return segments;
}

// Token-index start of `query`'s word sequence in the summary, or -1.
function findPhraseStart(summary: string, words: string[]): number {
  if (words.length === 0) return -1;
  const tokens = splitSummaryTokens(summary);
  const wordIndexes = collectWordIndexes(tokens);
  for (let slot = 0; slot < wordIndexes.length; slot += 1) {
    if (matchesWords(tokens, wordIndexes, slot, words)) return wordIndexes[slot];
  }
  return -1;
}

// Define-request position for a free-text/chip query. When the text occurs
// in the paragraph, its own token index is used so a typed search and a
// tap on the in-text phrase hit the SAME durable cache entry (one LLM
// definition serves both). Otherwise a stable hash inside the server's
// accepted range keeps re-searching the same phrase a cache hit across
// visits too.
export function phraseLookupPosition(summary: string, query: string): number {
  const words = phraseWords(query);
  const found = findPhraseStart(summary, words);
  if (found >= 0) return found;
  return hashNormalized(words.join(" ").toLocaleLowerCase()) % (maxWordLookupPosition + 1);
}

// Identity for search-box/chip history entries. Negative so it can never
// collide with tap lookups, whose keys are token indexes (>= 0), inside
// the shared searched-words history and lookup caches.
export function freeTextEntryKey(query: string): number {
  return -(hashNormalized(phraseWords(query).join(" ").toLocaleLowerCase()) % 1_000_000_000) - 1;
}
