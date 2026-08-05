import { fetchJSON, requestOK } from "./fetchJSON";

// Mirrors newsarticle.Article.Status's three values — "pending" while
// asyncjob.KindArticleStudy is still generating the summary/quiz in the
// background, "done" once it's ready, "failed" after an attempt errored
// (the asyncjob reaper still retries it from scratch regardless, so this is
// shown the same as "pending", not a dead end).
export type ArticleStatus = "pending" | "done" | "failed";

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
  publishedAt: number; // unix seconds; 0 if the source feed had no usable pubDate
  status: ArticleStatus;
}

// Mirrors httpserver's articleDraw shape — enough to render the reading
// view and, once the learner asks to see it, the quiz's Choices. Never
// carries CorrectIndex/Explanation; the server only reveals those via
// answerArticle, after the learner has actually picked one. Summary/choices
// are empty while status is "pending"/"failed" — see fetchArticleInstance,
// which polls this same shape until generation lands.
export interface ArticleDraw {
  id: string;
  source: string;
  title: string;
  summary: string;
  choices: string[];
  // unix seconds; 0 if the source feed had no usable pubDate. Present from
  // the very first draw response, even while status is still "pending".
  publishedAt: number;
  status: ArticleStatus;
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

// Draws a fresh, never-before-seen (by this learner) news article. Returns
// the instant the server reserves it — status "pending" means its English
// summary + quiz are still generating in the background (see
// newsarticle.Store.ReserveArticle's URL-keyed cache and
// asyncjob.KindArticleStudy), independent of this request; "done" means an
// already-cached story was reused, near-instant. Either way, poll
// fetchArticleInstance(draw.id) until status leaves "pending" — that keeps
// working even if the learner navigated away and came back, since the
// generation itself never depended on this request staying open. No daily
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

// Re-fetches one of the caller's own draws by id — the poll target for a
// draw whose study content was still generating (status "pending") when the
// learner last saw it, whether they've been watching it the whole time or
// just navigated back to a still-pending row in fetchArticleInstances' list.
// Returns null on any failure (network error, non-200, bad JSON) so a poll
// tick can just skip a beat and retry rather than tearing down the view.
export async function fetchArticleInstance(id: string): Promise<ArticleDraw | null> {
  try {
    const res = await fetch(`api/articles/${encodeURIComponent(id)}`);
    if (!res.ok) return null;
    return (await res.json()) as ArticleDraw;
  } catch {
    return null;
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
