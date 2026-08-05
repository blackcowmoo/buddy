import { fetchJSON, requestOK } from "./fetchJSON";
import type { WordSuggestion } from "./protocol";

// Mirrors httpserver's wordItem shape (apps/server/internal/httpserver/words.go).
// No "retired" field: a word is never removed from review rotation, just
// reviewed less and less often as nextReviewAt drifts further out (see
// wordreview.stageIntervals' doc server-side for why there's no ceiling).
export type WordReviewStatus = "pending" | "verified" | "rejected";

export interface WordReviewItem {
  id: string;
  word: string;
  meaning: string;
  example: string;
  stage: number;
  reviewCount: number;
  nextReviewAt: number; // unix seconds
  // pending: still being fact-checked in the background (see
  // wordSaveHandler's doc comment — never blocks the save response).
  // verified: passed the model-consensus check, in normal review rotation.
  // rejected: failed it — see verifyReason, and pages/WordReview.tsx's
  // "제외된 단어" section, where the learner reviews and deletes it.
  status: WordReviewStatus;
  verifyReason?: string;
}

// Adds one word-search suggestion the learner explicitly chose to study (the
// "학습하기" button in WordSearchControl) to their spaced-repetition study
// list — see httpserver.wordSaveHandler. Comes back "pending": a
// model-consensus check runs in the background and flips it to "verified" or
// "rejected" (see fetchWords) — this call itself never waits on that. A
// no-op server-side, returning the existing row, if this exact word+meaning
// pair is already tracked (the same word with a *different* meaning is a
// separate, independently tracked item). Returns null on any failure
// (network error, non-200, bad JSON) so the caller can leave the button in
// its un-saved state instead of assuming success.
export async function saveWord(s: WordSuggestion): Promise<WordReviewItem | null> {
  try {
    const res = await fetch("api/words/save", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ word: s.word, meaning: s.meaning, example: s.example }),
    });
    if (!res.ok) return null;
    return (await res.json()) as WordReviewItem;
  } catch {
    return null;
  }
}

// Mirrors httpserver's wordAutoAddStatus shape. status is "" (no run has
// ever started, treated as idle), "pending" (still generating in the
// background), "done", or "failed" — see store.JobStatusPending/Done/Failed
// server-side. count is only meaningful once status is "done": how many
// words that run actually added (0 means the model found nothing new).
export type WordAutoAddJobStatus = "" | "pending" | "done" | "failed";
export interface WordAutoAddStatus {
  status: WordAutoAddJobStatus;
  count: number;
}

// Starts (or, if one is already running, just reports) the "새 단어 추가로
// 학습하기" background job that generates and saves a batch of new words
// fit to the caller's learner profile — see httpserver.wordAutoAddHandler.
// Comes back the instant the job is durably marked pending — it never waits
// on the LLM generation, which keeps running server-side even if the caller
// navigates away. Poll fetchAutoAddStatus() until status leaves "pending" —
// that keeps working even after a reload, since generation itself never
// depended on this request staying open. Returns null on any failure
// (network error, non-200, bad JSON).
export async function startAutoAddWords(): Promise<WordAutoAddStatus | null> {
  try {
    const res = await fetch("api/words/auto-add", { method: "POST" });
    if (!res.ok) return null;
    return (await res.json()) as WordAutoAddStatus;
  } catch {
    return null;
  }
}

// Re-fetches the caller's own auto-add job status — the poll target for a
// run still generating in the background, whether the caller has been
// watching it the whole time or just reopened the word-review page while it
// was still going (see startAutoAddWords). Returns null on any failure.
export async function fetchAutoAddStatus(): Promise<WordAutoAddStatus | null> {
  return fetchJSON<WordAutoAddStatus | null>("api/words/auto-add", null);
}

// Fetches the caller's full study list plus how many of those words are due
// for review right now (see httpserver.wordsListHandler) — one call for both
// the word-review page's list and the menu badge's due count. Returns null on
// any failure, same fallback reasoning as lib/recordings.ts's fetchRecordings.
export async function fetchWords(): Promise<{ words: WordReviewItem[]; dueCount: number } | null> {
  return fetchJSON<{ words: WordReviewItem[]; dueCount: number } | null>("api/words", null);
}

// Records one review answer against a tracked word, returning its updated
// schedule (see httpserver.wordReviewHandler) so the caller can show when
// it's next due. repeat marks a correct-but-forced-guess answer (the "억지로
// 맞췄어요" button in WordReview.tsx) — the word gets rescheduled at the same
// interval it just came from instead of advancing; meaningless when correct
// is false. Returns null on any failure.
export async function reviewWord(id: string, correct: boolean, repeat = false): Promise<WordReviewItem | null> {
  try {
    const res = await fetch(`api/words/${encodeURIComponent(id)}/review`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ correct, repeat }),
    });
    if (!res.ok) return null;
    return (await res.json()) as WordReviewItem;
  } catch {
    return null;
  }
}

// Removes one tracked word from the caller's study list. Returns whether the
// request succeeded, same as lib/recordings.ts's deleteRecording.
export async function deleteWord(id: string): Promise<boolean> {
  return requestOK(`api/words/${encodeURIComponent(id)}`, { method: "DELETE" });
}
