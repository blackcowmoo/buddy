import { fetchJSON } from "./fetchJSON";
import type { Correction } from "./protocol";

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
  correction?: Correction;
  translation?: string;
}

export interface SessionDetail {
  session: SessionSummary;
  turns: TurnRecord[];
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

// Deletes one chat room and its transcript (the server also cascades to any
// recordings archived under it — see httpserver.sessionDeleteHandler).
// Returns whether the request succeeded, so the caller can decide what to do
// on failure (e.g. leave the room in the list) instead of assuming success.
export async function deleteSession(id: string): Promise<boolean> {
  try {
    const res = await fetch(`api/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });
    return res.ok;
  } catch {
    return false;
  }
}
