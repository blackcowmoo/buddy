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
}

export interface SessionDetail {
  session: SessionSummary;
  turns: TurnRecord[];
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

// Fetches one room's full transcript for replay. Returns null on any
// failure — network error, non-200 (including a 404 for someone else's
// session ID), or bad JSON.
export async function fetchSessionDetail(id: string): Promise<SessionDetail | null> {
  return fetchJSON<SessionDetail | null>(`api/sessions/${encodeURIComponent(id)}`, null);
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

// Deletes one chat room and its transcript (the server also cascades to any
// recordings archived under it — see httpserver.sessionDeleteHandler).
// Returns whether the request succeeded, so the caller can decide what to do
// on failure (e.g. leave the room in the list) instead of assuming success.
export async function deleteSession(id: string): Promise<boolean> {
  return requestOK(`api/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });
}
