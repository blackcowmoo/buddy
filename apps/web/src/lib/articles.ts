import { fetchJSON, requestOK } from "./fetchJSON";

// Mirrors httpserver's articleListItem shape (apps/server/internal/httpserver/articles.go).
// Never carries the quiz's Choices/CorrectIndex/Explanation — the list only
// shows what a learner already saw plus their own outcome.
export interface ArticleInstance {
  id: string;
  source: string;
  title: string;
  summary: string;
  answered: boolean;
  correct: boolean;
  createdAt: number; // unix seconds
}

// Mirrors httpserver's articleDraw shape — enough to render the reading
// view and, once the learner asks to see it, the quiz's Choices. Never
// carries CorrectIndex/Explanation; the server only reveals those via
// answerArticle, after the learner has actually picked one.
export interface ArticleDraw {
  id: string;
  source: string;
  title: string;
  summary: string;
  choices: string[];
}

// Mirrors httpserver's articleResult shape — the reveal shown right after
// answering.
export interface ArticleAnswerResult {
  correct: boolean;
  correctIndex: number;
  translation: string; // the accurate choice's own text
  explanation: string;
}

export type ArticleDrawResult =
  | { status: "ok"; draw: ArticleDraw }
  // Every candidate from today's feeds has already been drawn by this
  // learner (see httpserver.articleDrawHandler's 204 response) — distinct
  // from a real failure so the UI can say "다시 시도해보세요" only for the
  // latter.
  | { status: "noMore" }
  | { status: "error" };

// Draws a fresh, never-before-seen (by this learner) news article,
// generating its English summary + quiz on the server if this is the first
// time anyone has drawn this exact story (see
// newsarticle.Store.SaveArticle's cache) — otherwise near-instant. No daily
// limit: only already-drawn articles are ever excluded.
export async function drawArticle(): Promise<ArticleDrawResult> {
  try {
    const res = await fetch("api/articles/draw", { method: "POST" });
    if (res.status === 204) return { status: "noMore" };
    if (!res.ok) return { status: "error" };
    return { status: "ok", draw: (await res.json()) as ArticleDraw };
  } catch {
    return { status: "error" };
  }
}

// Fetches the caller's own past article-quiz attempts, most recently drawn
// first — this feature's own dedicated list, the same "instant, unlimited,
// own list" shape as fetchInstantSessions. Returns [] on any failure so the
// list can render an empty state instead of throwing.
export async function fetchArticleInstances(): Promise<ArticleInstance[]> {
  return fetchJSON<ArticleInstance[]>("api/articles", []);
}

// Records the caller's choice for one of their own article-quiz instances
// and returns the reveal — correctness is always computed server-side
// against the stored answer key, never trusting a client-supplied verdict.
// Returns null on any failure (network error, non-200, bad JSON).
export async function answerArticle(id: string, selectedIndex: number): Promise<ArticleAnswerResult | null> {
  try {
    const res = await fetch(`api/articles/${encodeURIComponent(id)}/answer`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ selectedIndex }),
    });
    if (!res.ok) return null;
    return (await res.json()) as ArticleAnswerResult;
  } catch {
    return null;
  }
}

// Deletes one of the caller's own article-quiz instances. Returns whether
// the request succeeded, same as lib/sessions.ts's deleteSession.
export async function deleteArticleInstance(id: string): Promise<boolean> {
  return requestOK(`api/articles/${encodeURIComponent(id)}`, { method: "DELETE" });
}
