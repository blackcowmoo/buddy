import { fetchJSON, requestOK } from "./fetchJSON";
import type { Correction, InputSource } from "./protocol";

// Mirrors store.SessionMeta / store.Turn (apps/server/internal/store/store.go).
export interface SessionSummary {
  id: string;
  title: string;
  createdAt: number;
  updatedAt: number;
}

export interface TurnRecord {
  turn: number;
  role: "user" | "assistant";
  text: string;
  refined: boolean;
  // How the learner produced this turn — absent for assistant turns.
  source?: InputSource;
  correction?: Correction;
  translation?: string;
  // Unix seconds this turn was first saved. Optional only so older test
  // fixtures / hand-built records don't need updating — real API responses
  // always set it (see store.Turn.CreatedAt).
  createdAt?: number;
  // This turn's durable reply-job status ("pending" | "processing" | "done"
  // | "failed"), set only for an assistant turn whose reply was generated
  // via the async job queue (see store.Turn.ReplyStatus). Absent for a user
  // turn, and for an assistant turn that predates this feature or was never
  // queued — both mean "nothing to poll for."
  replyStatus?: "pending" | "processing" | "done" | "failed";
  // Mirrors replyStatus for the grammar-correction job on a user turn (see
  // store.Turn.CorrectionStatus). "failed" is what lets a reload tell "the
  // analysis errored" apart from "it ran and found nothing" (correction
  // present, empty issues) — both look the same in `correction` alone.
  correctionStatus?: "pending" | "processing" | "done" | "failed";
}

export interface SessionDetail {
  session: SessionSummary;
  turns: TurnRecord[];
  // Whether older turns exist beyond this page (see sessionDetailHandler's
  // ?before=/?limit= query params) — the cue to fetch another page when the
  // learner scrolls to the top of the transcript instead of assuming
  // `turns` is the whole history.
  hasMore: boolean;
}

// Mirrors the JSON shape written by httpserver.sessionCompactionHandler.
export interface SessionCompaction {
  summary: string;
  recentMessages: number;
  totalTurns: number;
}

// Fetches the caller's own chat rooms, most recently active first. Returns
// [] on any failure so the room list can render an empty state instead of
// throwing.
export async function fetchSessions(): Promise<SessionSummary[]> {
  return fetchJSON<SessionSummary[]>("api/sessions", []);
}

// Fetches one page of a room's transcript for replay, most recent turns
// first — opts.before (a turn-number cursor, for fetching the page older
// than one already loaded) and opts.limit (page size) mirror
// sessionDetailHandler's query params. Omitting opts.limit gets the
// server's default page (defaultSessionPageLimit); opts.limit: 0
// deliberately asks for the *whole* transcript instead (see
// pollMissingFeedback, which needs every turn, not just the latest page) —
// so limit/before are only left off the query string when actually
// undefined, not just falsy. Returns null on any failure — network error,
// non-200 (including a 404 for someone else's session ID), or bad JSON.
export async function fetchSessionDetail(
  id: string,
  opts?: { before?: number; limit?: number },
): Promise<SessionDetail | null> {
  const params = new URLSearchParams();
  if (opts?.before !== undefined) params.set("before", String(opts.before));
  if (opts?.limit !== undefined) params.set("limit", String(opts.limit));
  const query = params.toString();
  return fetchJSON<SessionDetail | null>(
    `api/sessions/${encodeURIComponent(id)}${query ? `?${query}` : ""}`,
    null,
  );
}

// Fetches how much of a room's conversation is still sent to the LLM
// verbatim vs. folded into its rolling summary (see internal/session's
// compaction — nothing here is ever dropped from the transcript itself,
// only from the copy sent to the model). Returns null on any failure.
export async function fetchSessionCompaction(id: string): Promise<SessionCompaction | null> {
  return fetchJSON<SessionCompaction | null>(
    `api/sessions/${encodeURIComponent(id)}/compaction`,
    null,
  );
}

// Mirrors the JSON shape written by httpserver.sessionStudySummaryHandler.
export interface StudySummary {
  // Empty when issueCount is 0 — the server skips the LLM call entirely
  // rather than being asked to summarize nothing (see EndConversationControl,
  // which shows its own canned message for that case).
  summary: string;
  issueCount: number;
}

// Fetches a synthesized "what to study next" wrap-up from every
// grammar/vocabulary issue flagged so far in a room — meant to be called
// once, when the learner explicitly ends the conversation, not polled like
// fetchSessionCompaction. Returns null on any failure.
export async function fetchStudySummary(id: string): Promise<StudySummary | null> {
  return fetchJSON<StudySummary | null>(
    `api/sessions/${encodeURIComponent(id)}/study-summary`,
    null,
  );
}

// Deletes one chat room and its transcript (the server also cascades to any
// recordings archived under it — see httpserver.sessionDeleteHandler).
// Returns whether the request succeeded, so the caller can decide what to do
// on failure (e.g. leave the room in the list) instead of assuming success.
export async function deleteSession(id: string): Promise<boolean> {
  return requestOK(`api/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });
}
