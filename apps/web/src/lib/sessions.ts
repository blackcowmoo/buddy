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
}

export interface SessionDetail {
  session: SessionSummary;
  turns: TurnRecord[];
}

// Fetches the caller's own chat rooms, most recently active first. Relative
// URL: resolves against the current page, so this still hits the right
// server whether the app is mounted at "/" or under a ROOT_PATH prefix.
// Returns [] on any failure so the room list can render an empty state
// instead of throwing.
export async function fetchSessions(): Promise<SessionSummary[]> {
  try {
    const res = await fetch("api/sessions");
    if (!res.ok) return [];
    return (await res.json()) as SessionSummary[];
  } catch {
    return [];
  }
}

// Fetches one room's full transcript for replay. Returns null on any
// failure — network error, non-200 (including a 404 for someone else's
// session ID), or bad JSON.
export async function fetchSessionDetail(id: string): Promise<SessionDetail | null> {
  try {
    const res = await fetch(`api/sessions/${encodeURIComponent(id)}`);
    if (!res.ok) return null;
    return (await res.json()) as SessionDetail;
  } catch {
    return null;
  }
}
